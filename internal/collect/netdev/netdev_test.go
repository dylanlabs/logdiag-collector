// SPDX-License-Identifier: GPL-3.0-or-later

package netdev

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/netsim"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// fast 是测试用的等待时间：模拟设备在本机，几十毫秒足够。
var fast = Timing{Quiet: 30 * time.Millisecond, BigQuiet: 80 * time.Millisecond, Login: 2 * time.Second,
	Command: 3 * time.Second, Big: 5 * time.Second, Progress: 20 * time.Millisecond}

func startSim(t *testing.T, kind string, mod func(*netsim.Device)) (*netsim.Server, *netsim.Device) {
	t.Helper()
	d, err := netsim.New(kind)
	if err != nil {
		t.Fatal(err)
	}
	d.Pause = 50 * time.Millisecond
	if mod != nil {
		mod(d)
	}
	srv, err := netsim.Listen("127.0.0.1:0", d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, d
}

func request(t *testing.T, srv *netsim.Server, d *netsim.Device) Request {
	return Request{
		Address: srv.Addr, User: d.User, Password: d.Password, Enable: d.Enable,
		Store: &collect.Store{
			Dir:     t.TempDir(),
			BatchID: "4b0e2c1e-7d1a-4f0e-9f57-2f8a3c9d1b20",
			Tool:    collect.Tool{Name: "LogDiag Collector", Version: "0.0.0-test"},
			Now:     func() time.Time { return time.Date(2026, 10, 26, 14, 3, 11, 0, time.FixedZone("CST", 8*3600)) },
		},
		Recorder: NewRecorder(),
		Timing:   fast,
	}
}

type want struct {
	vendor, model, serial, os, version, logType, hostname string
	files                                                 []string
}

var wants = map[string]want{
	"ios":    {"Cisco", "WS-C2960X-48TS-L", "FOCSYNTH005", "IOS", "15.2(7)E4", "cisco.ios.showtech", "SW-ACCESS-01", []string{"show-tech-support.txt", "show-logging.txt"}},
	"iosxe":  {"Cisco", "C9300-48P", "FOCSYNTH001", "IOS-XE", "17.03.04", "cisco.ios.showtech", "SW-CORE-01", []string{"show-tech-support.txt", "show-logging.txt"}},
	"nxos":   {"Cisco", "N9K-C93180YC-EX", "FDOSYNTH002", "NX-OS", "9.3(8)", "cisco.nxos.showtech", "N9K-LEAF-01", []string{"show-tech-support.txt", "show-logging-logfile.txt"}},
	"mds":    {"Cisco", "DS-C9148S-K9", "JAFSYNTH003", "NX-OS", "8.4(2c)", "cisco.mds.showtech", "MDS-FAB-A", []string{"show-tech-support-details.txt", "show-logging-logfile.txt"}},
	"asa":    {"Cisco", "ASA5516", "JADSYNTH004", "ASA", "9.12(4)37", "cisco.asa.showtech", "ASA-EDGE-01", []string{"show-tech-support.txt", "show-logging.txt"}},
	"huawei": {"Huawei", "CE6850-48S6Q-HI", "2102350DLS10SYNTH001", "VRP", "V200R005C10SPC800", "huawei.vrp.diag", "HUAWEI-CE-01", []string{"display-diagnostic-information.txt", "display-logbuffer.txt", "display-trapbuffer.txt"}},
	"h3c":    {"H3C", "S6850-56HF", "210235A2FSHSYNTH0001", "Comware", "7.1.070 Release 6728P22", "h3c.comware.diag", "H3C-S6850-01", []string{"display-diagnostic-information.txt", "display-logbuffer.txt"}},
}

func TestCollectSimulated(t *testing.T) {
	for _, kind := range netsim.Kinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			srv, d := startSim(t, kind, nil)
			req := request(t, srv, d)
			var events []collect.Event
			req.Progress = func(e collect.Event) { events = append(events, e) }
			res, err := Collect(context.Background(), req)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			w := wants[kind]
			m := res.Manifest
			got := []string{m.Device.Type, m.Device.Vendor, m.Device.Model, m.Device.Serial, m.Device.Controller.Type, m.Device.Controller.Firmware, m.Log.Type, m.Log.Method, m.Device.Hostname, m.Device.MgmtIP}
			exp := []string{"network", w.vendor, w.model, w.serial, w.os, w.version, w.logType, "ssh", w.hostname, "127.0.0.1"}
			if strings.Join(got, "|") != strings.Join(exp, "|") {
				t.Errorf("manifest 设备信息 = %q\n应为 %q", got, exp)
			}
			if !m.Redaction.Applied || strings.Join(m.Redaction.Rules, ",") != "password,auth-key,snmp-community,ciphertext" {
				t.Errorf("redaction = %+v", m.Redaction)
			}
			var names []string
			for i, f := range m.Files {
				names = append(names, f.Name)
				role := map[bool]string{true: "main", false: "extra"}[i == 0]
				if f.Role != role || f.Size == 0 || len(f.SHA256) != 64 {
					t.Errorf("文件 %+v", f)
				}
			}
			if strings.Join(names, ",") != strings.Join(w.files, ",") {
				t.Errorf("文件 = %v，应为 %v", names, w.files)
			}
			if res.SSHHostKey != srv.HostKey {
				t.Errorf("SSHHostKey = %q，应为 %q", res.SSHHostKey, srv.HostKey)
			}
			if srv.Failures() != 0 {
				t.Errorf("设备记了 %d 次密码错误", srv.Failures())
			}

			main := readFile(t, res.Path)
			lines := strings.Split(strings.TrimSuffix(main, "\n"), "\n")
			prompt := map[bool]string{true: "<" + d.Hostname + ">", false: d.Hostname + "#"}[kind == "huawei" || kind == "h3c"]
			diag := map[string]string{"mds": "show tech-support details", "huawei": "display diagnostic-information", "h3c": "display diagnostic-information"}[kind]
			if lines[0] != prompt+cmp.Or(diag, "show tech-support") {
				t.Errorf("第一行 = %q", lines[0])
			}
			if lines[len(lines)-1] != prompt {
				t.Errorf("最后一行 = %q，应为提示符 %q", lines[len(lines)-1], prompt)
			}
			for _, s := range []string{"Collecting data, please wait ... done", Mask, w.serial} {
				if !strings.Contains(main, s) {
					t.Errorf("主文件里没有 %q", s)
				}
			}
			for _, f := range m.Files {
				text := readFile(t, filepath.Join(res.Dir, f.Name))
				for _, bad := range []string{"S3cr3t", "More", "\r", "\x1b", "\b"} {
					if strings.Contains(text, bad) {
						t.Errorf("%s 里有 %q：%s", f.Name, bad, around(text, bad))
					}
				}
			}
			if !strings.Contains(stagesOf(events), "探测 触发 下载 校验") {
				t.Errorf("进度阶段 = %s", stagesOf(events))
			}

			// 诊断记录：没有密码、口令和设备地址。
			data, _ := json.Marshal(req.Recorder.Record())
			for _, bad := range []string{d.Password, "Enable@123", "S3cr3t", "127.0.0.1"} {
				if strings.Contains(string(data), bad) {
					t.Errorf("诊断记录里有 %q", bad)
				}
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func around(text, s string) string {
	i := strings.Index(text, s)
	return text[max(0, i-60):min(len(text), i+60)]
}

func stagesOf(events []collect.Event) string {
	var out []string
	for _, e := range events {
		if s := e.Stage.String(); len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

func TestIdentifySimulated(t *testing.T) {
	for _, kind := range netsim.Kinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			srv, d := startSim(t, kind, nil)
			id, err := Identify(context.Background(), request(t, srv, d))
			if err != nil {
				t.Fatalf("Identify: %v", err)
			}
			w := wants[kind]
			got := []string{id.Vendor, id.Model, id.Serial, id.OS, id.Version, id.LogType, id.Hostname}
			exp := []string{w.vendor, w.model, w.serial, w.os, w.version, w.logType, w.hostname}
			if strings.Join(got, "|") != strings.Join(exp, "|") {
				t.Errorf("Identify = %q\n应为 %q", got, exp)
			}
			for _, c := range srv.Commands() {
				if strings.Contains(c, "tech") || strings.Contains(c, "diagnostic") {
					t.Errorf("识别时不应执行 %q", c)
				}
			}
		})
	}
}

// 关闭分页的命令被拒绝时，靠自动翻页也能收全，而且一页都不少。
func TestPagingFallback(t *testing.T) {
	for _, kind := range []string{"iosxe", "huawei", "asa"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ref, d := startSim(t, kind, nil)
			full, err := Collect(context.Background(), request(t, ref, d))
			if err != nil {
				t.Fatal(err)
			}
			srv, d := startSim(t, kind, func(d *netsim.Device) { d.KeepPaging, d.PageLines = true, 10 })
			res, err := Collect(context.Background(), request(t, srv, d))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if a, b := readFile(t, full.Path), readFile(t, res.Path); a != b {
				t.Errorf("自动翻页收到的内容和关闭分页时不一样：%d 字节 / %d 字节", len(b), len(a))
			}
		})
	}
}

func TestAuthFailures(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		mod   func(*netsim.Device)
		req   func(*Request)
		kind2 redfish.Kind
		fails int
	}{
		{"SSH 密码错误", "iosxe", nil, func(r *Request) { r.Password = "wrong-pass" }, redfish.KindAuth, 1},
		{"enable 密码错误", "ios", nil, func(r *Request) { r.Enable = "wrong-enable" }, redfish.KindAuth, 1},
		{"ASA enable 密码错误", "asa", nil, func(r *Request) { r.Enable = "wrong-enable" }, redfish.KindAuth, 1},
		{"没有 enable 密码", "ios", nil, func(r *Request) { r.Enable = "" }, redfish.KindPermission, 0},
		{"命令行登录密码错误", "iosxe", func(d *netsim.Device) { d.ShellLogin = true }, func(r *Request) { r.Password = "wrong-pass" }, redfish.KindAuth, 1},
		{"强制修改密码", "h3c", func(d *netsim.Device) { d.ForceChange = true }, nil, redfish.KindAuth, 0},
		{"主机密钥不符", "huawei", nil, func(r *Request) { r.HostKey = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }, redfish.KindHostKey, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, d := startSim(t, c.kind, c.mod)
			req := request(t, srv, d)
			if c.req != nil {
				c.req(&req)
			}
			_, err := Collect(context.Background(), req)
			if k := redfish.KindOf(err); k != c.kind2 {
				t.Fatalf("错误 = %v，分类 %v，应为 %v", err, k, c.kind2)
			}
			if n := srv.Failures(); n != c.fails {
				t.Errorf("设备记了 %d 次密码错误，应为 %d", n, c.fails)
			}
			if files := filesUnder(t, req.Store.Dir); len(files) != 0 {
				t.Errorf("失败后不应留下文件：%v", files)
			}
		})
	}
}

// 部分老设备 SSH 层不认证，进入命令行后才问账号密码。
func TestShellLogin(t *testing.T) {
	srv, d := startSim(t, "ios", func(d *netsim.Device) { d.ShellLogin = true })
	id, err := Identify(context.Background(), request(t, srv, d))
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if id.Serial != "FOCSYNTH005" {
		t.Errorf("Serial = %q", id.Serial)
	}
}

// 取消时立即断开，不留下半个文件夹。
func TestCancel(t *testing.T) {
	srv, d := startSim(t, "nxos", func(d *netsim.Device) { d.Pause = 3 * time.Second })
	req := request(t, srv, d)
	ctx, cancel := context.WithCancel(context.Background())
	var canceled time.Time
	var once sync.Once
	req.Progress = func(e collect.Event) {
		if e.Stage == collect.StageTrigger && strings.Contains(e.Message, "tech-support") {
			// 设备输出到一半停顿时取消。
			once.Do(func() { time.AfterFunc(300*time.Millisecond, func() { canceled = time.Now(); cancel() }) })
		}
	}
	_, err := Collect(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，应为已取消", err)
	}
	if d := time.Since(canceled); d > time.Second {
		t.Errorf("取消后过了 %v 才返回", d)
	}
	if files := filesUnder(t, req.Store.Dir); len(files) != 0 {
		t.Errorf("取消后不应留下文件：%v", files)
	}
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func TestReplay(t *testing.T) {
	for _, kind := range []string{"iosxe", "huawei"} {
		t.Run(kind, func(t *testing.T) {
			srv, d := startSim(t, kind, nil)
			req := request(t, srv, d)
			live, err := Collect(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			rec := req.Recorder.Record()

			p := NewReplay(rec)
			req2 := request(t, srv, d)
			req2.Dial, req2.Recorder = p.Dial, nil
			res, err := Collect(context.Background(), req2)
			if err != nil {
				t.Fatalf("回放 Collect: %v（%v）", err, p.Err())
			}
			if left := p.Remaining(); len(left) > 0 {
				t.Errorf("记录里还有没走到的步骤：%q", left)
			}
			if a, b := readFile(t, live.Path), readFile(t, res.Path); a != b {
				t.Errorf("回放保存的内容和录制时不一样")
			}
		})
	}
}
