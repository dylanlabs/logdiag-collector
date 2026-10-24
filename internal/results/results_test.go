// SPDX-License-Identifier: GPL-3.0-or-later

package results

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
)

var tool = collect.Tool{Name: "LogDiag Collector", Version: "0.0.0-test"}

// collectOne 用真正的 Job.Save 生成一个设备文件夹，和收集时的布局完全一样。
func collectOne(t *testing.T, root, customer, serial string, at time.Time) *collect.Result {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("log line\n")
	tw.WriteHeader(&tar.Header{Name: "dump/log.txt", Mode: 0o644, Size: int64(len(body))})
	tw.Write(body)
	tw.Close()
	gz.Close()
	j := &collect.Job{
		Device: collect.Device{Type: "server", Vendor: "Huawei", Model: "2288H V5", Serial: serial, Address: "10.0.0.10",
			Controller: collect.Controller{Type: "iBMC", Firmware: "3.08"}},
		Store: &collect.Store{Dir: root, Customer: customer, Tool: tool, Now: func() time.Time { return at }},
	}
	res, err := j.Save(collect.Log{Type: "huawei.ibmc.dump", Method: "redfish-oem"}, "dump_info.tar.gz", func(w io.Writer) error {
		_, err := w.Write(buf.Bytes())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	day := time.Date(2026, 10, 26, 14, 3, 0, 0, time.FixedZone("CST", 8*3600))
	collectOne(t, root, "示例客户", "SN1", day)
	newer := collectOne(t, root, "", "SN2", day.Add(time.Hour))
	broken := collectOne(t, root, "示例客户", "SN3", day.Add(-time.Hour))
	os.Remove(broken.Path)
	os.MkdirAll(filepath.Join(root, PackageFolder, "x"), 0o755)
	os.WriteFile(filepath.Join(root, PackageFolder, "x", "manifest.json"), []byte("{}"), 0o644)

	list, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("找到 %d 个结果：%+v", len(list), list)
	}
	if list[0].Path != newer.Dir || list[0].Customer != "" || list[0].Device.Serial != "SN2" || list[0].Problem != "" || list[0].Size == 0 {
		t.Errorf("最新的结果：%+v", list[0])
	}
	if list[2].Device.Serial != "SN3" || list[2].Problem == "" || !list[2].Files[0].Missing {
		t.Errorf("文件缺失的结果应标出问题：%+v", list[2])
	}
	if list, err := Scan(filepath.Join(root, "不存在")); err != nil || len(list) != 0 {
		t.Errorf("保存位置不存在时应返回空列表：%v, %v", list, err)
	}
}

func TestBuildPackage(t *testing.T) {
	root := t.TempDir()
	day := time.Date(2026, 10, 26, 14, 3, 0, 0, time.FixedZone("CST", 8*3600))
	a := collectOne(t, root, "示例客户", "SN1", day)
	b := collectOne(t, root, "示例客户", "SN2", day)
	other := collectOne(t, root, "别的客户", "SN3", day)
	rel := func(r *collect.Result) string { p, _ := filepath.Rel(root, r.Dir); return filepath.ToSlash(p) }

	if _, err := BuildPackage(root, []string{rel(a), rel(other)}, tool, day); err == nil {
		t.Error("不同客户的结果不能打进同一个包")
	}
	if _, err := BuildPackage(root, []string{"../outside"}, tool, day); err == nil {
		t.Error("保存位置以外的路径应拒绝")
	}

	res, err := BuildPackage(root, []string{rel(a), rel(b)}, tool, day)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(res.Path) != filepath.Join(root, PackageFolder) || !strings.HasPrefix(filepath.Base(res.Path), "LogDiag-示例客户-20261026-1403-") || res.Devices != 2 {
		t.Errorf("包：%+v", res)
	}
	z, err := zip.OpenReader(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	names := map[string]bool{}
	var pkg Package
	for _, f := range z.File {
		names[f.Name] = true
		if f.Name == "package.json" {
			rc, _ := f.Open()
			json.NewDecoder(rc).Decode(&pkg)
			rc.Close()
		}
	}
	for _, want := range []string{"package.json", filepath.Base(a.Dir) + "/manifest.json", filepath.Base(b.Dir) + "/dump_info.tar.gz"} {
		if !names[want] {
			t.Errorf("包里缺少 %s：%v", want, names)
		}
	}
	if pkg.Schema != PackageSchema || pkg.Customer != "示例客户" || len(pkg.Devices) != 2 || len(pkg.BatchIDs) != 2 {
		t.Errorf("package.json：%+v", pkg)
	}
	if f := pkg.Devices[0].Files; len(f) != 2 || f[0].Role != "manifest" || f[1].SHA256 != a.Manifest.Files[0].SHA256 {
		t.Errorf("package.json 的文件清单：%+v", f)
	}
	if list, _ := Scan(root); len(list) != 3 {
		t.Errorf("离线上传包文件夹不应被当成结果：%d", len(list))
	}

	// 日志包被改过：SHA-256 对不上时停止，不留下半成品。
	os.WriteFile(a.Path, []byte("tampered"), 0o644)
	os.Truncate(a.Path, a.Manifest.Files[0].Size) // 大小不变，内容变了
	if _, err := BuildPackage(root, []string{rel(a)}, tool, day.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("内容被改过应报错：%v", err)
	}
	parts, _ := filepath.Glob(filepath.Join(root, PackageFolder, "*.part"))
	if len(parts) != 0 {
		t.Errorf("失败后留下了 %v", parts)
	}
}
