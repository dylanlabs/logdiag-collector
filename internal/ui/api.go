// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/appdata"
	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/results"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
	"github.com/dylanlabs/logdiag-collector/internal/tasks"
	"github.com/dylanlabs/logdiag-collector/internal/update"
	"github.com/dylanlabs/logdiag-collector/internal/xlsx"
)

// App 是界面背后的数据和服务。
type App struct {
	Devices      *inventory.Store
	Settings     *appdata.SettingsStore
	Tasks        *tasks.Manager
	DataDir      string
	SecretMethod string          // 密码的加密方式，显示在设置页
	Warnings     []string        // 启动时发现的问题（例如设备库损坏已备份），界面打开时提示
	Update       *update.Checker // 检查更新；为 nil 时读 logdiag.com，连不上时读 GitHub

	imports sync.Map // 导入预览，键是一次性令牌
}

// maxJSON 是普通请求体的大小上限。
const maxJSON = 4 << 20

type apiHandler func(w http.ResponseWriter, r *http.Request) (any, error)

// userError 是给用户看的错误，原样显示在界面上。
type userError struct {
	status int
	msg    string
}

func (e *userError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &userError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func (s *Server) api(pattern string, h apiHandler) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		v, err := h(w, r)
		if err != nil {
			status := http.StatusBadRequest
			var ue *userError
			if errors.As(err, &ue) {
				status = ue.status
			} else if errors.Is(err, inventory.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		if v != nil {
			writeJSON(w, http.StatusOK, v)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxJSON))
	if err := dec.Decode(v); err != nil {
		return badRequest("请求格式不对：%v", err)
	}
	return nil
}

// attachment 设置下载文件名：中文文件名用 RFC 5987 编码，另给一个英文文件名，供不认识编码写法的程序使用。
func attachment(w http.ResponseWriter, name, ascii, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name})+
		`; filename="`+ascii+`"`)
}

type idsBody struct {
	IDs []string `json:"ids"`
}

func (s *Server) routes(app *App) {
	s.api("GET /api/state", app.state)
	s.api("GET /api/devices", app.listDevices)
	s.api("POST /api/devices", s.publishing(app.addDevice))
	s.api("POST /api/devices/update", s.publishing(app.updateDevices))
	s.api("POST /api/devices/delete", s.publishing(app.deleteDevices))
	s.api("POST /api/devices/batch", s.publishing(app.batchAdd))
	s.api("POST /api/devices/import", app.previewImport)
	s.api("POST /api/devices/import/commit", s.publishing(app.commitImport))
	s.api("GET /api/devices/export", app.exportDevices)
	s.api("GET /api/devices/template", app.template)
	s.api("POST /api/devices/check", app.check)
	s.api("POST /api/devices/identify", app.identify)
	s.api("POST /api/credentials", s.publishing(app.addCredential))
	s.api("POST /api/credentials/{id}", s.publishing(app.updateCredential))
	s.api("POST /api/credentials/{id}/delete", s.publishing(app.deleteCredential))
	s.api("GET /api/tasks", app.listTasks)
	s.api("POST /api/tasks", app.startTasks)
	s.api("POST /api/tasks/cancel-all", app.cancelAll)
	s.api("POST /api/tasks/clear", app.clearTasks)
	s.api("POST /api/tasks/{id}/cancel", app.cancelTask)
	s.api("GET /api/tasks/{id}/record", app.taskRecord)
	s.api("GET /api/results", app.listResults)
	s.api("POST /api/results/package", app.buildPackage)
	s.api("POST /api/open", app.open)
	s.api("POST /api/update/check", app.checkUpdate)
	s.api("GET /api/settings", app.getSettings)
	s.api("POST /api/settings", s.publishing(app.setSettings))
}

// publishing 在修改成功后通知所有窗口重新读取设备列表（另一个窗口可能也开着）。
func (s *Server) publishing(h apiHandler) apiHandler {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		v, err := h(w, r)
		if err == nil {
			s.opts.Hub.Publish("devices", nil)
		}
		return v, err
	}
}

func (a *App) state(w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]any{
		"info":          buildinfo.Get(),
		"settings":      a.Settings.Get(),
		"data_dir":      a.DataDir,
		"devices_file":  a.Devices.Path(),
		"secret_method": a.SecretMethod,
		"warnings":      a.Warnings,
		"supported":     tasks.SupportedText(),
		"supported_net": tasks.NetworkSupportedText(),
	}, nil
}

func (a *App) listDevices(w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]any{
		"devices":     a.Devices.List(),
		"credentials": a.Devices.Credentials(),
		"customers":   a.Devices.Customers(),
	}, nil
}

func (a *App) addDevice(w http.ResponseWriter, r *http.Request) (any, error) {
	var p inventory.Patch
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	return a.Devices.Add(p)
}

func (a *App) updateDevices(w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		IDs   []string        `json:"ids"`
		Patch inventory.Patch `json:"patch"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if len(body.IDs) == 0 {
		return nil, badRequest("没有选择设备")
	}
	updated, err := a.Devices.Update(body.IDs, body.Patch)
	if err != nil {
		return nil, err
	}
	return map[string]any{"devices": updated}, nil
}

func (a *App) deleteDevices(w http.ResponseWriter, r *http.Request) (any, error) {
	var body idsBody
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	n, err := a.Devices.Delete(body.IDs)
	if err != nil {
		return nil, err
	}
	return map[string]int{"deleted": n}, nil
}

func (a *App) batchAdd(w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		Addresses string          `json:"addresses"`
		Patch     inventory.Patch `json:"patch"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	addrs, err := inventory.ExpandAddresses(body.Addresses)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	patches := make([]inventory.Patch, len(addrs))
	for i, addr := range addrs {
		p := body.Patch
		p.MgmtIP = &addr
		patches[i] = p
	}
	added, err := a.Devices.AddMany(patches, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"added": len(added), "skipped": len(addrs) - len(added), "ids": deviceIDs(added)}, nil
}

type pendingImport struct {
	preview *inventory.ImportPreview
	expires time.Time
}

// previewImport 读取上传的表格（multipart 的 file 字段）或粘贴的文字（JSON 的 text 字段），返回预览和一次性令牌。
// 预览里不含密码，确认导入时服务端用令牌取回解析结果，密码不经过界面。
func (a *App) previewImport(w http.ResponseWriter, r *http.Request) (any, error) {
	var data []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.Body = http.MaxBytesReader(w, r.Body, inventory.MaxImportBytes+1<<20)
		f, _, err := r.FormFile("file")
		if err != nil {
			return nil, badRequest("没有收到文件：%v", err)
		}
		defer f.Close()
		if data, err = io.ReadAll(io.LimitReader(f, inventory.MaxImportBytes+1)); err != nil {
			return nil, err
		}
	} else {
		var body struct {
			Text string `json:"text"`
		}
		if err := decode(r, &body); err != nil {
			return nil, err
		}
		data = []byte(body.Text)
	}
	rows, err := inventory.ReadTable(data)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	p, err := a.Devices.PreviewImport(rows)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	token := randomHex(16)
	now := time.Now()
	a.imports.Range(func(k, v any) bool {
		if v.(*pendingImport).expires.Before(now) {
			a.imports.Delete(k)
		}
		return true
	})
	a.imports.Store(token, &pendingImport{p, now.Add(30 * time.Minute)})
	return map[string]any{"token": token, "preview": p, "ready": p.Ready()}, nil
}

func (a *App) commitImport(w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		Token           string `json:"token"`
		DefaultCustomer string `json:"default_customer"`
		CredentialID    string `json:"credential_id"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	v, ok := a.imports.LoadAndDelete(body.Token)
	if !ok || v.(*pendingImport).expires.Before(time.Now()) {
		return nil, badRequest("导入预览已过期，请重新选择文件")
	}
	added, err := a.Devices.CommitImport(v.(*pendingImport).preview, body.DefaultCustomer, body.CredentialID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"added": len(added), "ids": deviceIDs(added)}, nil
}

// deviceIDs 返回新加设备的 ID，界面用来选中它们。
func deviceIDs(ds []inventory.Device) []string {
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	return ids
}

func splitIDs(s string) []string {
	var out []string
	for _, id := range strings.Split(s, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func writeTable(w http.ResponseWriter, format, base, ascii string, rows [][]string, widths []int) error {
	var buf bytes.Buffer
	stamp := time.Now().Format("20060102-1504")
	if format == "csv" {
		if err := inventory.WriteCSV(&buf, rows); err != nil {
			return err
		}
		attachment(w, base+"-"+stamp+".csv", ascii+"-"+stamp+".csv", "text/csv; charset=utf-8")
	} else {
		if err := xlsx.Write(&buf, base, rows, widths); err != nil {
			return err
		}
		attachment(w, base+"-"+stamp+".xlsx", ascii+"-"+stamp+".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func (a *App) exportDevices(w http.ResponseWriter, r *http.Request) (any, error) {
	rows := a.Devices.ExportRows(splitIDs(r.URL.Query().Get("ids")))
	return nil, writeTable(w, r.URL.Query().Get("format"), "设备列表", "devices", rows, inventory.ColumnWidths(false))
}

func (a *App) template(w http.ResponseWriter, r *http.Request) (any, error) {
	return nil, writeTable(w, r.URL.Query().Get("format"), "设备导入模板", "device-import-template", inventory.TemplateRows(), inventory.ColumnWidths(true))
}

func (a *App) check(w http.ResponseWriter, r *http.Request) (any, error) {
	var body idsBody
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	return map[string]int{"started": a.Tasks.Check(body.IDs)}, nil
}

func (a *App) identify(w http.ResponseWriter, r *http.Request) (any, error) {
	var body idsBody
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	n, skipped := a.Tasks.Identify(body.IDs)
	return map[string]any{"started": n, "skipped": nonNil(skipped)}, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (a *App) addCredential(w http.ResponseWriter, r *http.Request) (any, error) {
	var p inventory.CredentialPatch
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	return a.Devices.AddCredential(p)
}

func (a *App) updateCredential(w http.ResponseWriter, r *http.Request) (any, error) {
	var p inventory.CredentialPatch
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	return a.Devices.UpdateCredential(r.PathValue("id"), p)
}

func (a *App) deleteCredential(w http.ResponseWriter, r *http.Request) (any, error) {
	n, err := a.Devices.DeleteCredential(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return map[string]int{"unlinked": n}, nil
}

func (a *App) listTasks(w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]any{"tasks": a.Tasks.Tasks()}, nil
}

func (a *App) startTasks(w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		DeviceIDs []string `json:"device_ids"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if len(body.DeviceIDs) == 0 {
		return nil, badRequest("没有选择设备")
	}
	if err := os.MkdirAll(a.Settings.Get().OutputDir, 0o755); err != nil {
		return nil, badRequest("无法创建保存位置 %s：%v。请在设置里换一个位置", a.Settings.Get().OutputDir, err)
	}
	batch, started, skipped := a.Tasks.StartCollect(body.DeviceIDs)
	return map[string]any{"batch_id": batch, "started": nonNil(started), "skipped": nonNil(skipped)}, nil
}

func (a *App) cancelTask(w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]bool{"ok": true}, a.Tasks.Cancel(r.PathValue("id"))
}

func (a *App) cancelAll(w http.ResponseWriter, r *http.Request) (any, error) {
	a.Tasks.CancelAll()
	return map[string]bool{"ok": true}, nil
}

func (a *App) clearTasks(w http.ResponseWriter, r *http.Request) (any, error) {
	a.Tasks.ClearFinished()
	return map[string]bool{"ok": true}, nil
}

func (a *App) taskRecord(w http.ResponseWriter, r *http.Request) (any, error) {
	rec, t, err := a.Tasks.Record(r.PathValue("id"))
	if err != nil {
		return nil, &userError{http.StatusNotFound, err.Error()}
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	// 文件名不带设备 IP：诊断记录常被附在公开的反馈里。扩展名区分记录格式，回放工具按它选择读法。
	suffix := t.CreatedAt.Format("20060102-150405") + recordExt(rec)
	attachment(w, "诊断记录-"+suffix, "diagnostic-record-"+suffix, "application/json")
	_, err = w.Write(append(data, '\n'))
	return nil, err
}

// recordExt 是诊断记录文件的扩展名：服务器的 HTTP 记录 .json，网络设备的命令行记录 .netrec.json，
// SSH 会话记录 .sshrec.json。
func recordExt(rec any) string {
	switch rec.(type) {
	case *netdev.Record:
		return ".netrec.json"
	case *sshx.Record:
		return ".sshrec.json"
	}
	return ".json"
}

func (a *App) listResults(w http.ResponseWriter, r *http.Request) (any, error) {
	dir := a.Settings.Get().OutputDir
	list, err := results.Scan(dir)
	if err != nil {
		return nil, err
	}
	return map[string]any{"output_dir": dir, "entries": nonNil(list)}, nil
}

func (a *App) buildPackage(w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		Dirs []string `json:"dirs"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	info := buildinfo.Get()
	res, err := results.BuildPackage(a.Settings.Get().OutputDir, body.Dirs, collect.Tool{Name: info.Product, Version: info.Version}, time.Now())
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return res, nil
}

// open 在资源管理器里打开保存位置、数据文件夹，或保存位置里的某个结果；
// target 为 link 时在系统默认浏览器里打开 LogDiag 网站或本项目的 GitHub 页面。
func (a *App) open(w http.ResponseWriter, r *http.Request) (any, error) {
	var body struct {
		Target string `json:"target"` // output、data、path、link
		Path   string `json:"path"`
		URL    string `json:"url"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	out := a.Settings.Get().OutputDir
	var p string
	switch body.Target {
	case "link":
		if !allowedLink(body.URL) {
			return nil, badRequest("只能打开 LogDiag 网站和本项目的 GitHub 页面")
		}
		if err := OpenURL(body.URL); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	case "output":
		if err := os.MkdirAll(out, 0o755); err != nil {
			return nil, badRequest("无法创建保存位置：%v", err)
		}
		p = out
	case "data":
		p = a.DataDir
	case "path":
		p = filepath.Clean(body.Path)
		if !filepath.IsAbs(p) || !results.Within(out, p) {
			return nil, badRequest("只能打开保存位置里的文件夹")
		}
	default:
		return nil, badRequest("未知的打开目标")
	}
	if _, err := os.Stat(p); err != nil {
		return nil, badRequest("找不到 %s，可能已被移动或删除", p)
	}
	if err := OpenFolder(p); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

// checkUpdate 读最新版本信息，只在用户点击“检查更新”时调用。连不上网不是程序的错，返回 502 和原因。
func (a *App) checkUpdate(w http.ResponseWriter, r *http.Request) (any, error) {
	c := a.Update
	if c == nil {
		c = &update.Checker{}
	}
	res, err := c.Check(r.Context(), buildinfo.Version)
	if err != nil {
		return nil, &userError{http.StatusBadGateway, err.Error()}
	}
	return res, nil
}

// allowedLink 限定界面能打开的外部网页（检查更新给出的下载页由 update.PageLink 按同样的规则筛过）。
func allowedLink(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch u.Host {
	case "logdiag.com", "www.logdiag.com":
		return true
	case "github.com":
		return u.Path == "/dylanlabs/logdiag-collector" || strings.HasPrefix(u.Path, "/dylanlabs/logdiag-collector/")
	}
	return false
}

func (a *App) getSettings(w http.ResponseWriter, r *http.Request) (any, error) {
	return map[string]any{"settings": a.Settings.Get(), "defaults": appdata.Defaults()}, nil
}

func (a *App) setSettings(w http.ResponseWriter, r *http.Request) (any, error) {
	v := a.Settings.Get()
	if err := decode(r, &v); err != nil {
		return nil, err
	}
	v.OutputDir = strings.TrimSpace(v.OutputDir)
	saved, err := a.Settings.Set(v)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"settings": saved}, nil
}
