// SPDX-License-Identifier: GPL-3.0-or-later

package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/huawei"
	"github.com/dylanlabs/logdiag-collector/internal/collect/inspur"
	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// capture 记下驱动实际发出的请求，回放器本身只按方法和路径匹配。
type capture struct {
	next http.RoundTripper
	mu   sync.Mutex
	reqs []sent
}

type sent struct {
	method, path string
	header       http.Header
	body         []byte
}

func (c *capture) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, sent{req.Method, req.URL.Path, req.Header.Clone(), body})
	c.mu.Unlock()
	return c.next.RoundTrip(req)
}

func (c *capture) find(method, path string) []sent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sent
	for _, r := range c.reqs {
		if r.method == method && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

type replay struct {
	req    Request
	cap    *capture
	waits  []time.Duration
	events []collect.Event
	dir    string
}

// newReplay 用 testdata 里的录制记录代替设备，等待只记录不真的等。
func newReplay(t *testing.T, name string) *replay {
	t.Helper()
	return newReplayFrom(t, loadCassette(t, name))
}

func loadCassette(t *testing.T, name string) *httprec.Cassette {
	t.Helper()
	c, err := httprec.Load(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newReplayFrom 用（测试里改过的）录制记录代替设备。
func newReplayFrom(t *testing.T, c *httprec.Cassette) *replay {
	t.Helper()
	p := httprec.NewReplayer(c)
	t.Cleanup(func() {
		if left := p.Remaining(); len(left) > 0 {
			t.Errorf("录制记录里还有没走到的请求：%v", left)
		}
		if missed := p.Missed(); len(missed) > 0 {
			t.Errorf("录制记录里没有的请求：%v", missed)
		}
	})
	r := &replay{cap: &capture{next: p}, dir: t.TempDir()}
	var mu sync.Mutex
	r.req = Request{
		Address:    "bmc.replay.test",
		User:       "admin",
		Password:   "s3cret!",
		HTTPClient: redfish.NewHTTPClient(redfish.TransportOptions{}),
		UserAgent:  "LogDiagCollector/test",
		Store: &collect.Store{
			Dir:      r.dir,
			Customer: "示例客户",
			BatchID:  "4b0e2c1e-7d1a-4f0e-9f57-2f8a3c9d1b20",
			Tool:     collect.Tool{Name: "LogDiag Collector", Version: "0.0.0-test"},
			Now:      func() time.Time { return time.Date(2026, 10, 26, 14, 3, 11, 0, time.FixedZone("CST", 8*3600)) },
		},
		Progress: func(e collect.Event) {
			mu.Lock()
			r.events = append(r.events, e)
			mu.Unlock()
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			r.waits = append(r.waits, d)
			mu.Unlock()
			return ctx.Err()
		},
	}
	r.req.HTTPClient.Transport = r.cap
	return r
}

func (r *replay) stages() []collect.Stage {
	var out []collect.Stage
	for _, e := range r.events {
		if len(out) == 0 || out[len(out)-1] != e.Stage {
			out = append(out, e.Stage)
		}
	}
	return out
}

func readManifest(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHuaweiDump(t *testing.T) {
	r := newReplay(t, "huawei_dump.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	wantDir := filepath.Join(r.dir, "示例客户", "20261026", "Huawei_2288HV5_2102311SYNTH0001_dump_20261026-1403")
	if res.Dir != wantDir {
		t.Errorf("Dir = %q，应为 %q", res.Dir, wantDir)
	}
	if filepath.Base(res.Path) != "dump_info.tar.gz" {
		t.Errorf("Path = %q", res.Path)
	}
	m := res.Manifest
	if m.Log != (collect.Log{Type: "huawei.ibmc.dump", Method: "redfish-oem"}) {
		t.Errorf("Log = %+v", m.Log)
	}
	if m.Device.Vendor != "Huawei" || m.Device.Model != "2288H V5" || m.Device.Serial != "2102311SYNTH0001" ||
		m.Device.Hostname != "db01" || m.Device.MgmtIP != "bmc.replay.test" ||
		m.Device.Controller != (collect.Controller{Type: "iBMC", Firmware: "3.08"}) {
		t.Errorf("Device = %+v", m.Device)
	}
	if f := m.Files[0]; f.SHA256 != "b8c2494616e024183deb15eb32b4ab1d935a11c231fbabb3527d667403b2ed70" || f.Size != 172 || f.Role != "main" {
		t.Errorf("Files = %+v", m.Files)
	}
	onDisk := readManifest(t, res.Dir)
	if onDisk["schema"] != "logdiag.manifest/1" || onDisk["customer"] != "示例客户" || onDisk["collected_at"] != "2026-10-26T14:03:11+08:00" {
		t.Errorf("manifest.json = %v", onDisk)
	}

	// 参数与华为官方工具一致。
	dump := r.cap.find("POST", "/redfish/v1/Managers/1/Actions/Oem/Huawei/Manager.Dump")
	if len(dump) != 1 || string(dump[0].body) != `{"Content":"/tmp/web/dump_info.tar.gz","Type":"URI"}` {
		t.Errorf("Manager.Dump 请求体 = %q", dump)
	}
	dl := r.cap.find("POST", "/redfish/v1/Managers/1/Actions/Oem/Huawei/Manager.GeneralDownload")
	if len(dl) != 1 || string(dl[0].body) != `{"Path":"/tmp/web/dump_info.tar.gz","TransferProtocol":"HTTPS"}` {
		t.Errorf("Manager.GeneralDownload 请求体 = %q", dl)
	}
	if len(r.cap.find("DELETE", "/redfish/v1/SessionService/Sessions/a1b2c3")) != 1 {
		t.Error("结束后应删除会话")
	}

	want := []collect.Stage{collect.StageProbe, collect.StageTrigger, collect.StageGenerate,
		collect.StageDownload, collect.StageVerify, collect.StageDone}
	if got := r.stages(); !slices.Equal(got, want) {
		t.Errorf("阶段 = %v，应为 %v", got, want)
	}
	// 进度来自 Oem.Huawei.TaskPercentage。
	if !slices.ContainsFunc(r.events, func(e collect.Event) bool { return e.Stage == collect.StageGenerate && e.Percent == 40 }) {
		t.Errorf("没有报告 40%% 的进度：%v", r.events)
	}
}

func TestXFusionTaskFailed(t *testing.T) {
	r := newReplay(t, "xfusion_dump_failed.json")
	_, err := Collect(context.Background(), r.req)
	if redfish.KindOf(err) != redfish.KindTaskFailed {
		t.Fatalf("err = %v（%s），应为设备端任务失败", err, redfish.KindOf(err))
	}
	if !strings.Contains(err.Error(), "Collecting information failed.") {
		t.Errorf("错误里应带设备给的消息：%v", err)
	}
	if len(r.cap.find("POST", "/redfish/v1/Managers/1/Actions/Oem/xFusion/Manager.Dump")) != 1 {
		t.Error("应按设备实际的 OEM 键（xFusion）调用一键收集")
	}
	if len(r.cap.find("DELETE", "/redfish/v1/SessionService/Sessions/x9y8z7")) != 1 {
		t.Error("失败时也应删除会话")
	}
	entries, _ := os.ReadDir(filepath.Join(r.dir, "示例客户", "20261026"))
	if len(entries) != 0 {
		t.Errorf("失败时不应留下设备文件夹：%v", entries)
	}
}

func TestInspurM6(t *testing.T) {
	r := newReplay(t, "inspur_m6.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if filepath.Base(res.Dir) != "Inspur_NF5280M6_SYNTH21A0001_onekeylog_20261026-1403" || filepath.Base(res.Path) != "onekeylog.tar.gz" {
		t.Errorf("Path = %q", res.Path)
	}
	m := res.Manifest
	if m.Log != (collect.Log{Type: "inspur.bmc.onekeylog", Method: "bmc-web-api"}) ||
		m.Device.Controller != (collect.Controller{Type: "BMC", Firmware: "4.02.00"}) {
		t.Errorf("Manifest = %+v", m)
	}
	if m.Files[0].SHA256 != "db192a6de68c2701b6ce9251b50b0440b6ee8c7c40b08323eb937c75027d37e2" {
		t.Errorf("SHA256 = %s", m.Files[0].SHA256)
	}

	// 识别用的 Redfish 会话在走网页接口之前就删掉。
	order := r.cap.reqs
	iRF := slices.IndexFunc(order, func(s sent) bool { return s.method == "DELETE" && strings.HasPrefix(s.path, "/redfish/") })
	iWeb := slices.IndexFunc(order, func(s sent) bool { return s.method == "POST" && s.path == "/api/session" })
	if iRF < 0 || iWeb < 0 || iRF > iWeb {
		t.Errorf("应先删除 Redfish 会话再登录网页接口（%d, %d）", iRF, iWeb)
	}
	login := order[iWeb]
	if got := string(login.body); got != "password=s3cret%21&username=admin" {
		t.Errorf("网页登录请求体 = %q", got)
	}
	// 登录后的请求都带 CSRF 令牌。
	for _, s := range order[iWeb+1:] {
		if s.header.Get("X-CSRFToken") == "" {
			t.Errorf("%s %s 没有带 X-CSRFToken", s.method, s.path)
		}
	}
	if len(r.cap.find("DELETE", "/api/session")) != 1 {
		t.Error("结束后应退出网页会话")
	}
	// 触发后每 5 秒查一次进度，完成后再等 3 秒下载。
	want := []time.Duration{inspur.PollInterval, inspur.PollInterval, 3 * time.Second}
	if !slices.Equal(r.waits, want) {
		t.Errorf("等待 = %v，应为 %v", r.waits, want)
	}
}

func TestInspurM5(t *testing.T) {
	r := newReplay(t, "inspur_m5.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if filepath.Base(res.Path) != "onekeylog.tar" || res.Manifest.Files[0].Size != 10240 {
		t.Errorf("Path = %q, Files = %+v", res.Path, res.Manifest.Files)
	}
	var total time.Duration
	for _, d := range r.waits {
		total += d
	}
	if total != inspur.M5Wait {
		t.Errorf("M5 应固定等待 %s，实际 %s（%v）", inspur.M5Wait, total, r.waits)
	}
}

func TestInspurM5Check(t *testing.T) {
	r := newReplay(t, "inspur_m5_check.json")
	res, err := Collect(context.Background(), r.req)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if filepath.Base(res.Path) != "onekeylog_SYNTH19C0003.tar" {
		t.Errorf("Path = %q", res.Path)
	}
	if want := []time.Duration{inspur.CheckInterval, inspur.CheckInterval}; !slices.Equal(r.waits, want) {
		t.Errorf("等待 = %v，应为 %v", r.waits, want)
	}
	if len(r.cap.find("GET", "/api/logs/onekeylog")) != 0 {
		t.Error("4.34 起的固件不应调用老的 onekeylog 接口")
	}
}

func TestInspurAuthFailedStops(t *testing.T) {
	r := newReplay(t, "inspur_auth_failed.json")
	_, err := Collect(context.Background(), r.req)
	if redfish.KindOf(err) != redfish.KindAuth {
		t.Fatalf("err = %v（%s），应为认证失败", err, redfish.KindOf(err))
	}
	if n := len(r.cap.find("POST", "/api/session")); n != 1 {
		t.Errorf("认证失败后不应重试登录，实际登录了 %d 次", n)
	}
	if len(r.cap.find("DELETE", "/api/session")) != 0 {
		t.Error("没登录成功就不需要退出")
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name    string
		root    *redfish.ServiceRoot
		system  *redfish.Resource
		manager *redfish.Resource
		want    string
	}{
		{"制造商", nil, &redfish.Resource{Manufacturer: "Huawei"}, nil, "huawei"},
		{"超聚变优先于华为 OEM 键", &redfish.ServiceRoot{Oem: map[string]json.RawMessage{"Huawei": nil}},
			&redfish.Resource{Manufacturer: "xFusion"}, nil, "xfusion"},
		{"服务根 Vendor", &redfish.ServiceRoot{Vendor: "Inspur"}, &redfish.Resource{}, nil, "inspur"},
		{"浪潮信息", nil, &redfish.Resource{Manufacturer: "IEIT SYSTEMS"}, nil, "inspur"},
		{"管理器 OEM 键", &redfish.ServiceRoot{}, &redfish.Resource{}, &redfish.Resource{Oem: map[string]json.RawMessage{"xFusion": nil}}, "xfusion"},
		{"Dell", nil, &redfish.Resource{Manufacturer: "Dell Inc."}, nil, "dell"},
		{"联想服务根", &redfish.ServiceRoot{Vendor: "Lenovo"}, &redfish.Resource{}, nil, "lenovo"},
		{"HPE", nil, &redfish.Resource{Manufacturer: "HPE"}, nil, "hpe"},
		{"Gen9 制造商 HP", nil, &redfish.Resource{Manufacturer: "HP"}, nil, "hpe"},
		{"Gen9 OEM 键 Hp", &redfish.ServiceRoot{}, &redfish.Resource{}, &redfish.Resource{Oem: map[string]json.RawMessage{"Hp": nil}}, "hpe"},
		{"HP 要完整单词", nil, &redfish.Resource{Manufacturer: "CHPC Systems"}, nil, ""},
		{"认不出", &redfish.ServiceRoot{Vendor: "Contoso"}, &redfish.Resource{Manufacturer: "Contoso"}, nil, ""},
	}
	for _, tc := range cases {
		b, ok := Detect(tc.root, tc.system, tc.manager)
		if b.Key != tc.want || ok != (tc.want != "") {
			t.Errorf("%s：Detect = %q, %v，应为 %q", tc.name, b.Key, ok, tc.want)
		}
	}
	if b, _ := BrandByKey("XFusion"); b.Driver != (huawei.Driver{}) || b.Vendor != "xFusion" {
		t.Errorf("BrandByKey(XFusion) = %+v", b)
	}
}

// TestIdentify 复用华为收集记录的识别部分：登录、读管理器和系统、退出会话，不碰收集接口。
func TestIdentify(t *testing.T) {
	c, err := httprec.Load(filepath.Join("testdata", "huawei_dump.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.Interactions = slices.DeleteFunc(c.Interactions, func(it *httprec.Interaction) bool {
		return strings.Contains(it.Request.Path, "/Actions/") || strings.Contains(it.Request.Path, "/TaskService/")
	})
	p := httprec.NewReplayer(c)
	hc := redfish.NewHTTPClient(redfish.TransportOptions{})
	hc.Transport = p
	id, err := Identify(context.Background(), Request{Address: "bmc.replay.test", User: "admin", Password: "s3cret!", HTTPClient: hc})
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if id.Brand.Key != "huawei" || id.Vendor() != "Huawei" || id.Model != "2288H V5" || id.Serial != "2102311SYNTH0001" ||
		id.Hostname != "db01" || id.Firmware != "3.08" {
		t.Errorf("Identity = %+v", id)
	}
	if left := p.Remaining(); len(left) > 0 {
		t.Errorf("没有退出会话或漏了请求：%v", left)
	}
	if missed := p.Missed(); len(missed) > 0 {
		t.Errorf("发出了记录里没有的请求：%v", missed)
	}
}

func TestIdentifyAuthFailed(t *testing.T) {
	p, err := httprec.LoadReplayer(filepath.Join("..", "redfish", "testdata", "session_unauthorized.json"))
	if err != nil {
		t.Fatal(err)
	}
	cap := &capture{next: p}
	hc := redfish.NewHTTPClient(redfish.TransportOptions{})
	hc.Transport = cap
	_, err = Identify(context.Background(), Request{Address: "bmc.replay.test", User: "admin", Password: "wrong", HTTPClient: hc})
	if redfish.KindOf(err) != redfish.KindAuth {
		t.Fatalf("err = %v，应为认证失败", err)
	}
	if n := len(cap.find("POST", "/redfish/v1/SessionService/Sessions")); n != 1 {
		t.Errorf("认证失败后不应重试登录，实际登录了 %d 次", n)
	}
}
