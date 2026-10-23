// SPDX-License-Identifier: GPL-3.0-or-later

// Package results 列出保存位置里已收集的日志包，并把选中的结果打成离线上传包。
//
// 每个设备文件夹里都有 manifest.json（见 internal/collect），这里只读它，不另建索引：
// 用户在资源管理器里移动、删除文件夹后，刷新一下就能看到真实情况。
package results

import (
	"archive/zip"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
)

// PackageSchema 是离线上传包里 package.json 的格式，见 docs/schema/package-1.schema.json。
const PackageSchema = "logdiag.package/1"

// PackageFolder 是保存位置下存放离线上传包的文件夹。
const PackageFolder = "离线上传包"

// maxDepth 是 manifest.json 相对保存位置的最大深度：[客户]/日期/设备/manifest.json。
const maxDepth = 4

// File 是结果里的一个文件和它在磁盘上的状态。
type File struct {
	collect.File
	Missing bool `json:"missing,omitempty"`
	SizeOK  bool `json:"size_ok"`
}

// Entry 是一次收集的结果（一个设备文件夹）。
type Entry struct {
	Dir         string                 `json:"dir"`  // 相对保存位置，用 / 分隔
	Path        string                 `json:"path"` // 完整路径
	CollectedAt string                 `json:"collected_at"`
	Customer    string                 `json:"customer"`
	BatchID     string                 `json:"batch_id"`
	Tool        collect.Tool           `json:"tool"`
	Device      collect.ManifestDevice `json:"device"`
	Log         collect.Log            `json:"log"`
	Files       []File                 `json:"files"`
	Size        int64                  `json:"size"`
	Problem     string                 `json:"problem,omitempty"` // 文件缺失、大小不对或 manifest 无法读取
}

// Scan 找出 root 下所有设备文件夹，按收集时间从新到旧排列。root 不存在时返回空列表。
// 只检查文件是否存在和大小，不计算 SHA-256（日志包可能有几个 GB）。
func Scan(root string) ([]Entry, error) {
	var out []Entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return nil // 没有权限的子文件夹跳过
		}
		rel, _ := filepath.Rel(root, path)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if rel != "." && (depth >= maxDepth-1 || d.Name() == PackageFolder || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == "manifest.json" && depth >= 1 {
			out = append(out, readEntry(root, filepath.Dir(path)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b Entry) int {
		return cmp.Or(-strings.Compare(a.CollectedAt, b.CollectedAt), strings.Compare(a.Dir, b.Dir))
	})
	return out, nil
}

func readEntry(root, dir string) Entry {
	rel, _ := filepath.Rel(root, dir)
	e := Entry{Dir: filepath.ToSlash(rel), Path: dir}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	var m collect.Manifest
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err != nil || !strings.HasPrefix(m.Schema, "logdiag.manifest/") {
		e.Problem = "manifest.json 无法读取"
		return e
	}
	e.CollectedAt, e.Customer, e.BatchID, e.Tool, e.Device, e.Log = m.CollectedAt, m.Customer, m.BatchID, m.Tool, m.Device, m.Log
	for _, f := range m.Files {
		rf := File{File: f}
		st, err := os.Stat(filepath.Join(dir, safeName(f.Name)))
		switch {
		case err != nil:
			rf.Missing = true
			e.Problem = "文件 " + f.Name + " 不存在"
		case st.Size() != f.Size:
			e.Problem = fmt.Sprintf("文件 %s 的大小与记录不符", f.Name)
		default:
			rf.SizeOK = true
		}
		e.Size += f.Size
		e.Files = append(e.Files, rf)
	}
	if len(m.Files) == 0 {
		e.Problem = "manifest.json 里没有文件"
	}
	return e
}

// safeName 只取文件名本身，防止 manifest 里的 ../ 把路径带出设备文件夹。
func safeName(name string) string {
	return filepath.Base(filepath.Clean("/" + filepath.FromSlash(name)))
}

// Within 报告 path 是否在 root 里面（含 root 本身）。
func Within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Package 是 package.json 的内容。
type Package struct {
	Schema    string          `json:"schema"`
	PackageID string          `json:"package_id"`
	CreatedAt string          `json:"created_at"`
	Tool      collect.Tool    `json:"tool"`
	Customer  string          `json:"customer"`
	BatchIDs  []string        `json:"batch_ids"`
	Devices   []PackageDevice `json:"devices"`
}

// PackageDevice 是包里的一个设备文件夹。
type PackageDevice struct {
	Dir     string         `json:"dir"`
	LogType string         `json:"log_type"`
	Files   []collect.File `json:"files"` // 含 manifest.json（role 为 manifest）
}

// PackageResult 是生成的离线上传包。
type PackageResult struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Devices int    `json:"devices"`
}

// BuildPackage 把 dirs（相对 root 的设备文件夹）打成一个 zip：每个设备文件夹放在包的根目录下，
// 最后写 package.json 列出全部文件和 SHA-256。打包时逐个核对日志文件的 SHA-256，不一致就停止。
// 一个包只能有一个客户的结果，网站按客户归档。
func BuildPackage(root string, dirs []string, tool collect.Tool, now time.Time) (*PackageResult, error) {
	if len(dirs) == 0 {
		return nil, errors.New("请先选择要打包的结果")
	}
	var entries []Entry
	seenBase := map[string]bool{}
	for _, d := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(d))
		if !Within(root, abs) || abs == filepath.Clean(root) {
			return nil, fmt.Errorf("%q 不在保存位置里", d)
		}
		e := readEntry(root, abs)
		if e.Problem != "" {
			return nil, fmt.Errorf("%s：%s", e.Dir, e.Problem)
		}
		base := filepath.Base(abs)
		if seenBase[base] {
			return nil, fmt.Errorf("选中的结果里有两个同名文件夹 %s，请分开打包", base)
		}
		seenBase[base] = true
		entries = append(entries, e)
	}
	customer := entries[0].Customer
	for _, e := range entries[1:] {
		if e.Customer != customer {
			return nil, errors.New("一个离线上传包只能包含一个客户的结果，请按客户分开打包")
		}
	}

	pkg := Package{Schema: PackageSchema, PackageID: newUUID(), CreatedAt: now.Format(time.RFC3339), Tool: tool, Customer: customer}
	outDir := filepath.Join(root, PackageFolder)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	name := "LogDiag"
	if customer != "" {
		name += "-" + safePart(customer)
	}
	name += "-" + now.Format("20060102-1504") + "-" + pkg.PackageID[:8] + ".zip"
	out := filepath.Join(outDir, name)

	f, err := os.OpenFile(out+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	err = writePackage(f, &pkg, entries)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(out+".part", out)
	}
	if err != nil {
		os.Remove(out + ".part")
		return nil, err
	}
	st, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	return &PackageResult{Path: out, Size: st.Size(), Devices: len(entries)}, nil
}

func writePackage(w io.Writer, pkg *Package, entries []Entry) error {
	z := zip.NewWriter(w)
	for _, e := range entries {
		if e.BatchID != "" && !slices.Contains(pkg.BatchIDs, e.BatchID) {
			pkg.BatchIDs = append(pkg.BatchIDs, e.BatchID)
		}
		base := filepath.Base(e.Path)
		pd := PackageDevice{Dir: base, LogType: e.Log.Type}
		mf, err := addFile(z, base+"/manifest.json", filepath.Join(e.Path, "manifest.json"), "")
		if err != nil {
			return err
		}
		mf.Role = "manifest"
		pd.Files = append(pd.Files, mf)
		for _, f := range e.Files {
			got, err := addFile(z, base+"/"+safeName(f.Name), filepath.Join(e.Path, safeName(f.Name)), f.SHA256)
			if err != nil {
				return err
			}
			got.Role = f.Role
			pd.Files = append(pd.Files, got)
		}
		pkg.Devices = append(pkg.Devices, pd)
	}
	data, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	pw, err := z.Create("package.json")
	if err != nil {
		return err
	}
	if _, err := pw.Write(append(data, '\n')); err != nil {
		return err
	}
	return z.Close()
}

// addFile 把文件写进 zip，同时计算 SHA-256；want 非空时核对。日志包本身已压缩，直接存储不再压缩。
func addFile(z *zip.Writer, name, path, want string) (collect.File, error) {
	src, err := os.Open(path)
	if err != nil {
		return collect.File{}, err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return collect.File{}, err
	}
	method := zip.Store
	if lower := strings.ToLower(name); strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".log") {
		method = zip.Deflate
	}
	hdr := &zip.FileHeader{Name: name, Method: method, Modified: st.ModTime()}
	dst, err := z.CreateHeader(hdr)
	if err != nil {
		return collect.File{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if err != nil {
		return collect.File{}, fmt.Errorf("打包 %s：%w", name, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want != "" && !strings.EqualFold(sum, want) {
		return collect.File{}, fmt.Errorf("%s 的 SHA-256 与收集时记录的不一致，文件可能已损坏或被修改", name)
	}
	return collect.File{Name: filepath.Base(path), Size: n, SHA256: sum}, nil
}

// safePart 把客户名称变成可用在文件名里的一段。
func safePart(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) || r == ' ' {
			return '-'
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40])
	}
	return strings.Trim(s, ".-")
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
