// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Store 把日志包保存到输出目录，一台设备一个文件夹：
//
//	<Dir>/[客户]/<日期>/<品牌_型号_序列号_日志类型_时间>/
//	    manifest.json
//	    dump_info.tar.gz
//
// 这个结构直接就是离线上传包里的布局，打包时不用再整理。
type Store struct {
	Dir      string
	Customer string // 可空
	BatchID  string // 同一次批量收集共用；为空时自动生成
	Tool     Tool
	Now      func() time.Time // 为 nil 时用 time.Now

	once sync.Once
}

// Result 是一次收集的结果。
type Result struct {
	Dir      string // 设备文件夹
	Path     string // 主日志包
	Manifest Manifest
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) batchID() string {
	s.once.Do(func() {
		if s.BatchID == "" {
			s.BatchID = newUUID()
		}
	})
	return s.BatchID
}

// Save 创建设备文件夹，由 fetch 把设备上的文件写进来，校验后写 manifest.json。
// name 是设备给的文件名，例如 dump_info.tar.gz。
//
// 下载失败时删除文件夹；下载完整但格式校验失败时保留文件（便于排查）、不写 manifest，并返回错误。
func (j *Job) Save(log Log, name string, fetch func(w io.Writer) error) (*Result, error) {
	s := j.Store
	start := s.now()
	dir, err := s.deviceDir(j.Device, log, start)
	if err != nil {
		return nil, err
	}
	name = safeFileName(name)
	path := filepath.Join(dir, name)

	size, sum, err := writeFile(path+".part", fetch)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.Rename(path+".part", path); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	j.Report(StageVerify, -1, "检查文件完整性")
	if err := verifyArchive(path, size); err != nil {
		return nil, fmt.Errorf("文件已保存到 %s，但校验没有通过：%w", path, err)
	}

	m := Manifest{
		Schema:      ManifestSchema,
		Tool:        s.Tool,
		CollectedAt: s.now().Format(time.RFC3339),
		Customer:    s.Customer,
		BatchID:     s.batchID(),
		Device: ManifestDevice{
			Type:       firstNonEmpty(j.Device.Type, "server"),
			Vendor:     j.Device.Vendor,
			Model:      j.Device.Model,
			Serial:     j.Device.Serial,
			Hostname:   j.Device.Hostname,
			MgmtIP:     j.Device.Address,
			HostIP:     j.Device.HostIP,
			OS:         j.Device.OS,
			Controller: j.Device.Controller,
		},
		Log:   log,
		Files: []File{{Name: name, Size: size, SHA256: sum, Role: "main"}},
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	return &Result{Dir: dir, Path: path, Manifest: m}, nil
}

// deviceDir 创建本次收集的设备文件夹，重名时加 -2、-3。
func (s *Store) deviceDir(dev Device, log Log, t time.Time) (string, error) {
	parent := s.Dir
	if s.Customer != "" {
		parent = filepath.Join(parent, safePart(s.Customer, 60))
	}
	parent = filepath.Join(parent, t.Format("20060102"))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	kind := log.Type[strings.LastIndex(log.Type, ".")+1:]
	base := strings.Join([]string{
		safePart(dev.Vendor, 20), safePart(dev.Model, 40), safePart(dev.Serial, 40),
		safePart(kind, 20), t.Format("20060102-1504"),
	}, "_")
	for i := 1; ; i++ {
		dir := filepath.Join(parent, base)
		if i > 1 {
			dir = fmt.Sprintf("%s-%d", dir, i)
		}
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) || i >= 100 {
			return "", err
		}
	}
}

func writeFile(path string, fetch func(w io.Writer) error) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	err = fetch(cw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && cw.n == 0 {
		err = errors.New("设备返回的文件是空的")
	}
	if err != nil {
		os.Remove(path)
		return 0, "", err
	}
	return cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// verifyArchive 按扩展名把压缩包完整读一遍，发现截断或损坏。
func verifyArchive(path string, size int64) error {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		z, err := zip.OpenReader(path)
		if err != nil {
			return fmt.Errorf("不是完整的 zip 文件：%w", err)
		}
		defer z.Close()
		for _, f := range z.File {
			rc, err := f.Open()
			if err == nil {
				_, err = io.Copy(io.Discard, rc)
				rc.Close()
			}
			if err != nil {
				return fmt.Errorf("zip 里的 %s 已损坏：%w", f.Name, err)
			}
		}
		return nil
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return readArchive(path, true, true)
	case strings.HasSuffix(lower, ".gz"):
		return readArchive(path, true, false)
	case strings.HasSuffix(lower, ".tar"):
		return readArchive(path, false, true)
	}
	return nil
}

func readArchive(path string, gz, isTar bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if gz {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("不是 gzip 文件：%w", err)
		}
		defer zr.Close()
		r = zr
	}
	if !isTar {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return fmt.Errorf("gzip 数据不完整：%w", err)
		}
		return nil
	}
	tr := tar.NewReader(r)
	n := 0
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err == nil {
			_, err = io.Copy(io.Discard, tr)
		}
		if err != nil {
			return fmt.Errorf("tar 包不完整：%w", err)
		}
		n++
	}
	if n == 0 {
		return errors.New("压缩包里没有文件")
	}
	// tar 的结束标记之后还要读完 gzip 尾部，才能发现截断和校验和错误。
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("gzip 数据不完整：%w", err)
	}
	return nil
}

// safePart 把名称变成可用在 Windows 文件夹名里的一段：去掉空白，替换非法字符和分隔用的下划线。
func safePart(s string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			continue
		case r < 0x20 || strings.ContainsRune(`<>:"/\|?*_`, r):
			r = '-'
		}
		if n >= limit {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.Trim(b.String(), ". -")
}

// safeFileName 只保留设备给的文件名本身，防止路径穿越。
func safeFileName(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`<>:"|?*`, r) {
			r = '-'
		}
		b.WriteRune(r)
	}
	name = strings.Trim(b.String(), ". ")
	if name == "" || strings.EqualFold(name, "manifest.json") {
		name = "log.bin"
	}
	return name
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
