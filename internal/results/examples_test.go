// SPDX-License-Identifier: GPL-3.0-or-later

package results

// 契约样例：docs/schema/examples/ 下的 manifest.json、package.json 和 packages/ 里的样例包由这里生成，
// packages/expected.json 写明每个样例包应得到的检查结果。网站的服务端和浏览器端用同一套样例做测试，
// 所以样例必须和代码保持一致：改了打包或检查规则后运行
//
//	go test ./internal/results -run TestExamples -update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
)

var updateExamples = flag.Bool("update", false, "重新生成 docs/schema/examples/ 下的契约样例")

const examplesDir = "../../docs/schema/examples"

var (
	exampleZone     = time.FixedZone("CST", 8*3600)
	exampleTool     = collect.Tool{Name: "LogDiag Collector", Version: "0.1.0"}
	exampleBatch    = "2f6c1d0e-8a4b-4c1e-9f3a-5b7d2e9c4a10"
	examplePackage  = "5d0c8e1a-3b7f-4e2a-9c61-8f4b2a7d9e03"
	exampleCustomer = "示例客户"
	exampleModified = time.Date(2026, 11, 20, 14, 6, 0, 0, exampleZone)
)

// expectedPackage 是 expected.json 里的一项。
type expectedPackage struct {
	File        string `json:"file"`
	Description string `json:"description"`
	// Result：ok 没有问题；problems 有问题，见 Problems；not_package 不是离线上传包（找不到 package.json），
	// 网站照旧按普通日志处理；not_zip 不是完整的 zip。
	Result   string            `json:"result"`
	Nested   bool              `json:"nested,omitempty"` // package.json 在唯一的顶层文件夹里
	Package  *expectedSummary  `json:"package,omitempty"`
	Problems []expectedProblem `json:"problems,omitempty"`
}

type expectedSummary struct {
	PackageID string           `json:"package_id"`
	Customer  string           `json:"customer"`
	Devices   []expectedDevice `json:"devices"`
}

type expectedDevice struct {
	Dir     string   `json:"dir"`
	LogType string   `json:"log_type"`
	Main    []string `json:"main"`
	Extra   []string `json:"extra"`
}

type expectedProblem struct {
	Code string `json:"code"`
	Path string `json:"path"`
}

type expectedFile struct {
	Note     string            `json:"_说明"`
	Packages []expectedPackage `json:"packages"`
}

const expectedNote = "每个样例包应得到的检查结果，检查步骤见 docs/schema/README.md。problems 按顺序比较 code 和 path，" +
	"message 只给人看、不比较。浏览器端不计算大文件的 SHA-256，比较时跳过 path 不是 manifest.json 的 checksum_mismatch。" +
	"样例里的日志文件是合成的小文件，只能测拆包和校验，不能测解析器。"

func TestExamples(t *testing.T) {
	dir := examplesDir
	if !*updateExamples {
		dir = t.TempDir()
	}
	want := buildExamples(t, dir)

	if !*updateExamples {
		// 提交的样例必须和现在的代码生成的一致（比较 JSON 原文和 zip 里每个文件的内容，不比较压缩后的字节）。
		for _, name := range []string{"manifest.json", "package.json", "packages/expected.json"} {
			got, _ := os.ReadFile(filepath.Join(examplesDir, name))
			gen, _ := os.ReadFile(filepath.Join(dir, name))
			if !bytes.Equal(got, gen) {
				t.Errorf("docs/schema/examples/%s 和代码生成的不一致，请运行 go test ./internal/results -run TestExamples -update", name)
			}
		}
		for _, p := range want {
			if got, gen := zipListing(t, filepath.Join(examplesDir, "packages", p.File)), zipListing(t, filepath.Join(dir, "packages", p.File)); got != gen {
				t.Errorf("样例包 %s 和代码生成的不一致，请运行 go test ./internal/results -run TestExamples -update\n提交的：\n%s\n生成的：\n%s", p.File, got, gen)
			}
		}
	}

	// 提交的样例包逐个检查，结果必须和 expected.json 一致。
	data, err := os.ReadFile(filepath.Join(examplesDir, "packages", "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp expectedFile
	if err := json.Unmarshal(data, &exp); err != nil {
		t.Fatal(err)
	}
	if len(exp.Packages) != len(want) {
		t.Fatalf("expected.json 有 %d 项，代码里有 %d 项", len(exp.Packages), len(want))
	}
	for _, p := range exp.Packages {
		t.Run(p.File, func(t *testing.T) {
			got := verifyExample(t, filepath.Join(examplesDir, "packages", p.File))
			g, _ := json.MarshalIndent(got, "", "  ")
			w, _ := json.MarshalIndent(expectedPackage{File: p.File, Result: p.Result, Nested: p.Nested, Package: p.Package, Problems: p.Problems}, "", "  ")
			if !bytes.Equal(g, w) {
				t.Errorf("检查结果：\n%s\n应为：\n%s", g, w)
			}
		})
	}
}

// verifyExample 用 VerifyPackage 检查样例包，结果写成 expected.json 的格式（不含说明）。
func verifyExample(t *testing.T, path string) expectedPackage {
	t.Helper()
	got := expectedPackage{File: filepath.Base(path)}
	v, err := VerifyPackage(path)
	switch {
	case errors.Is(err, ErrNotZip):
		got.Result = "not_zip"
		return got
	case errors.Is(err, ErrNotPackage):
		got.Result = "not_package"
		return got
	case err != nil:
		t.Fatal(err)
	}
	got.Nested = v.Prefix != ""
	if !v.OK() {
		got.Result = "problems"
		for _, p := range v.Problems {
			if p.Message == "" {
				t.Errorf("问题 %s 没有说明", p.Code)
			}
			got.Problems = append(got.Problems, expectedProblem{Code: p.Code, Path: p.Path})
		}
		return got
	}
	got.Result = "ok"
	got.Package = summarize(v.Package)
	return got
}

func summarize(pkg *Package) *expectedSummary {
	s := &expectedSummary{PackageID: pkg.PackageID, Customer: pkg.Customer}
	for _, d := range pkg.Devices {
		ed := expectedDevice{Dir: d.Dir, LogType: d.LogType, Main: []string{}, Extra: []string{}}
		for _, f := range d.Files {
			switch f.Role {
			case "main":
				ed.Main = append(ed.Main, f.Name)
			case "extra":
				ed.Extra = append(ed.Extra, f.Name)
			}
		}
		s.Devices = append(s.Devices, ed)
	}
	return s
}

// buildExamples 在 dir 下生成全部样例，返回每个样例包应得到的结果。
func buildExamples(t *testing.T, dir string) []expectedPackage {
	t.Helper()
	pkgDir := filepath.Join(dir, "packages")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old, _ := filepath.Glob(filepath.Join(pkgDir, "*.zip"))
	for _, f := range old {
		os.Remove(f)
	}

	// 三台设备：华为、浪潮走真正的 Job.Save；HPE 另带两个附带文件（role 为 extra）。
	root := t.TempDir()
	huawei := saveExample(t, root, 3, collect.Device{Type: "server", Vendor: "Huawei", Model: "2288H V5", Serial: "2102311TYBN0K8000123",
		Address: "192.0.2.10", Controller: collect.Controller{Type: "iBMC", Firmware: "3.08.01.07"}},
		collect.Log{Type: "huawei.ibmc.dump", Method: "redfish-oem"}, "dump_info.tar.gz", tarGz("dump_info/README.txt"))
	inspur := saveExample(t, root, 4, collect.Device{Type: "server", Vendor: "Inspur", Model: "NF5280M6", Serial: "220123456",
		Address: "192.0.2.11", Controller: collect.Controller{Type: "BMC", Firmware: "4.20.0"}},
		collect.Log{Type: "inspur.bmc.onekeylog", Method: "bmc-web-api"}, "onekeylog.tar.gz", tarGz("onekeylog/README.txt"))
	hpe := saveExample(t, root, 5, collect.Device{Type: "server", Vendor: "HPE", Model: "ProLiant DL380 Gen10", Serial: "CZJ00000A1",
		Address: "192.0.2.12", Controller: collect.Controller{Type: "iLO", Firmware: "2.81"}},
		collect.Log{Type: "hpe.ilo.ahs", Method: "redfish-oem"}, "HPE_CZJ00000A1_20261120.ahs", []byte("AHS 合成样例，不是真机日志。\n"))
	addExtras(t, hpe, map[string]string{
		"IML.json": "/redfish/v1/Systems/1/LogServices/IML/Entries",
		"IEL.json": "/redfish/v1/Managers/1/LogServices/IEL/Entries",
	})
	results := []*collect.Result{huawei, inspur, hpe}
	var dirs []string
	for _, r := range results {
		filepath.WalkDir(r.Dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				os.Chtimes(p, exampleModified, exampleModified)
			}
			return nil
		})
		rel, _ := filepath.Rel(root, r.Dir)
		dirs = append(dirs, filepath.ToSlash(rel))
	}

	defer func(f func() string) { newPackageID = f }(newPackageID)
	newPackageID = func() string { return examplePackage }
	res, err := BuildPackage(root, dirs, exampleTool, time.Date(2026, 11, 20, 14, 30, 0, 0, exampleZone))
	if err != nil {
		t.Fatal(err)
	}
	okPath := filepath.Join(pkgDir, "ok.zip")
	copyFile(t, res.Path, okPath)

	base := readZip(t, okPath)
	var pkg Package
	if err := json.Unmarshal(base.get(t, "package.json"), &pkg); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "package.json"), &pkg)
	copyFile(t, filepath.Join(huawei.Dir, "manifest.json"), filepath.Join(dir, "manifest.json"))
	dH, dI, dP := pkg.Devices[0].Dir, pkg.Devices[1].Dir, pkg.Devices[2].Dir

	var exp []expectedPackage
	add := func(e expectedPackage) { exp = append(exp, e) }
	add(expectedPackage{File: "ok.zip", Description: "Collector 打的离线上传包：华为、浪潮各一台，HPE 一台另带 IML.json、IEL.json 两个附带文件",
		Result: "ok", Package: summarize(&pkg)})

	// 用户解压后连文件夹一起用资源管理器重新压缩：多一层顶层文件夹（GBK 文件名，不设 UTF-8 标志），
	// 有文件夹条目，所有文件都改成 Deflate 压缩。
	top, _ := simplifiedchinese.GBK.NewEncoder().String("LogDiag-示例客户-20261120-1430-5d0c8e1a")
	rez := []zipEntry{{name: top + "/", nonUTF8: true}}
	for _, d := range pkg.Devices {
		rez = append(rez, zipEntry{name: top + "/" + d.Dir + "/", nonUTF8: true})
	}
	for _, e := range base {
		rez = append(rez, zipEntry{name: top + "/" + e.name, data: e.data, method: zip.Deflate, nonUTF8: true})
	}
	writeZip(t, filepath.Join(pkgDir, "ok-rezipped.zip"), rez)
	add(expectedPackage{File: "ok-rezipped.zip", Description: "ok.zip 解压后连文件夹一起重新压缩：多一层顶层文件夹（GBK 编码、无 UTF-8 标志），全部用 Deflate 压缩，带文件夹条目",
		Result: "ok", Nested: true, Package: summarize(&pkg)})

	writeZip64(t, filepath.Join(pkgDir, "ok-zip64.zip"), base)
	add(expectedPackage{File: "ok-zip64.zip", Description: "ok.zip 的内容，所有条目都用 zip64 扩展字段和 zip64 目录结尾记录（大于 4 GB 的包就是这样）",
		Result: "ok", Package: summarize(&pkg)})

	// 文件损坏：华为主文件内容被改（大小不变），浪潮主文件丢了，HPE 的 IEL.json 被截短。
	bad := base.clone()
	b := bad.get(t, dH+"/dump_info.tar.gz")
	b[len(b)-1] ^= 0xff
	bad = bad.without(dI + "/onekeylog.tar.gz")
	bad.set(dP+"/IEL.json", bad.get(t, dP+"/IEL.json")[:10])
	writeZip(t, filepath.Join(pkgDir, "bad-files.zip"), bad)
	add(expectedPackage{File: "bad-files.zip", Description: "文件损坏：内容被改、文件丢失、文件被截短；问题一次列全",
		Result: "problems", Problems: []expectedProblem{
			{CodeChecksumMismatch, dH + "/dump_info.tar.gz"},
			{CodeFileMissing, dI + "/onekeylog.tar.gz"},
			{CodeSizeMismatch, dP + "/IEL.json"},
		}})

	// manifest 不对：华为的 log.type 和 package.json 不一致，浪潮的 manifest.json 不是合法 JSON，
	// HPE 的 package.json 漏列了 manifest.json。改过的 manifest.json 在 package.json 里同步了大小和 SHA-256。
	bad = base.clone()
	bp := clonePackage(t, &pkg)
	m := bad.get(t, dH+"/manifest.json")
	bad.set(dH+"/manifest.json", bytes.Replace(m, []byte(`"huawei.ibmc.dump"`), []byte(`"huawei.ibmc.sel"`), 1))
	bad.set(dI+"/manifest.json", []byte(`{"schema": "logdiag.manifest/1", "tool": `))
	for i := range 2 {
		f := &bp.Devices[i].Files[0]
		data := bad.get(t, bp.Devices[i].Dir+"/"+f.Name)
		f.Size, f.SHA256 = int64(len(data)), sha256Hex(data)
	}
	bp.Devices[2].Files = bp.Devices[2].Files[1:]
	bad.set("package.json", marshalJSON(t, bp))
	writeZip(t, filepath.Join(pkgDir, "bad-manifest.zip"), bad)
	add(expectedPackage{File: "bad-manifest.zip", Description: "manifest 不对：和 package.json 不一致、不是合法 JSON、package.json 没列出 manifest.json",
		Result: "problems", Problems: []expectedProblem{
			{CodeManifestMismatch, dH + "/manifest.json"},
			{CodeManifestInvalid, dI + "/manifest.json"},
			{CodeManifestMissing, dP},
		}})

	// 路径不安全：设备文件夹名带 ../，同一个设备文件夹列了两次，文件名带冒号（Windows 上会写进备用数据流）。
	bad = base.clone()
	bp = clonePackage(t, &pkg)
	evil := bp.Devices[0]
	evil.Dir = "../evil"
	bp.Devices[1].Files[1].Name = "C:onekeylog.tar.gz"
	bp.Devices = []PackageDevice{bp.Devices[0], evil, bp.Devices[0], bp.Devices[1]}
	bad.set("package.json", marshalJSON(t, bp))
	writeZip(t, filepath.Join(pkgDir, "bad-paths.zip"), bad)
	add(expectedPackage{File: "bad-paths.zip", Description: "路径不安全：设备文件夹名带 ../、设备文件夹重复、文件名带冒号（因此也和 manifest 对不上）；网站只按检查过的名字读文件，不能按 zip 里的条目名解压",
		Result: "problems", Problems: []expectedProblem{
			{CodePathInvalid, "../evil"},
			{CodeDuplicatePath, dH},
			{CodePathInvalid, dI + "/C:onekeylog.tar.gz"},
			{CodeManifestMismatch, dI + "/manifest.json"}, // 文件名改了，和 manifest 的文件列表也对不上
		}})

	bad = base.clone()
	bad.set("package.json", bytes.Replace(bad.get(t, "package.json"), []byte(`"package_id"`), []byte(`"package-id"`), 1))
	writeZip(t, filepath.Join(pkgDir, "bad-package.zip"), bad)
	add(expectedPackage{File: "bad-package.zip", Description: "package.json 缺少 package_id：整个包不能用",
		Result: "problems", Problems: []expectedProblem{{CodePackageInvalid, "package.json"}}})

	bad = base.clone()
	bad.set("package.json", bytes.Replace(bad.get(t, "package.json"), []byte(PackageSchema), []byte("logdiag.package/2"), 1))
	writeZip(t, filepath.Join(pkgDir, "newer-version.zip"), bad)
	add(expectedPackage{File: "newer-version.zip", Description: "更新的大版本（logdiag.package/2）：提示升级，不按第 1 版硬读",
		Result: "problems", Problems: []expectedProblem{{CodeSchemaUnsupported, "package.json"}}})

	writeZip(t, filepath.Join(pkgDir, "not-a-package.zip"), []zipEntry{
		{name: "FFDC/xcc_ffdc_J300XXXX.tzz", data: []byte("合成样例：普通日志压缩包，不是离线上传包。\n")},
		{name: "readme.txt", data: []byte("没有 package.json。\n"), method: zip.Deflate},
	})
	add(expectedPackage{File: "not-a-package.zip", Description: "普通的 zip（没有 package.json）：不是离线上传包，网站照旧按普通日志处理", Result: "not_package"})

	if err := os.WriteFile(filepath.Join(pkgDir, "not-a-zip.zip"), []byte("这不是 zip 文件，只是扩展名叫 .zip。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	add(expectedPackage{File: "not-a-zip.zip", Description: "扩展名是 .zip 但不是 zip 文件", Result: "not_zip"})

	writeJSON(t, filepath.Join(pkgDir, "expected.json"), expectedFile{Note: expectedNote, Packages: exp})
	return exp
}

// saveExample 用 Job.Save 生成一个设备文件夹，收集时间是 2026-11-20 14:0x（北京时间）。
func saveExample(t *testing.T, root string, minute int, dev collect.Device, log collect.Log, name string, body []byte) *collect.Result {
	t.Helper()
	at := time.Date(2026, 11, 20, 14, minute, 0, 0, exampleZone)
	j := &collect.Job{
		Device: dev,
		Store:  &collect.Store{Dir: root, Customer: exampleCustomer, BatchID: exampleBatch, Tool: exampleTool, Now: func() time.Time { return at }},
	}
	res, err := j.Save(log, name, func(w io.Writer) error {
		_, err := w.Write(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// addExtras 往设备文件夹里加附带文件（Redfish 日志条目集合），并写进 manifest。
func addExtras(t *testing.T, res *collect.Result, files map[string]string) {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		data := marshalJSON(t, map[string]any{
			"@odata.id":           files[n],
			"Members@odata.count": 1,
			"Members": []map[string]any{{
				"@odata.id": files[n] + "/1", "Id": "1", "Created": "2026-11-18T09:12:00Z", "Severity": "OK",
				"Message": "合成样例条目，不是真机日志。",
			}},
		})
		if err := os.WriteFile(filepath.Join(res.Dir, n), data, 0o644); err != nil {
			t.Fatal(err)
		}
		res.Manifest.Files = append(res.Manifest.Files, collect.File{Name: n, Size: int64(len(data)), SHA256: sha256Hex(data), Role: "extra"})
	}
	data, err := json.MarshalIndent(res.Manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(res.Dir, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tarGz 生成一个只含说明文件的 tar.gz，代替设备的日志包。
func tarGz(name string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("合成样例：只用于测试离线上传包的拆包和校验，不是真机日志。\n")
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: exampleModified, Format: tar.FormatPAX})
	tw.Write(body)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type zipEntry struct {
	name    string
	data    []byte
	method  uint16
	nonUTF8 bool
}

type zipEntries []zipEntry

func readZip(t *testing.T, path string) zipEntries {
	t.Helper()
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	var out zipEntries
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, zipEntry{name: f.Name, data: data, method: f.Method})
	}
	return out
}

func (z zipEntries) clone() zipEntries {
	out := make(zipEntries, len(z))
	for i, e := range z {
		e.data = bytes.Clone(e.data)
		out[i] = e
	}
	return out
}

func (z zipEntries) get(t *testing.T, name string) []byte {
	t.Helper()
	for _, e := range z {
		if e.name == name {
			return e.data
		}
	}
	t.Fatalf("样例包里没有 %s", name)
	return nil
}

func (z zipEntries) set(name string, data []byte) {
	for i := range z {
		if z[i].name == name {
			z[i].data = data
		}
	}
}

func (z zipEntries) without(name string) zipEntries {
	return slices.DeleteFunc(z, func(e zipEntry) bool { return e.name == name })
}

func writeZip(t *testing.T, path string, entries []zipEntry) {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := z.CreateHeader(&zip.FileHeader{Name: e.name, Method: e.method, Modified: exampleModified, NonUTF8: e.nonUTF8})
		if err == nil {
			_, err = w.Write(e.data)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeZip64 写一个所有条目都用 zip64 扩展字段、带 zip64 目录结尾记录的 zip。Go 的 archive/zip
// 只在文件超过 4 GB 时才这样写，样例里的小文件只能手工拼出来。
func writeZip64(t *testing.T, path string, entries []zipEntry) {
	t.Helper()
	const dosTime, dosDate = 14<<11 | 6<<5, (2026-1980)<<9 | 11<<5 | 20
	le := binary.LittleEndian
	var out, cd bytes.Buffer
	for _, e := range entries {
		data := e.data
		if e.method == zip.Deflate {
			var c bytes.Buffer
			fw, _ := flate.NewWriter(&c, flate.DefaultCompression)
			fw.Write(e.data)
			fw.Close()
			data = c.Bytes()
		}
		var flags uint16
		if strings.ContainsFunc(e.name, func(r rune) bool { return r >= 0x80 }) {
			flags |= 0x800
		}
		crc := crc32.ChecksumIEEE(e.data)
		offset := uint64(out.Len())

		local := le.AppendUint32(nil, 0x04034b50)
		local = le.AppendUint16(local, 45)
		local = le.AppendUint16(local, flags)
		local = le.AppendUint16(local, e.method)
		local = le.AppendUint16(local, dosTime)
		local = le.AppendUint16(local, dosDate)
		local = le.AppendUint32(local, crc)
		local = le.AppendUint32(local, 0xffffffff)
		local = le.AppendUint32(local, 0xffffffff)
		local = le.AppendUint16(local, uint16(len(e.name)))
		local = le.AppendUint16(local, 20)
		local = append(local, e.name...)
		local = le.AppendUint16(local, 0x0001)
		local = le.AppendUint16(local, 16)
		local = le.AppendUint64(local, uint64(len(e.data)))
		local = le.AppendUint64(local, uint64(len(data)))
		out.Write(local)
		out.Write(data)

		c := le.AppendUint32(nil, 0x02014b50)
		c = le.AppendUint16(c, 45)
		c = le.AppendUint16(c, 45)
		c = le.AppendUint16(c, flags)
		c = le.AppendUint16(c, e.method)
		c = le.AppendUint16(c, dosTime)
		c = le.AppendUint16(c, dosDate)
		c = le.AppendUint32(c, crc)
		c = le.AppendUint32(c, 0xffffffff)
		c = le.AppendUint32(c, 0xffffffff)
		c = le.AppendUint16(c, uint16(len(e.name)))
		c = le.AppendUint16(c, 28)
		c = le.AppendUint16(c, 0) // 注释长度
		c = le.AppendUint16(c, 0) // 起始磁盘
		c = le.AppendUint16(c, 0) // 内部属性
		c = le.AppendUint32(c, 0) // 外部属性
		c = le.AppendUint32(c, 0xffffffff)
		c = append(c, e.name...)
		c = le.AppendUint16(c, 0x0001)
		c = le.AppendUint16(c, 24)
		c = le.AppendUint64(c, uint64(len(e.data)))
		c = le.AppendUint64(c, uint64(len(data)))
		c = le.AppendUint64(c, offset)
		cd.Write(c)
	}
	cdOffset, cdSize, n := uint64(out.Len()), uint64(cd.Len()), uint64(len(entries))
	out.Write(cd.Bytes())
	end64 := uint64(out.Len())

	r := le.AppendUint32(nil, 0x06064b50)
	r = le.AppendUint64(r, 44)
	r = le.AppendUint16(r, 45)
	r = le.AppendUint16(r, 45)
	r = le.AppendUint32(r, 0)
	r = le.AppendUint32(r, 0)
	r = le.AppendUint64(r, n)
	r = le.AppendUint64(r, n)
	r = le.AppendUint64(r, cdSize)
	r = le.AppendUint64(r, cdOffset)
	r = le.AppendUint32(r, 0x07064b50)
	r = le.AppendUint32(r, 0)
	r = le.AppendUint64(r, end64)
	r = le.AppendUint32(r, 1)
	r = le.AppendUint32(r, 0x06054b50)
	r = le.AppendUint16(r, 0)
	r = le.AppendUint16(r, 0)
	r = le.AppendUint16(r, 0xffff)
	r = le.AppendUint16(r, 0xffff)
	r = le.AppendUint32(r, 0xffffffff)
	r = le.AppendUint32(r, 0xffffffff)
	r = le.AppendUint16(r, 0)
	out.Write(r)
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// zipListing 列出 zip 里每个条目的名字、压缩方式、大小和 CRC，用来比较两个包的内容是否相同。
func zipListing(t *testing.T, path string) string {
	t.Helper()
	z, err := zip.OpenReader(path)
	if err != nil {
		data, _ := os.ReadFile(path)
		return fmt.Sprintf("不是 zip：%x\n", sha256Hex(data))
	}
	defer z.Close()
	var b strings.Builder
	for _, f := range z.File {
		fmt.Fprintf(&b, "%q method=%d size=%d crc=%08x utf8=%v\n", f.Name, f.Method, f.UncompressedSize64, f.CRC32, f.Flags&0x800 != 0)
	}
	return b.String()
}

func clonePackage(t *testing.T, p *Package) *Package {
	t.Helper()
	var out Package
	if err := json.Unmarshal(marshalJSON(t, p), &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.WriteFile(path, marshalJSON(t, v), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err == nil {
		err = os.WriteFile(to, data, 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
