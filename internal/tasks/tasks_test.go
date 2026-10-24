// SPDX-License-Identifier: GPL-3.0-or-later

package tasks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/appdata"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
	"github.com/dylanlabs/logdiag-collector/internal/secret"
)

func ptr[T any](v T) *T { return &v }

type env struct {
	m      *Manager
	inv    *inventory.Store
	set    appdata.Settings
	mu     sync.Mutex
	events []string
}

func newEnv(t *testing.T, o Options) *env {
	t.Helper()
	dir := t.TempDir()
	box, _ := secret.New(dir)
	inv, _, err := inventory.Open(dir, box)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{inv: inv, set: appdata.Defaults()}
	e.set.OutputDir = t.TempDir()
	e.set.Concurrency = 2
	o.Devices = inv
	o.Settings = func() appdata.Settings { return e.set }
	o.Publish = func(name string, data any) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if task, ok := data.(Task); ok {
			e.events = append(e.events, name+":"+task.State)
		} else {
			e.events = append(e.events, name)
		}
	}
	e.m = New(o)
	t.Cleanup(func() { e.m.Shutdown(5 * time.Second); inv.Close() })
	return e
}

func (e *env) add(t *testing.T, ip, vendor string) string {
	t.Helper()
	d, err := e.inv.Add(inventory.Patch{Customer: ptr("示例客户"), MgmtIP: ptr(ip), Vendor: ptr(vendor), User: ptr("admin"), Password: ptr("pw")})
	if err != nil {
		t.Fatal(err)
	}
	return d.ID
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) states() map[string]string {
	out := map[string]string{}
	for _, t := range e.m.Tasks() {
		out[t.MgmtIP] = t.State
	}
	return out
}

func fakeResult(req drivers.Request) *collect.Result {
	return &collect.Result{Dir: req.Store.Dir + "/dev", Path: req.Store.Dir + "/dev/dump_info.tar.gz", Manifest: collect.Manifest{
		Device: collect.ManifestDevice{Vendor: "Huawei", Model: "2288H V5", Serial: "SN-" + req.Address, Controller: collect.Controller{Firmware: "3.08"}},
		Log:    collect.Log{Type: "huawei.ibmc.dump"},
		Files:  []collect.File{{Name: "dump_info.tar.gz", Size: 10, SHA256: "ab"}},
	}}
}

func TestCollectConcurrencyAndResults(t *testing.T) {
	var running, peak atomic.Int32
	release := make(chan struct{})
	var batches sync.Map
	e := newEnv(t, Options{Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		batches.Store(req.Store.BatchID, true)
		req.Progress(collect.Event{Stage: collect.StageGenerate, Percent: 40, Message: "生成中"})
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if req.Address == "10.0.0.3" {
			return nil, fmt.Errorf("登录：%w", &redfish.HTTPError{Method: "POST", Path: "/redfish/v1/SessionService/Sessions", StatusCode: 401})
		}
		if req.User != "admin" || req.Password != "pw" || req.Brand != "" {
			return nil, fmt.Errorf("参数不对：%+v", req)
		}
		return fakeResult(req), nil
	}})
	a := e.add(t, "10.0.0.1", "")
	b := e.add(t, "10.0.0.2", "Huawei")
	c := e.add(t, "10.0.0.3", "")
	noPw, _ := e.inv.Add(inventory.Patch{MgmtIP: ptr("10.0.0.4"), User: ptr("admin")})

	_, started, skipped := e.m.StartCollect([]string{a, b, c, noPw.ID})
	if len(started) != 3 || len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "密码") {
		t.Fatalf("started %d, skipped %+v", len(started), skipped)
	}
	if _, again, skipped := e.m.StartCollect([]string{a}); len(again) != 0 || skipped[0].Reason != "已在收集中" {
		t.Errorf("同一台设备不应同时有两个任务：%+v", skipped)
	}
	waitFor(t, "两个任务开始", func() bool { return running.Load() == 2 })
	if !e.m.Busy() {
		t.Error("有任务时 Busy 应为 true")
	}
	close(release)
	waitFor(t, "全部结束", func() bool { return !e.m.Busy() })

	if peak.Load() != 2 {
		t.Errorf("并发峰值 %d，应为设置的 2", peak.Load())
	}
	n := 0
	batches.Range(func(any, any) bool { n++; return true })
	if n != 1 {
		t.Errorf("同一次收集应共用一个批次号，实际 %d 个", n)
	}
	st := e.states()
	if st["10.0.0.1"] != StateDone || st["10.0.0.2"] != StateDone || st["10.0.0.3"] != StateFailed {
		t.Errorf("任务状态：%v", st)
	}
	for _, task := range e.m.Tasks() {
		if task.MgmtIP == "10.0.0.3" && (task.Kind != "认证失败" || task.Hint == "") {
			t.Errorf("失败任务：%+v", task)
		}
		if task.MgmtIP == "10.0.0.1" && (task.Result == nil || task.Serial != "SN-10.0.0.1" || !task.HasRecord) {
			t.Errorf("成功任务：%+v", task)
		}
	}
	d, _ := e.inv.Get(a)
	if d.Model != "2288H V5" || d.Serial != "SN-10.0.0.1" || d.LastCollect == nil || !d.LastCollect.OK || d.Status.Login != inventory.LoginOK {
		t.Errorf("成功后应回填设备：%+v", d)
	}
	d, _ = e.inv.Get(c)
	if d.Status.Login != inventory.LoginAuthFailed || d.LastCollect.OK {
		t.Errorf("认证失败应记到设备上：%+v", d)
	}
	// 认证失败后账号密码没改，不再登录。
	if _, started, skipped := e.m.StartCollect([]string{c}); len(started) != 0 || !strings.Contains(skipped[0].Reason, "认证失败") {
		t.Errorf("认证失败的设备不应再次收集：%+v", skipped)
	}
	if _, _, err := e.m.Record(started[0].ID); err != nil {
		t.Errorf("应保留诊断记录：%v", err)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, Options{Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("等待设备：%w", ctx.Err())
	}})
	e.set.Concurrency = 1
	a := e.add(t, "10.0.0.1", "")
	b := e.add(t, "10.0.0.2", "")
	_, started, _ := e.m.StartCollect([]string{a, b})
	waitFor(t, "第一个开始", func() bool { return e.states()["10.0.0.1"] == StateRunning })
	if e.states()["10.0.0.2"] != StateQueued {
		t.Fatal("并发 1 时第二个应排队")
	}
	e.m.Cancel(started[1].ID)
	if e.states()["10.0.0.2"] != StateCanceled {
		t.Error("排队的任务应立即取消")
	}
	e.m.CancelAll()
	waitFor(t, "取消完成", func() bool { return e.states()["10.0.0.1"] == StateCanceled })
	if d, _ := e.inv.Get(a); d.LastCollect != nil {
		t.Error("取消不算一次收集结果")
	}
	e.m.ClearFinished()
	if len(e.m.Tasks()) != 0 {
		t.Error("清除后不应还有已结束的任务")
	}
}

func TestSkipUnsupported(t *testing.T) {
	e := newEnv(t, Options{Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
		return fakeResult(req), nil
	}})
	h3c := e.add(t, "10.0.0.1", "H3C")
	storage, _ := e.inv.Add(inventory.Patch{Type: ptr("storage"), MgmtIP: ptr("10.0.0.2"), User: ptr("admin"), Password: ptr("pw")})
	_, started, skipped := e.m.StartCollect([]string{h3c, storage.ID})
	if len(started) != 0 || len(skipped) != 2 || !strings.Contains(skipped[0].Reason, "暂不支持H3C") || !strings.Contains(skipped[1].Reason, "只支持服务器") {
		t.Fatalf("未支持的品牌和类型应跳过：started %d, skipped %+v", len(started), skipped)
	}
}

func TestInspurBrandHint(t *testing.T) {
	got := make(chan string, 2)
	e := newEnv(t, Options{Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
		got <- req.Brand
		return fakeResult(req), nil
	}})
	e.m.StartCollect([]string{e.add(t, "10.0.0.1", "浪潮"), e.add(t, "10.0.0.2", "华为")})
	a, b := <-got, <-got
	if a+b != "inspur" {
		t.Errorf("只有浪潮指定驱动，其他自动识别：%q %q", a, b)
	}
	if !Supported("XFusion") || Supported("H3C") || SupportedText() != "华为、超聚变、浪潮、联想、Dell、HPE" {
		t.Error("品牌支持判断不对")
	}
	for _, v := range []string{"Dell Inc.", "戴尔", "Lenovo", "联想", "HPE", "HP", "惠普", "Hewlett Packard Enterprise"} {
		if !Supported(v) {
			t.Errorf("%s 应已支持", v)
		}
	}
}

// TestDriverHint 检查驱动给的专门建议（Dell 未接受许可协议）原样显示，并且“代为接受”跟随设置。
func TestDriverHint(t *testing.T) {
	for _, allow := range []bool{false, true} {
		accept := make(chan bool, 1)
		e := newEnv(t, Options{Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
			accept <- req.AcceptEULA
			return nil, redfish.WithHint(errors.New("SRV085"), redfish.KindConsent, "先接受许可协议")
		}})
		e.set.AcceptEULA = allow
		e.m.StartCollect([]string{e.add(t, "10.0.0.1", "Dell")})
		if got := <-accept; got != allow {
			t.Errorf("设置为 %v 时 AcceptEULA = %v", allow, got)
		}
		waitFor(t, "任务失败", func() bool { return e.states()["10.0.0.1"] == StateFailed })
		if task := e.m.Tasks()[0]; task.Kind != "需要确认" || task.Hint != "先接受许可协议" {
			t.Errorf("Kind = %q, Hint = %q", task.Kind, task.Hint)
		}
	}
}

func TestCheckAndIdentify(t *testing.T) {
	e := newEnv(t, Options{
		Check: func(ctx context.Context, address string, hc *http.Client) (string, string) {
			if address == "10.0.0.2" {
				return inventory.ReachUnreachable, "连不上 443 端口"
			}
			return inventory.ReachService, "Redfish 1.0.2"
		},
		Identify: func(ctx context.Context, req drivers.Request) (*drivers.Identity, error) {
			if req.Address == "10.0.0.2" {
				return nil, fmt.Errorf("登录：%w", &redfish.HTTPError{StatusCode: 401})
			}
			b, _ := drivers.BrandByKey("xfusion")
			return &drivers.Identity{Brand: b, Manufacturer: "xFusion", Model: "2288H V6", Serial: "SN1", Hostname: "db01", Firmware: "5.01"}, nil
		},
	})
	a := e.add(t, "10.0.0.1", "")
	b := e.add(t, "10.0.0.2", "")
	if n := e.m.Check([]string{a, b}); n != 2 {
		t.Fatalf("Check 开始了 %d 台", n)
	}
	waitFor(t, "检查完成", func() bool {
		da, _ := e.inv.Get(a)
		db, _ := e.inv.Get(b)
		return da.Status.Reach == inventory.ReachService && db.Status.Reach == inventory.ReachUnreachable
	})
	n, skipped := e.m.Identify([]string{a, b})
	if n != 2 || len(skipped) != 0 {
		t.Fatalf("Identify = %d, %+v", n, skipped)
	}
	waitFor(t, "识别完成", func() bool {
		da, _ := e.inv.Get(a)
		db, _ := e.inv.Get(b)
		return da.Status.Login == inventory.LoginOK && db.Status.Login == inventory.LoginAuthFailed
	})
	if d, _ := e.inv.Get(a); d.Vendor != "xFusion" || d.Model != "2288H V6" || d.Hostname != "db01" || d.Firmware != "5.01" {
		t.Errorf("识别结果没有回填：%+v", d)
	}
	if _, skipped := e.m.Identify([]string{b}); len(skipped) != 1 {
		t.Error("认证失败后账号密码没改，不应再次登录")
	}
	e.inv.Update([]string{b}, inventory.Patch{Password: ptr("new")})
	if n, _ := e.m.Identify([]string{b}); n != 1 {
		t.Error("改密码后应能再次识别")
	}
}

func TestCheckReach(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redfish/v1/":
			w.Write([]byte(`{"RedfishVersion":"1.6.0","Vendor":"Huawei","Product":"2288H V5"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	hc := redfish.NewHTTPClient(redfish.TransportOptions{})
	addr := strings.TrimPrefix(srv.URL, "https://")
	if reach, msg := checkReach(context.Background(), addr, hc); reach != inventory.ReachService || msg != "Redfish 1.6.0，Huawei 2288H V5" {
		t.Errorf("checkReach = %s %s", reach, msg)
	}

	plain := httptest.NewServer(http.NotFoundHandler()) // 端口开着，但不是 HTTPS
	defer plain.Close()
	if reach, _ := checkReach(context.Background(), strings.TrimPrefix(plain.URL, "http://"), hc); reach != inventory.ReachPort {
		t.Errorf("不是 HTTPS 的端口应为 %s，得到 %s", inventory.ReachPort, reach)
	}
	if reach, _ := checkReach(context.Background(), plain.URL, hc); reach != inventory.ReachPort {
		t.Errorf("http:// 地址没有 Redfish 应为 %s，得到 %s", inventory.ReachPort, reach)
	}
	ln := plain.Listener.Addr().String()
	plain.Close()
	if reach, msg := checkReach(context.Background(), ln, hc); reach != inventory.ReachUnreachable || !strings.Contains(msg, "连不上") {
		t.Errorf("端口关闭应为不可达：%s %s", reach, msg)
	}
}
