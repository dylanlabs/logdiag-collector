// SPDX-License-Identifier: GPL-3.0-or-later

// Package tasks 是界面背后的任务中心：批量收集日志包、检查连通、登录识别设备。
//
// 规则与命令行一致：同一台设备同时只跑一个收集任务；全局并发数可调；认证失败立即停止、
// 不重试，账号密码没改之前也不再登录；取消只停止本地等待，不中断设备端的生成任务，会话照常删除。
package tasks

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/appdata"
	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// 收集任务的状态。
const (
	StateQueued   = "queued"
	StateRunning  = "running"
	StateDone     = "done"
	StateFailed   = "failed"
	StateCanceled = "canceled"
)

// maxKept 是保留的已结束任务数，更早的连同诊断记录一起丢弃。
const maxKept = 300

// Options 是 New 的参数。
type Options struct {
	Devices  *inventory.Store
	Settings func() appdata.Settings
	// Publish 把变化推给界面：事件名 task（Task）、device（inventory.Device）。可为 nil。
	Publish func(event string, data any)
	Logger  *slog.Logger

	// 以下供测试替换，nil 时用真实实现。
	Collect  func(context.Context, drivers.Request) (*collect.Result, error)
	Identify func(context.Context, drivers.Request) (*drivers.Identity, error)
	Check    func(ctx context.Context, address string, hc *http.Client) (reach, message string)
	Sleep    func(context.Context, time.Duration) error
}

// Task 是一个收集任务，界面直接显示它。
type Task struct {
	ID         string    `json:"id"`
	BatchID    string    `json:"batch_id"`
	DeviceID   string    `json:"device_id"`
	Customer   string    `json:"customer"`
	MgmtIP     string    `json:"mgmt_ip"`
	Vendor     string    `json:"vendor"`
	Model      string    `json:"model"`
	Serial     string    `json:"serial"`
	State      string    `json:"state"`
	Stage      string    `json:"stage"`
	StageIndex int       `json:"stage_index"` // 0 排队 … 6 完成，界面画进度条用
	Percent    int       `json:"percent"`     // 设备给出的生成进度，-1 表示没有
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Kind       string    `json:"kind,omitempty"` // 失败分类，例如“认证失败”
	Hint       string    `json:"hint,omitempty"` // 该怎么做
	Error      string    `json:"error,omitempty"`
	Result     *Result   `json:"result,omitempty"`
	HasRecord  bool      `json:"has_record"`
}

// Result 是收集成功后保存的文件。
type Result struct {
	Dir     string `json:"dir"`
	Path    string `json:"path"`
	File    string `json:"file"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	LogType string `json:"log_type"`
}

type task struct {
	Task
	cancel   context.CancelFunc
	cassette *httprec.Cassette
}

// Skip 是没有开始的设备和原因。
type Skip struct {
	DeviceID string `json:"device_id"`
	Label    string `json:"label"`
	Reason   string `json:"reason"`
}

// Manager 是任务中心，可以被多个 goroutine 同时使用。
type Manager struct {
	o      Options
	ctx    context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup
	checks chan struct{} // 检查和识别的并发上限

	mu      sync.Mutex
	tasks   []*task // 从旧到新
	queue   []*task
	running int
	busy    map[string]string // 设备 ID → 正在进行的检查或识别
}

// New 创建任务中心。
func New(o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Collect == nil {
		o.Collect = drivers.Collect
	}
	if o.Identify == nil {
		o.Identify = drivers.Identify
	}
	if o.Check == nil {
		o.Check = checkReach
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Manager{o: o, ctx: ctx, stop: stop, checks: make(chan struct{}, 16), busy: map[string]string{}}
}

func (m *Manager) publish(event string, data any) {
	if m.o.Publish != nil {
		m.o.Publish(event, data)
	}
}

func (m *Manager) publishDevice(d inventory.Device, ok bool) {
	if ok {
		m.publish("device", d)
	}
}

func label(d inventory.Device) string {
	if d.Customer != "" {
		return d.Customer + " " + d.MgmtIP
	}
	return d.MgmtIP
}

// Tasks 返回全部任务，最新的在前。
func (m *Manager) Tasks() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for i := len(m.tasks) - 1; i >= 0; i-- {
		out = append(out, m.tasks[i].Task)
	}
	return out
}

// Busy 报告是否还有排队或进行中的收集任务（界面窗口关闭后程序据此决定要不要等）。
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running > 0 || len(m.queue) > 0
}

func (m *Manager) collecting(deviceID string) bool {
	for _, t := range m.tasks {
		if t.DeviceID == deviceID && (t.State == StateQueued || t.State == StateRunning) {
			return true
		}
	}
	return false
}

// StartCollect 为每台设备建一个收集任务，同一次调用的任务共用一个批次号（manifest 的 batch_id）。
func (m *Manager) StartCollect(ids []string) (batchID string, started []Task, skipped []Skip) {
	batchID = newUUID()
	now := time.Now()
	m.mu.Lock()
	for _, id := range ids {
		a, err := m.o.Devices.Access(id)
		d := a.Device
		switch {
		case errors.Is(err, inventory.ErrNotFound):
			skipped = append(skipped, Skip{id, id, "设备不存在，可能已被删除"})
			continue
		case err != nil:
			skipped = append(skipped, Skip{id, label(d), err.Error()})
			continue
		case m.collecting(id):
			skipped = append(skipped, Skip{id, label(d), "已在收集中"})
			continue
		case d.Status.Login == inventory.LoginAuthFailed:
			skipped = append(skipped, Skip{id, label(d), "上次登录认证失败。为避免账号被锁定，请先修改账号或密码"})
			continue
		case d.Type != "" && d.Type != inventory.TypeServer:
			skipped = append(skipped, Skip{id, label(d), "目前只支持服务器的一键收集，存储和网络设备会在后续版本支持"})
			continue
		case d.Vendor != "" && !Supported(d.Vendor):
			skipped = append(skipped, Skip{id, label(d), "暂不支持" + d.Vendor + "的一键收集（已支持" + SupportedText() + "）。如果品牌填错了，清空品牌后收集时会自动识别"})
			continue
		}
		t := &task{Task: Task{
			ID: newID(), BatchID: batchID, DeviceID: id, Customer: d.Customer, MgmtIP: d.MgmtIP,
			Vendor: d.Vendor, Model: d.Model, Serial: d.Serial, State: StateQueued,
			Stage: collect.StageQueued.String(), Percent: -1, Message: "等待空闲的收集通道", CreatedAt: now,
		}}
		m.tasks = append(m.tasks, t)
		m.queue = append(m.queue, t)
		started = append(started, t.Task)
	}
	m.trim()
	m.mu.Unlock()
	for _, t := range started {
		m.publish("task", t)
	}
	m.pump()
	return batchID, started, skipped
}

// trim 丢弃最早的已结束任务。
func (m *Manager) trim() {
	for len(m.tasks) > maxKept {
		i := slices.IndexFunc(m.tasks, func(t *task) bool { return t.State != StateQueued && t.State != StateRunning })
		if i < 0 {
			return
		}
		m.tasks = slices.Delete(m.tasks, i, i+1)
	}
}

// pump 在并发数允许的范围内启动排队的任务。并发数每次都读最新设置。
func (m *Manager) pump() {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := max(1, m.o.Settings().Concurrency)
	for m.running < limit && len(m.queue) > 0 && m.ctx.Err() == nil {
		t := m.queue[0]
		m.queue = m.queue[1:]
		m.running++
		ctx, cancel := context.WithCancel(m.ctx)
		t.cancel = cancel
		t.State, t.StartedAt, t.Message = StateRunning, time.Now(), "开始"
		m.wg.Add(1)
		go m.run(ctx, t)
	}
}

// Cancel 取消一个任务。排队中的直接取消；进行中的停止本地等待并退出会话，设备端的生成任务不受影响。
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	t := m.find(id)
	if t == nil {
		m.mu.Unlock()
		return errors.New("任务不存在")
	}
	switch t.State {
	case StateQueued:
		m.queue = slices.DeleteFunc(m.queue, func(q *task) bool { return q == t })
		m.finishLocked(t, context.Canceled)
		snap := t.Task
		m.mu.Unlock()
		m.publish("task", snap)
		return nil
	case StateRunning:
		t.Message = "正在取消，退出会话后结束"
		cancel := t.cancel
		snap := t.Task
		m.mu.Unlock()
		cancel()
		m.publish("task", snap)
		return nil
	}
	m.mu.Unlock()
	return nil
}

// CancelAll 取消所有排队和进行中的任务。
func (m *Manager) CancelAll() {
	m.mu.Lock()
	var ids []string
	for _, t := range m.tasks {
		if t.State == StateQueued || t.State == StateRunning {
			ids = append(ids, t.ID)
		}
	}
	m.mu.Unlock()
	// 先取消排队的，免得进行中的任务结束时又启动新的。
	slices.Reverse(ids)
	for _, id := range ids {
		m.Cancel(id)
	}
}

// ClearFinished 从列表里去掉已结束的任务（连同诊断记录）。
func (m *Manager) ClearFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks = slices.DeleteFunc(m.tasks, func(t *task) bool { return t.State != StateQueued && t.State != StateRunning })
}

func (m *Manager) find(id string) *task {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Record 返回任务的诊断记录（已去掉密码、令牌和设备 IP）。
func (m *Manager) Record(id string) (*httprec.Cassette, Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.find(id)
	switch {
	case t == nil:
		return nil, Task{}, errors.New("任务不存在")
	case t.cassette == nil:
		return nil, t.Task, errors.New("这个任务没有诊断记录：任务还没结束，或设置里关闭了“保留诊断记录”")
	}
	return t.cassette, t.Task, nil
}

func (m *Manager) run(ctx context.Context, t *task) {
	defer m.wg.Done()
	settings := m.o.Settings()
	ctx, cancelTimeout := context.WithTimeout(ctx, time.Duration(settings.TimeoutMinutes)*time.Minute)
	defer cancelTimeout()
	m.publish("task", m.snapshot(t))

	res, rec, err := m.collect(ctx, t, settings)

	m.mu.Lock()
	if rec != nil {
		c := rec.Cassette()
		c.Note = "真机录制：LogDiag Collector 界面收集"
		c.Tool = buildinfo.Get().String()
		c.RecordedAt = time.Now().Format(time.RFC3339)
		t.cassette = c
		t.HasRecord = true
	}
	if err == nil {
		f := res.Manifest.Files[0]
		t.Result = &Result{Dir: res.Dir, Path: res.Path, File: f.Name, Size: f.Size, SHA256: f.SHA256, LogType: res.Manifest.Log.Type}
		t.Vendor, t.Model, t.Serial = res.Manifest.Device.Vendor, res.Manifest.Device.Model, res.Manifest.Device.Serial
	}
	m.finishLocked(t, err)
	m.running--
	snap := t.Task
	m.mu.Unlock()

	m.afterCollect(snap, res, err)
	m.publish("task", snap)
	m.pump()
}

func (m *Manager) snapshot(t *task) Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return t.Task
}

// collect 执行一次收集。返回的录制器在出错时也有内容，便于导出诊断记录。
func (m *Manager) collect(ctx context.Context, t *task, settings appdata.Settings) (*collect.Result, *httprec.Recorder, error) {
	a, err := m.o.Devices.Access(t.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	hc := redfish.NewHTTPClient(redfish.TransportOptions{Legacy: a.Device.LegacyTLS})
	var rec *httprec.Recorder
	if settings.Record {
		rec = httprec.NewRecorder(hc.Transport)
		hc.Transport = rec
	}
	info := buildinfo.Get()
	m.o.Logger.Info("开始收集", "设备", t.MgmtIP, "客户", t.Customer)
	res, err := m.o.Collect(ctx, drivers.Request{
		Address:    a.Device.MgmtIP,
		User:       a.User,
		Password:   a.Password,
		Brand:      brandHint(a.Device.Vendor),
		AcceptEULA: settings.AcceptEULA,
		HTTPClient: hc,
		UserAgent:  buildinfo.UserAgent(),
		Store: &collect.Store{
			Dir:      settings.OutputDir,
			Customer: a.Device.Customer,
			BatchID:  t.BatchID,
			Tool:     collect.Tool{Name: info.Product, Version: info.Version},
		},
		Progress: func(e collect.Event) { m.progress(t, e) },
		Sleep:    m.o.Sleep,
	})
	return res, rec, err
}

func (m *Manager) progress(t *task, e collect.Event) {
	m.mu.Lock()
	if t.State != StateRunning {
		m.mu.Unlock()
		return
	}
	changed := t.StageIndex != int(e.Stage) || t.Percent != e.Percent || t.Message != e.Message
	t.Stage, t.StageIndex, t.Percent, t.Message = e.Stage.String(), int(e.Stage), e.Percent, e.Message
	snap := t.Task
	m.mu.Unlock()
	if changed {
		m.publish("task", snap)
		m.o.Logger.Debug("收集进度", "设备", snap.MgmtIP, "阶段", snap.Stage, "进度", snap.Percent, "说明", snap.Message)
	}
}

// finishLocked 根据错误设置任务的最终状态。调用方持有 m.mu。
func (m *Manager) finishLocked(t *task, err error) {
	t.FinishedAt = time.Now()
	t.Percent = -1
	switch {
	case err == nil:
		t.State, t.Stage, t.StageIndex = StateDone, collect.StageDone.String(), int(collect.StageDone)
		t.Message = "已保存"
	case errors.Is(err, context.Canceled):
		t.State, t.Kind, t.Hint = StateCanceled, redfish.KindCanceled.String(), redfish.KindCanceled.Hint()
		t.Message = "已取消"
	default:
		kind := redfish.KindOf(err)
		t.State, t.Kind, t.Hint, t.Error = StateFailed, kind.String(), kind.Hint(), err.Error()
		switch {
		case redfish.HintOf(err) != "":
			t.Hint = redfish.HintOf(err) // 驱动给的专门建议，例如 Dell 要先接受许可协议
		case kind == redfish.KindUnsupported:
			t.Hint = "该品牌或固件暂不支持一键收集。已支持：" + SupportedText() + "。可导出诊断记录发给开发者。"
		case kind == redfish.KindTimeout:
			t.Hint = "设备生成日志较慢，可在设置里调大单台超时后重试。"
		}
		t.Message = t.Kind
	}
}

// afterCollect 把结果写回设备库：最后收集、登录状态，以及从设备读到的型号和序列号。
func (m *Manager) afterCollect(t Task, res *collect.Result, err error) {
	id := t.DeviceID
	now := time.Now()
	switch {
	case err == nil:
		md := res.Manifest.Device
		m.o.Devices.SetIdentity(id, inventory.Identity{Vendor: md.Vendor, Model: md.Model, Serial: md.Serial, Hostname: md.Hostname, Firmware: md.Controller.Firmware})
		m.o.Devices.SetStatus(id, func(s *inventory.Status) {
			s.Reach, s.ReachMessage, s.CheckedAt = inventory.ReachService, "", now
			s.Login, s.LoginMessage, s.LoginAt = inventory.LoginOK, "", now
		})
		d, ok := m.o.Devices.SetLastCollect(id, inventory.LastCollect{At: now, OK: true, Message: "已保存 " + t.Result.File, Dir: res.Dir})
		m.publishDevice(d, ok)
		m.o.Logger.Info("收集完成", "设备", t.MgmtIP, "文件", res.Path)
	case errors.Is(err, context.Canceled):
		m.o.Logger.Info("收集已取消", "设备", t.MgmtIP)
	default:
		kind := redfish.KindOf(err)
		if kind == redfish.KindAuth {
			m.o.Devices.SetStatus(id, func(s *inventory.Status) {
				s.Login, s.LoginMessage, s.LoginAt = inventory.LoginAuthFailed, err.Error(), now
			})
		}
		d, ok := m.o.Devices.SetLastCollect(id, inventory.LastCollect{At: now, OK: false, Message: kind.String() + "：" + err.Error()})
		m.publishDevice(d, ok)
		m.o.Logger.Warn("收集失败", "设备", t.MgmtIP, "原因", kind.String(), "错误", err)
	}
}

// Shutdown 取消所有任务和检查，等它们退出会话后返回（最多 timeout）。
func (m *Manager) Shutdown(timeout time.Duration) {
	m.mu.Lock()
	for _, t := range m.queue {
		m.finishLocked(t, context.Canceled)
	}
	m.queue = nil
	m.mu.Unlock()
	m.stop()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		m.o.Logger.Warn("部分任务没有及时退出会话")
	}
}

// brandHint 只在品牌是浪潮时指定驱动：关闭了 Redfish 的浪潮设备认不出品牌，只能直接走网页接口。
// 其他品牌一律自动识别，免得手填的品牌（例如把超聚变写成华为）让 manifest 的日志类型出错。
func brandHint(vendor string) string {
	if b := brandOf(vendor); b == "inspur" {
		return b
	}
	return ""
}

// brands 是已支持一键收集的品牌：驱动键、中文名和设备表里常见的写法。
var brands = []struct {
	key, name string
	aliases   []string
}{
	{"huawei", "华为", []string{"huawei", "华为"}},
	{"xfusion", "超聚变", []string{"xfusion", "超聚变"}},
	{"inspur", "浪潮", []string{"inspur", "浪潮", "ieit"}},
	{"lenovo", "联想", []string{"lenovo", "联想"}},
	{"dell", "Dell", []string{"dell", "戴尔"}},
	{"hpe", "HPE", []string{"hpe", "hewlett", "慧与", "惠普", "hp"}}, // 设备表里常把 HPE 写成 HP 或惠普
}

// brandOf 把设备表里的品牌写法对应到驱动键，认不出时为空。
func brandOf(vendor string) string {
	v := strings.ToLower(strings.TrimSpace(vendor))
	if v == "" {
		return ""
	}
	for _, b := range brands {
		for _, a := range b.aliases {
			if strings.Contains(v, a) {
				if _, ok := drivers.BrandByKey(b.key); ok {
					return b.key
				}
			}
		}
	}
	return ""
}

// SupportedText 返回已支持品牌的中文名，例如“华为、超聚变、浪潮、联想、Dell、HPE”。
func SupportedText() string {
	var names []string
	for _, b := range brands {
		if _, ok := drivers.BrandByKey(b.key); ok {
			names = append(names, b.name)
		}
	}
	return strings.Join(names, "、")
}

// Supported 报告设备表里的品牌写法是否已支持一键收集；品牌为空时返回 false（收集时会自动识别）。
func Supported(vendor string) bool { return brandOf(vendor) != "" }
