// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func tarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	data := []byte("hello")
	tw.WriteHeader(&tar.Header{Name: "dump/a.txt", Mode: 0o644, Size: int64(len(data))})
	tw.Write(data)
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func newJob(t *testing.T) *Job {
	t.Helper()
	return &Job{
		Device: Device{Vendor: "Huawei", Model: "2288H V5", Serial: "2102311ABC", Address: "10.0.0.10",
			Controller: Controller{Type: "iBMC", Firmware: "3.08"}},
		Store: &Store{
			Dir:  t.TempDir(),
			Tool: Tool{Name: "LogDiag Collector", Version: "0.1.0"},
			Now:  func() time.Time { return time.Date(2026, 12, 20, 14, 3, 11, 0, time.FixedZone("CST", 8*3600)) },
		},
	}
}

var dumpLog = Log{Type: "huawei.ibmc.dump", Method: "redfish-oem"}

func TestSave(t *testing.T) {
	j := newJob(t)
	data := tarGz(t)
	res, err := j.Save(dumpLog, "dump_info.tar.gz", func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(j.Store.Dir, "20261220", "Huawei_2288HV5_2102311ABC_dump_20261220-1403")
	if res.Dir != wantDir || res.Path != filepath.Join(wantDir, "dump_info.tar.gz") {
		t.Errorf("Dir = %q, Path = %q", res.Dir, res.Path)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, data) {
		t.Error("文件内容不对")
	}
	if _, err := os.Stat(res.Path + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Error("临时文件应已改名")
	}

	var m Manifest
	raw, err := os.ReadFile(filepath.Join(res.Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != ManifestSchema || m.CollectedAt != "2026-12-20T14:03:11+08:00" || m.Device.Type != "server" ||
		m.Device.MgmtIP != "10.0.0.10" || m.Files[0].Size != int64(len(data)) || len(m.Files[0].SHA256) != 64 {
		t.Errorf("manifest = %+v", m)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(m.BatchID) {
		t.Errorf("BatchID = %q", m.BatchID)
	}

	// 同一分钟再收一次：文件夹加序号，批次号不变。
	res2, err := j.Save(dumpLog, "dump_info.tar.gz", func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Dir != wantDir+"-2" || res2.Manifest.BatchID != m.BatchID {
		t.Errorf("第二次 Dir = %q, BatchID = %q", res2.Dir, res2.Manifest.BatchID)
	}
}

func TestSaveCustomerFolder(t *testing.T) {
	j := newJob(t)
	j.Store.Customer = "示例/客户"
	res, err := j.Save(dumpLog, "dump_info.tar.gz", func(w io.Writer) error {
		_, err := w.Write(tarGz(t))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if rel, _ := filepath.Rel(j.Store.Dir, res.Dir); !strings.HasPrefix(rel, filepath.Join("示例-客户", "20261220")) {
		t.Errorf("Dir = %q", rel)
	}
}

func TestSaveDownloadFailed(t *testing.T) {
	j := newJob(t)
	_, err := j.Save(dumpLog, "dump_info.tar.gz", func(w io.Writer) error {
		w.Write([]byte("partial"))
		return errors.New("连接断开")
	})
	if err == nil || !strings.Contains(err.Error(), "连接断开") {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(j.Store.Dir, "20261220"))
	if len(entries) != 0 {
		t.Errorf("下载失败时应删除设备文件夹：%v", entries)
	}
	_, err = j.Save(dumpLog, "dump_info.tar.gz", func(io.Writer) error { return nil })
	if err == nil {
		t.Error("空文件应报错")
	}
}

// 下载完整但格式不对：保留文件便于排查，不写 manifest。
func TestSaveCorruptArchive(t *testing.T) {
	j := newJob(t)
	data := tarGz(t)
	_, err := j.Save(dumpLog, "dump_info.tar.gz", func(w io.Writer) error {
		_, err := w.Write(data[:len(data)-10])
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "校验没有通过") {
		t.Fatalf("err = %v", err)
	}
	dir := filepath.Join(j.Store.Dir, "20261220", "Huawei_2288HV5_2102311ABC_dump_20261220-1403")
	if _, err := os.Stat(filepath.Join(dir, "dump_info.tar.gz")); err != nil {
		t.Error("应保留文件")
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("校验失败时不应写 manifest")
	}
}

func TestVerifyArchive(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, data, 0o644)
		return p
	}
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("tsr/a.xml")
	f.Write([]byte("<a/>"))
	zw.Close()

	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"a.tar.gz", tarGz(t), true},
		{"a.zip", zbuf.Bytes(), true},
		{"a.zip", zbuf.Bytes()[:zbuf.Len()-5], false},
		{"a.tar", []byte("not a tar file at all"), false},
		{"a.tar.gz", []byte("plain text"), false},
		{"a.tzz", []byte("unknown format is accepted"), true},
	}
	for _, tc := range cases {
		p := write(tc.name, tc.data)
		if err := verifyArchive(p, int64(len(tc.data))); (err == nil) != tc.ok {
			t.Errorf("%s（%d 字节）：err = %v", tc.name, len(tc.data), err)
		}
	}
}

func TestSafeNames(t *testing.T) {
	for in, want := range map[string]string{
		"2288H V5":     "2288HV5",
		"NF5280M6":     "NF5280M6",
		`a/b\c:d*e?_f`: "a-b-c-d-e--f",
		" . ":          "",
	} {
		if got := safePart(in, 40); got != want {
			t.Errorf("safePart(%q) = %q，应为 %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"dump_info.tar.gz":      "dump_info.tar.gz",
		"../../etc/passwd":      "passwd",
		`C:\Windows\x.tar`:      "x.tar",
		"/tmp/onekeylog_SN.tar": "onekeylog_SN.tar",
		"manifest.json":         "log.bin",
		"..":                    "log.bin",
		"a<b>.tar":              "a-b-.tar",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// docs/schema 里的 JSON Schema 是和网站共用的契约，必填字段要和代码一致。
func TestManifestMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "schema", "manifest-1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Const    string   `json:"const"`
			Required []string `json:"required"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema 不是合法 JSON：%v", err)
	}
	if schema.Properties["schema"].Const != ManifestSchema {
		t.Errorf("schema 版本 = %q", schema.Properties["schema"].Const)
	}

	j := newJob(t)
	res, err := j.Save(dumpLog, "dump_info.tar.gz", func(w io.Writer) error {
		_, err := w.Write(tarGz(t))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(res.Manifest)
	var m map[string]json.RawMessage
	json.Unmarshal(data, &m)
	for _, k := range schema.Required {
		if _, ok := m[k]; !ok {
			t.Errorf("manifest 缺少必填字段 %s", k)
		}
	}
	for _, obj := range []string{"device", "log", "redaction"} {
		var sub map[string]json.RawMessage
		json.Unmarshal(m[obj], &sub)
		for _, k := range schema.Properties[obj].Required {
			if _, ok := sub[k]; !ok {
				t.Errorf("manifest.%s 缺少必填字段 %s", obj, k)
			}
		}
	}
}
