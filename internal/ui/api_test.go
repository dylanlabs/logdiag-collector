// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylanlabs/logdiag-collector/internal/appdata"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/secret"
	"github.com/dylanlabs/logdiag-collector/internal/tasks"
)

// apiClient 是登录过的界面窗口。
type apiClient struct {
	t    *testing.T
	b    *http.Client
	base string
}

func newAPI(t *testing.T) (*apiClient, *App) {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	settings, _, err := appdata.OpenSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	v := settings.Get()
	v.OutputDir = filepath.Join(dir, "out")
	if _, err := settings.Set(v); err != nil {
		t.Fatal(err)
	}
	devices, _, err := inventory.Open(dir, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devices.Close() })
	app := &App{Devices: devices, Settings: settings, DataDir: dir, SecretMethod: box.Method()}
	hub := NewHub()
	app.Tasks = tasks.New(tasks.Options{
		Devices: devices, Settings: settings.Get, Publish: hub.Publish,
		Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
			return nil, context.Canceled
		},
	})
	t.Cleanup(func() { app.Tasks.Shutdown(0) })
	s, _ := startServer(t, Options{Hub: hub, App: app, Busy: app.Tasks.Busy})
	c := &apiClient{t: t, b: browser(t), base: strings.TrimSuffix(s.URL(), "/")}
	get(t, c.b, s.LaunchURL())
	return c, app
}

func (c *apiClient) do(method, path, contentType string, body io.Reader, out any) (int, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, body)
	if method != http.MethodGet {
		req.Header.Set("Origin", c.base)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.b.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s：%v\n%s", method, path, err, data)
		}
	}
	return resp.StatusCode, resp.Header
}

func (c *apiClient) post(path string, in, out any) int {
	c.t.Helper()
	data, _ := json.Marshal(in)
	status, _ := c.do(http.MethodPost, path, "application/json", bytes.NewReader(data), out)
	return status
}

func (c *apiClient) get(path string, out any) int {
	c.t.Helper()
	status, _ := c.do(http.MethodGet, path, "", nil, out)
	return status
}

type deviceList struct {
	Devices     []inventory.Device     `json:"devices"`
	Credentials []inventory.Credential `json:"credentials"`
	Customers   []string               `json:"customers"`
}

func TestDevicesAPI(t *testing.T) {
	c, _ := newAPI(t)

	var d inventory.Device
	if st := c.post("/api/devices", map[string]any{"customer": "示例客户", "mgmt_ip": "https://10.0.0.10/", "user": "admin", "password": "pw"}, &d); st != http.StatusOK {
		t.Fatalf("添加：%d", st)
	}
	if d.MgmtIP != "10.0.0.10" || !d.HasPassword {
		t.Errorf("添加的设备：%+v", d)
	}
	var e struct{ Error string }
	if st := c.post("/api/devices", map[string]any{"customer": "示例客户", "mgmt_ip": "10.0.0.10"}, &e); st != http.StatusBadRequest || !strings.Contains(e.Error, "已有") {
		t.Errorf("重复设备应拒绝：%d %q", st, e.Error)
	}

	var batch struct {
		Added, Skipped int
		IDs            []string
	}
	c.post("/api/devices/batch", map[string]any{"addresses": "10.0.0.10-12", "patch": map[string]any{"customer": "示例客户"}}, &batch)
	if batch.Added != 2 || batch.Skipped != 1 || len(batch.IDs) != 2 {
		t.Errorf("批量添加：%+v", batch)
	}

	var upd struct{ Devices []inventory.Device }
	c.post("/api/devices/update", map[string]any{"ids": batch.IDs, "patch": map[string]any{"location": "A01"}}, &upd)
	if len(upd.Devices) != 2 || upd.Devices[0].Location != "A01" {
		t.Errorf("批量修改：%+v", upd)
	}

	var list deviceList
	c.get("/api/devices", &list)
	if len(list.Devices) != 3 || len(list.Customers) != 1 {
		t.Fatalf("列表：%+v", list)
	}
	raw, _ := json.Marshal(list)
	if bytes.Contains(raw, []byte(`"password"`)) {
		t.Error("设备列表不应返回密码")
	}

	var del struct{ Deleted int }
	c.post("/api/devices/delete", map[string]any{"ids": batch.IDs}, &del)
	if del.Deleted != 2 {
		t.Errorf("删除：%+v", del)
	}
}

func TestImportAndExport(t *testing.T) {
	c, _ := newAPI(t)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "设备.csv")
	fw.Write([]byte("客户名称,管理口IP,用户名,密码\n示例客户,10.0.0.20,admin,secret\n示例客户,不是地址,admin,x\n"))
	mw.Close()
	var preview struct {
		Token   string
		Ready   int
		Preview inventory.ImportPreview
	}
	if st, _ := c.do(http.MethodPost, "/api/devices/import", mw.FormDataContentType(), &body, &preview); st != http.StatusOK {
		t.Fatalf("导入预览：%d", st)
	}
	if preview.Ready != 1 || len(preview.Preview.Rows) != 2 || preview.Preview.Rows[1].Error == "" {
		t.Fatalf("预览：%+v", preview)
	}
	raw, _ := json.Marshal(preview)
	if bytes.Contains(raw, []byte("secret")) {
		t.Error("导入预览不应把密码返回给界面")
	}
	var done struct {
		Added int
		IDs   []string
	}
	c.post("/api/devices/import/commit", map[string]any{"token": preview.Token}, &done)
	if done.Added != 1 || len(done.IDs) != 1 {
		t.Errorf("导入：%+v", done)
	}
	var e struct{ Error string }
	if st := c.post("/api/devices/import/commit", map[string]any{"token": preview.Token}, &e); st != http.StatusBadRequest {
		t.Errorf("预览令牌只能用一次：%d", st)
	}

	status, h := c.do(http.MethodGet, "/api/devices/export?format=csv", "", nil, nil)
	cd := h.Get("Content-Disposition")
	if status != http.StatusOK || !strings.Contains(cd, "filename*=utf-8''") || !strings.Contains(cd, `filename="devices-`) {
		t.Errorf("导出：%d %s", status, cd)
	}
	req, _ := http.NewRequest(http.MethodGet, c.base+"/api/devices/export?format=xlsx", nil)
	resp, err := c.b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.HasPrefix(data, []byte("PK")) {
		t.Error("Excel 导出应是 zip 格式的 .xlsx")
	}
}

func TestCredentialsAPI(t *testing.T) {
	c, _ := newAPI(t)
	var cred inventory.Credential
	c.post("/api/credentials", map[string]any{"name": "华为默认", "user": "Administrator", "password": "pw"}, &cred)
	if cred.ID == "" || !cred.HasPassword {
		t.Fatalf("新建模板：%+v", cred)
	}
	var d inventory.Device
	c.post("/api/devices", map[string]any{"mgmt_ip": "10.0.0.30", "credential_id": cred.ID}, &d)
	var unlinked struct{ Unlinked int }
	c.post("/api/credentials/"+cred.ID+"/delete", nil, &unlinked)
	if unlinked.Unlinked != 1 {
		t.Errorf("删除模板应解除 1 台设备：%+v", unlinked)
	}
}

func TestSettingsAPI(t *testing.T) {
	c, app := newAPI(t)
	var res struct{ Settings appdata.Settings }
	if st := c.post("/api/settings", map[string]any{"concurrency": 4}, &res); st != http.StatusOK || res.Settings.Concurrency != 4 {
		t.Errorf("修改并发数：%d %+v", st, res)
	}
	if res.Settings.OutputDir != app.Settings.Get().OutputDir || !res.Settings.Record {
		t.Errorf("只改提交的项，其余保持：%+v", res.Settings)
	}
	var e struct{ Error string }
	if st := c.post("/api/settings", map[string]any{"output_dir": "relative"}, &e); st != http.StatusBadRequest || e.Error == "" {
		t.Errorf("相对路径应拒绝：%d %q", st, e.Error)
	}
	var state struct {
		Supported    string `json:"supported"`
		SecretMethod string `json:"secret_method"`
	}
	c.get("/api/state", &state)
	if state.Supported == "" || state.SecretMethod == "" {
		t.Errorf("状态：%+v", state)
	}
}

func TestResultsAndOpenAPI(t *testing.T) {
	c, app := newAPI(t)
	var res struct {
		OutputDir string `json:"output_dir"`
		Entries   []any  `json:"entries"`
	}
	if st := c.get("/api/results", &res); st != http.StatusOK || res.Entries == nil || len(res.Entries) != 0 {
		t.Errorf("没有结果时应返回空列表：%d %+v", st, res)
	}
	var e struct{ Error string }
	outside := filepath.Join(app.DataDir, "devices.json")
	if st := c.post("/api/open", map[string]any{"target": "path", "path": outside}, &e); st != http.StatusBadRequest {
		t.Errorf("保存位置以外的路径不应打开：%d", st)
	}
	for _, link := range []string{"http://logdiag.com/", "https://evil.example/", "https://github.com/someone/else", "https://logdiag.com:8443/", "file:///C:/Windows"} {
		if st := c.post("/api/open", map[string]any{"target": "link", "url": link}, &e); st != http.StatusBadRequest {
			t.Errorf("不应打开 %s：%d", link, st)
		}
	}
	if !allowedLink("https://logdiag.com/") || !allowedLink("https://github.com/dylanlabs/logdiag-collector/issues") {
		t.Error("LogDiag 网站和项目页面应能打开")
	}
	if st := c.post("/api/results/package", map[string]any{"dirs": []string{"../x"}}, &e); st != http.StatusBadRequest {
		t.Errorf("打包不存在的结果应失败：%d", st)
	}
	if _, err := os.Stat(app.Settings.Get().OutputDir); err == nil {
		t.Error("只是查看结果，不应创建保存位置")
	}
}

func TestTasksAPI(t *testing.T) {
	c, _ := newAPI(t)
	var d inventory.Device
	c.post("/api/devices", map[string]any{"mgmt_ip": "10.0.0.40", "vendor": "H3C", "user": "root", "password": "pw"}, &d)
	var res struct {
		Started []tasks.Task
		Skipped []tasks.Skip
	}
	c.post("/api/tasks", map[string]any{"device_ids": []string{d.ID}}, &res)
	if len(res.Started) != 0 || len(res.Skipped) != 1 {
		t.Errorf("暂不支持的品牌应跳过：%+v", res)
	}
	var e struct{ Error string }
	if st := c.post("/api/tasks", map[string]any{"device_ids": []string{}}, &e); st != http.StatusBadRequest {
		t.Errorf("没有选择设备应拒绝：%d", st)
	}
	if st := c.get("/api/tasks/nope/record", &e); st != http.StatusNotFound {
		t.Errorf("不存在的任务：%d", st)
	}
}
