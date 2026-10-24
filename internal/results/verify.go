// SPDX-License-Identifier: GPL-3.0-or-later

package results

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
)

// 离线上传包的检查步骤写在 docs/schema/README.md 里，网站收包时按同样的步骤检查。
// 问题代码（Problem.Code）和位置（Problem.Path）是和网站约定的；docs/schema/examples/packages/
// 里的样例包和 expected.json 是两边共用的测试，改这里的规则时要同步改文档和样例。

// 问题代码。
const (
	CodePackageInvalid    = "package_invalid"    // package.json 不是合法的 JSON，或缺字段、类型不对
	CodeSchemaUnsupported = "schema_unsupported" // package.json 或 manifest.json 是更新的大版本
	CodePathInvalid       = "path_invalid"       // 文件夹名或文件名不安全，例如含 / \ : 或是 ..
	CodeDuplicatePath     = "duplicate_path"     // 同名的设备文件夹或文件，或 zip 里有同名条目
	CodeFileMissing       = "file_missing"       // package.json 列出的文件不在包里
	CodeSizeMismatch      = "size_mismatch"      // 文件大小和记录的不一致
	CodeChecksumMismatch  = "checksum_mismatch"  // SHA-256 和记录的不一致，或文件已损坏
	CodeManifestMissing   = "manifest_missing"   // 设备文件夹里没有 manifest.json
	CodeMainMissing       = "main_missing"       // 设备文件夹里没有主日志文件
	CodeManifestInvalid   = "manifest_invalid"   // manifest.json 不是合法的 JSON，或缺字段、类型不对
	CodeManifestMismatch  = "manifest_mismatch"  // manifest.json 和 package.json 对不上
)

var (
	// ErrNotZip 表示文件不是完整的 zip。
	ErrNotZip = errors.New("不是完整的 zip 文件")
	// ErrNotPackage 表示 zip 里找不到 package.json，不是离线上传包；网站照旧按普通日志处理。
	ErrNotPackage = errors.New("不是 LogDiag 离线上传包（找不到 package.json）")
)

// Problem 是检查发现的一个问题。
type Problem struct {
	Code    string `json:"code"`
	Path    string `json:"path"` // 包里的位置，相对 package.json 所在的文件夹，例如 设备文件夹/文件名
	Message string `json:"message"`
}

// Verification 是离线上传包的检查结果。
type Verification struct {
	// Prefix 是 package.json 所在的文件夹。用户解压后重新压缩的包会多一层文件夹，平时为空。
	Prefix   string    `json:"prefix"`
	Package  *Package  `json:"package,omitempty"` // package.json 能读出来时才有
	Problems []Problem `json:"problems"`
}

// OK 报告包里没有发现问题。
func (v *Verification) OK() bool { return len(v.Problems) == 0 }

const (
	maxPackageJSON  = 16 << 20
	maxManifestJSON = 1 << 20
)

var (
	uuidRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	timeRE     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	logTypeRE  = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)+$`)
	schemaRE   = regexp.MustCompile(`^logdiag\.(package|manifest)/[0-9]+$`)
	deviceType = []string{"server", "storage", "network", "os"}
)

// VerifyPackage 检查 path 处的离线上传包：结构、每个文件的大小和 SHA-256、manifest 和 package.json
// 是否一致。文件不是 zip 时返回 ErrNotZip，找不到 package.json 时返回 ErrNotPackage；
// 包里的问题一次列全，记在 Problems 里。
func VerifyPackage(path string) (*Verification, error) {
	z, err := zip.OpenReader(path)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) { // 不安全的条目名不影响检查：这里只按 package.json 读文件，不解压
		return nil, fmt.Errorf("%w：%v", ErrNotZip, err)
	}
	defer z.Close()
	return verify(&z.Reader)
}

func verify(z *zip.Reader) (*Verification, error) {
	entries := map[string][]*zip.File{}
	for _, f := range z.File {
		entries[f.Name] = append(entries[f.Name], f)
	}
	prefix, ok := findPrefix(z.File, entries)
	if !ok {
		return nil, ErrNotPackage
	}
	v := &Verification{Prefix: prefix, Problems: []Problem{}}
	add := func(code, path, format string, args ...any) {
		v.Problems = append(v.Problems, Problem{Code: code, Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if len(entries[prefix+"package.json"]) > 1 {
		add(CodePackageInvalid, "package.json", "包里有不止一个 package.json")
		return v, nil
	}
	data, err := readZipEntry(entries[prefix+"package.json"][0], maxPackageJSON)
	if err != nil {
		add(CodePackageInvalid, "package.json", "package.json 无法读取：%v", err)
		return v, nil
	}
	pkg, code, msg := parsePackage(data)
	if code != "" {
		add(code, "package.json", "%s", msg)
		return v, nil
	}
	v.Package = pkg

	seenDir := map[string]bool{}
	for _, d := range pkg.Devices {
		if !safeEntryName(d.Dir) {
			add(CodePathInvalid, d.Dir, "设备文件夹名 %q 不安全", d.Dir)
			continue
		}
		if seenDir[d.Dir] {
			add(CodeDuplicatePath, d.Dir, "设备文件夹 %s 列出了不止一次", d.Dir)
			continue
		}
		seenDir[d.Dir] = true

		seenName := map[string]bool{}
		intact := map[string]*zip.File{} // 大小和 SHA-256 都对的文件
		var manifests []collect.File
		mains := 0
		for _, f := range d.Files {
			p := d.Dir + "/" + f.Name
			switch f.Role {
			case "manifest":
				manifests = append(manifests, f)
			case "main":
				mains++
			}
			if !safeEntryName(f.Name) {
				add(CodePathInvalid, p, "文件名 %q 不安全", f.Name)
				continue
			}
			if seenName[f.Name] {
				add(CodeDuplicatePath, p, "%s 列出了不止一次", p)
				continue
			}
			seenName[f.Name] = true
			found := entries[prefix+p]
			if len(found) > 1 {
				add(CodeDuplicatePath, p, "包里有不止一个 %s", p)
				continue
			}
			if len(found) == 0 || found[0].FileInfo().IsDir() {
				add(CodeFileMissing, p, "包里没有 %s", p)
				continue
			}
			if found[0].UncompressedSize64 != uint64(f.Size) {
				add(CodeSizeMismatch, p, "%s 的大小是 %d 字节，记录的是 %d 字节", p, found[0].UncompressedSize64, f.Size)
				continue
			}
			sum, err := hashEntry(found[0])
			if err != nil {
				add(CodeChecksumMismatch, p, "%s 已损坏：%v", p, err)
				continue
			}
			if sum != f.SHA256 {
				add(CodeChecksumMismatch, p, "%s 的内容和记录的 SHA-256 不一致，可能在拷贝时损坏或被修改过", p)
				continue
			}
			intact[f.Name] = found[0]
		}
		if len(manifests) != 1 || manifests[0].Name != "manifest.json" {
			add(CodeManifestMissing, d.Dir, "设备文件夹 %s 里应有且只有一个 manifest.json", d.Dir)
		}
		if mains == 0 {
			add(CodeMainMissing, d.Dir, "设备文件夹 %s 里没有主日志文件", d.Dir)
		}
		if len(manifests) != 1 || manifests[0].Name != "manifest.json" || intact["manifest.json"] == nil {
			continue
		}
		mp := d.Dir + "/manifest.json"
		data, err := readZipEntry(intact["manifest.json"], maxManifestJSON)
		if err != nil {
			add(CodeManifestInvalid, mp, "%s 无法读取：%v", mp, err)
			continue
		}
		m, code, msg := parseManifest(data)
		if code != "" {
			add(code, mp, "%s：%s", mp, msg)
			continue
		}
		if msg := manifestMismatch(m, pkg, d); msg != "" {
			add(CodeManifestMismatch, mp, "%s 和 package.json 对不上：%s", mp, msg)
		}
	}
	return v, nil
}

// findPrefix 找 package.json：在 zip 的根目录，或者在唯一的顶层文件夹里（用户解压后连文件夹一起重新压缩）。
func findPrefix(files []*zip.File, entries map[string][]*zip.File) (string, bool) {
	if len(entries["package.json"]) > 0 {
		return "", true
	}
	top := ""
	for _, f := range files {
		first, _, nested := strings.Cut(f.Name, "/")
		if !nested || (top != "" && first != top) {
			return "", false
		}
		top = first
	}
	if top == "" || len(entries[top+"/package.json"]) == 0 {
		return "", false
	}
	return top + "/", true
}

// safeEntryName 报告 name 能否安全地当作一层文件夹名或文件名：不能带出所在文件夹，
// 也不能是 Windows 上有特殊含义的写法。
func safeEntryName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == '/' || r == '\\' || r == ':' })
}

func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("文件太大（%d 字节）", f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

func hashEntry(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// 下面的 raw* 结构用指针区分“缺字段”和“空值”，类型不对时 json.Unmarshal 报错。

type rawTool struct {
	Name    *string `json:"name"`
	Version *string `json:"version"`
}

type rawFile struct {
	Name   *string `json:"name"`
	Size   *int64  `json:"size"`
	SHA256 *string `json:"sha256"`
	Role   *string `json:"role"`
}

type rawPackage struct {
	Schema    *string    `json:"schema"`
	PackageID *string    `json:"package_id"`
	CreatedAt *string    `json:"created_at"`
	Tool      *rawTool   `json:"tool"`
	Customer  *string    `json:"customer"`
	BatchIDs  *[]*string `json:"batch_ids"`
	Devices   *[]*struct {
		Dir     *string     `json:"dir"`
		LogType *string     `json:"log_type"`
		Files   *[]*rawFile `json:"files"`
	} `json:"devices"`
}

type rawManifest struct {
	Schema      *string  `json:"schema"`
	Tool        *rawTool `json:"tool"`
	CollectedAt *string  `json:"collected_at"`
	Customer    *string  `json:"customer"`
	BatchID     *string  `json:"batch_id"`
	Device      *struct {
		Type       *string `json:"type"`
		Vendor     *string `json:"vendor"`
		Model      *string `json:"model"`
		Serial     *string `json:"serial"`
		Hostname   *string `json:"hostname"`
		MgmtIP     *string `json:"mgmt_ip"`
		HostIP     *string `json:"host_ip"`
		OS         *string `json:"os"`
		Controller *struct {
			Type     *string `json:"type"`
			Firmware *string `json:"firmware"`
		} `json:"controller"`
	} `json:"device"`
	Log *struct {
		Type   *string `json:"type"`
		Method *string `json:"method"`
		Range  *struct {
			From *string `json:"from"`
			To   *string `json:"to"`
		} `json:"range"`
	} `json:"log"`
	Files     *[]*rawFile `json:"files"`
	Redaction *struct {
		Applied *bool      `json:"applied"`
		Rules   *[]*string `json:"rules"`
	} `json:"redaction"`
}

// checker 记下第一个不合规的字段。
type checker struct{ bad string }

func (c *checker) need(ok bool, field, why string) bool {
	if !ok && c.bad == "" {
		c.bad = field + " " + why
	}
	return ok
}

func (c *checker) str(p *string, field string) string {
	if c.need(p != nil, field, "缺失") {
		return *p
	}
	return ""
}

func (c *checker) match(p *string, re *regexp.Regexp, field string) string {
	s := c.str(p, field)
	if p != nil {
		c.need(re.MatchString(s), field, "格式不对")
	}
	return s
}

func (c *checker) tool(t *rawTool, field string) collect.Tool {
	if !c.need(t != nil, field, "缺失") {
		return collect.Tool{}
	}
	return collect.Tool{Name: c.str(t.Name, field+".name"), Version: c.str(t.Version, field+".version")}
}

func (c *checker) files(list []*rawFile, field string, roles ...string) []collect.File {
	out := make([]collect.File, 0, len(list))
	for i, f := range list {
		at := fmt.Sprintf("%s[%d]", field, i)
		if !c.need(f != nil, at, "缺失") {
			continue
		}
		cf := collect.File{
			Name:   c.str(f.Name, at+".name"),
			SHA256: c.match(f.SHA256, sha256RE, at+".sha256"),
			Role:   c.str(f.Role, at+".role"),
		}
		if c.need(f.Size != nil, at+".size", "缺失") {
			cf.Size = *f.Size
			c.need(cf.Size >= 0, at+".size", "不能是负数")
		}
		if f.Role != nil {
			c.need(slices.Contains(roles, cf.Role), at+".role", "只能是 "+strings.Join(roles, "、"))
		}
		out = append(out, cf)
	}
	return out
}

// schemaProblem 检查 schema 字段：want 之外的同类大版本是 schema_unsupported，其他都算格式不对。
func schemaProblem(p *string, want, invalid string) (code, msg string) {
	switch {
	case p == nil:
		return invalid, "schema 缺失"
	case *p == want:
		return "", ""
	case schemaRE.MatchString(*p) && strings.HasPrefix(*p, strings.TrimRight(want, "0123456789")):
		return CodeSchemaUnsupported, fmt.Sprintf("格式版本 %s 比这个程序支持的 %s 新，请升级", *p, want)
	}
	return invalid, fmt.Sprintf("schema 应为 %s，实际是 %q", want, *p)
}

// decodeJSON 解析 JSON，出错时返回给人看的原因。
func decodeJSON(data []byte, v any) string {
	d := json.NewDecoder(bytes.NewReader(data))
	err := d.Decode(v)
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &te) && te.Field == "":
		return "不是 JSON 对象"
	case errors.As(err, &te):
		return te.Field + " 的类型不对"
	case err != nil:
		return "不是合法的 JSON：" + err.Error()
	case d.More():
		return "不是合法的 JSON：后面还有多余的内容"
	}
	return ""
}

// peekSchema 先只读出 schema：更新的大版本可以改其他字段的类型，要先认出版本，再按第 1 版检查字段。
func peekSchema(data []byte) (schema *string, bad string) {
	var obj map[string]json.RawMessage
	if bad := decodeJSON(data, &obj); bad != "" {
		return nil, bad
	}
	if obj == nil {
		return nil, "不是 JSON 对象"
	}
	raw, ok := obj["schema"]
	if !ok || string(raw) == "null" {
		return nil, ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, "schema 的类型不对"
	}
	return &s, ""
}

func parsePackage(data []byte) (pkg *Package, code, msg string) {
	schema, bad := peekSchema(data)
	if bad != "" {
		return nil, CodePackageInvalid, "package.json " + bad
	}
	if code, msg := schemaProblem(schema, PackageSchema, CodePackageInvalid); code != "" {
		return nil, code, msg
	}
	var r rawPackage
	if bad := decodeJSON(data, &r); bad != "" {
		return nil, CodePackageInvalid, "package.json " + bad
	}
	var c checker
	pkg = &Package{
		Schema:    *schema,
		PackageID: c.match(r.PackageID, uuidRE, "package_id"),
		CreatedAt: c.match(r.CreatedAt, timeRE, "created_at"),
		Tool:      c.tool(r.Tool, "tool"),
		Customer:  c.str(r.Customer, "customer"),
		BatchIDs:  []string{},
	}
	if c.need(r.BatchIDs != nil, "batch_ids", "缺失") {
		for i, id := range *r.BatchIDs {
			pkg.BatchIDs = append(pkg.BatchIDs, c.match(id, uuidRE, fmt.Sprintf("batch_ids[%d]", i)))
		}
	}
	if c.need(r.Devices != nil, "devices", "缺失") && c.need(len(*r.Devices) > 0, "devices", "不能为空") {
		for i, d := range *r.Devices {
			at := fmt.Sprintf("devices[%d]", i)
			if !c.need(d != nil, at, "缺失") {
				continue
			}
			pd := PackageDevice{Dir: c.str(d.Dir, at+".dir"), LogType: c.match(d.LogType, logTypeRE, at+".log_type")}
			if c.need(d.Files != nil, at+".files", "缺失") {
				pd.Files = c.files(*d.Files, at+".files", "main", "extra", "manifest")
			}
			pkg.Devices = append(pkg.Devices, pd)
		}
	}
	if c.bad != "" {
		return nil, CodePackageInvalid, "package.json 的 " + c.bad
	}
	return pkg, "", ""
}

func parseManifest(data []byte) (m *collect.Manifest, code, msg string) {
	schema, bad := peekSchema(data)
	if bad != "" {
		return nil, CodeManifestInvalid, bad
	}
	if code, msg := schemaProblem(schema, collect.ManifestSchema, CodeManifestInvalid); code != "" {
		return nil, code, msg
	}
	var r rawManifest
	if bad := decodeJSON(data, &r); bad != "" {
		return nil, CodeManifestInvalid, bad
	}
	var c checker
	m = &collect.Manifest{
		Schema:      *schema,
		Tool:        c.tool(r.Tool, "tool"),
		CollectedAt: c.match(r.CollectedAt, timeRE, "collected_at"),
		Customer:    c.str(r.Customer, "customer"),
		BatchID:     c.match(r.BatchID, uuidRE, "batch_id"),
	}
	if d := r.Device; c.need(d != nil, "device", "缺失") {
		m.Device.Type = c.str(d.Type, "device.type")
		if d.Type != nil {
			c.need(slices.Contains(deviceType, *d.Type), "device.type", "只能是 "+strings.Join(deviceType, "、"))
		}
		m.Device.Vendor = c.str(d.Vendor, "device.vendor")
		m.Device.Model = c.str(d.Model, "device.model")
		m.Device.Serial = c.str(d.Serial, "device.serial")
		if c.need(d.Controller != nil, "device.controller", "缺失") {
			m.Device.Controller.Type = c.str(d.Controller.Type, "device.controller.type")
			m.Device.Controller.Firmware = c.str(d.Controller.Firmware, "device.controller.firmware")
		}
	}
	if l := r.Log; c.need(l != nil, "log", "缺失") {
		m.Log.Type = c.match(l.Type, logTypeRE, "log.type")
		m.Log.Method = c.str(l.Method, "log.method")
		if l.Range != nil {
			if l.Range.From != nil {
				c.match(l.Range.From, timeRE, "log.range.from")
			}
			if l.Range.To != nil {
				c.match(l.Range.To, timeRE, "log.range.to")
			}
		}
	}
	if c.need(r.Files != nil, "files", "缺失") && c.need(len(*r.Files) > 0, "files", "不能为空") {
		m.Files = c.files(*r.Files, "files", "main", "extra")
		for i, f := range m.Files {
			c.need(f.Size > 0, fmt.Sprintf("files[%d].size", i), "不能是 0")
		}
		c.need(slices.ContainsFunc(m.Files, func(f collect.File) bool { return f.Role == "main" }), "files", "里没有 role 为 main 的文件")
	}
	if c.need(r.Redaction != nil, "redaction", "缺失") {
		c.need(r.Redaction.Applied != nil, "redaction.applied", "缺失")
	}
	if c.bad != "" {
		return nil, CodeManifestInvalid, c.bad
	}
	return m, "", ""
}

// manifestMismatch 核对 manifest 和 package.json 里这台设备的记录，返回第一处不一致；一致时返回空字符串。
func manifestMismatch(m *collect.Manifest, pkg *Package, d PackageDevice) string {
	switch {
	case m.Log.Type != d.LogType:
		return fmt.Sprintf("log.type 是 %s，package.json 里是 %s", m.Log.Type, d.LogType)
	case m.Customer != pkg.Customer:
		return fmt.Sprintf("customer 是 %q，package.json 里是 %q", m.Customer, pkg.Customer)
	case !slices.Contains(pkg.BatchIDs, m.BatchID):
		return fmt.Sprintf("batch_id %s 不在 package.json 的 batch_ids 里", m.BatchID)
	}
	key := func(f collect.File) string {
		return fmt.Sprintf("%s\x00%d\x00%s\x00%s", f.Name, f.Size, f.SHA256, f.Role)
	}
	var want, got []string
	for _, f := range d.Files {
		if f.Role != "manifest" {
			want = append(want, key(f))
		}
	}
	for _, f := range m.Files {
		got = append(got, key(f))
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return "文件列表（文件名、大小、SHA-256、role）不一致"
	}
	return ""
}
