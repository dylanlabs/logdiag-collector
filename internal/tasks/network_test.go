// SPDX-License-Identifier: GPL-3.0-or-later

package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/netsim"
)

func TestNetworkVendors(t *testing.T) {
	for _, v := range []string{"Cisco Systems", "思科", "HUAWEI", "华为", "H3C", "新华三", "HPE"} {
		if !NetworkSupported(v) {
			t.Errorf("%s 应已支持", v)
		}
	}
	for _, v := range []string{"", "Juniper", "锐捷"} {
		if NetworkSupported(v) {
			t.Errorf("%q 不应算已支持", v)
		}
	}
	if NetworkSupportedText() != "思科、华为、新华三" {
		t.Errorf("NetworkSupportedText = %q", NetworkSupportedText())
	}
}

func TestNetworkSkips(t *testing.T) {
	got := make(chan drivers.Request, 4)
	e := newEnv(t, Options{Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
		got <- req
		return fakeResult(req), nil
	}})
	add := func(typ, ip, vendor string) string {
		d, err := e.inv.Add(inventory.Patch{Type: ptr(typ), MgmtIP: ptr(ip), Vendor: ptr(vendor), User: ptr("admin"), Password: ptr("pw"), Enable: ptr("en")})
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	ids := []string{add("network", "10.0.0.1", "Juniper"), add("storage", "10.0.0.2", ""), add("network", "10.0.0.3", "思科"), add("network", "10.0.0.4", "")}
	_, started, skipped := e.m.StartCollect(ids)
	if len(started) != 2 || len(skipped) != 2 || !strings.Contains(skipped[0].Reason, "暂不支持Juniper网络设备") || !strings.Contains(skipped[1].Reason, "存储会在后续版本支持") {
		t.Fatalf("started %d, skipped %+v", len(started), skipped)
	}
	for range 2 {
		req := <-got
		if req.Type != netdev.DeviceType || req.Enable != "en" || req.HostKey != "" || req.NetRecorder == nil || req.HTTPClient != nil {
			t.Errorf("网络设备的收集参数不对：%+v", req)
		}
	}
}

// fast 是模拟设备用的等待时间，见 netdev 的测试。
var fast = netdev.Timing{Quiet: 30 * time.Millisecond, BigQuiet: 80 * time.Millisecond, Login: 2 * time.Second,
	Command: 3 * time.Second, Big: 5 * time.Second, Progress: 20 * time.Millisecond}

// TestNetworkSimulated 用模拟的 Cisco IOS 交换机走一遍界面上的流程：检查、收集、记下主机密钥、
// enable 密码错误后不再登录、主机密钥不符时不发送密码。
func TestNetworkSimulated(t *testing.T) {
	sim, err := netsim.New("ios")
	if err != nil {
		t.Fatal(err)
	}
	sim.Pause = 50 * time.Millisecond
	srv, err := netsim.Listen("127.0.0.1:0", sim)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	e := newEnv(t, Options{Collect: func(ctx context.Context, req drivers.Request) (*collect.Result, error) {
		req.NetTiming = fast
		return drivers.Collect(ctx, req)
	}})
	d, err := e.inv.Add(inventory.Patch{Type: ptr("交换机"), Customer: ptr("示例客户"), MgmtIP: ptr(srv.Addr),
		User: ptr(sim.User), Password: ptr(sim.Password), Enable: ptr(sim.Enable)})
	if err != nil {
		t.Fatal(err)
	}
	id := d.ID
	device := func() inventory.Device {
		d, _ := e.inv.Get(id)
		return d
	}
	collectOnce := func(want string) Task {
		t.Helper()
		_, started, skipped := e.m.StartCollect([]string{id})
		if len(started) != 1 {
			t.Fatalf("没有开始收集：%+v", skipped)
		}
		waitFor(t, "收集结束", func() bool { return !e.m.Busy() })
		task := e.m.Tasks()[0]
		if task.State != want {
			t.Fatalf("任务状态 %s，应为 %s：%+v", task.State, want, task)
		}
		return task
	}

	e.m.Check([]string{id})
	waitFor(t, "检查结束", func() bool { return device().Status.Reach != inventory.ReachChecking })
	if s := device().Status; s.Reach != inventory.ReachService || !strings.Contains(s.ReachMessage, "SSH-2.0-Cisco") {
		t.Errorf("SSH 检查：%s %s", s.Reach, s.ReachMessage)
	}

	task := collectOnce(StateDone)
	if task.Result == nil || task.Result.LogType != "cisco.ios.showtech" || task.Vendor != "Cisco" || !task.HasRecord {
		t.Fatalf("收集结果：%+v", task)
	}
	if _, err := os.Stat(filepath.Join(task.Result.Dir, "manifest.json")); err != nil {
		t.Error(err)
	}
	dev := device()
	if dev.Vendor != "Cisco" || dev.Serial == "" || !strings.HasPrefix(dev.Firmware, "IOS ") || !strings.HasPrefix(dev.SSHHostKey, "SHA256:") || dev.Status.Login != inventory.LoginOK {
		t.Errorf("收集后应回填设备并记下主机密钥：%+v", dev)
	}
	rec, _, err := e.m.Record(task.ID)
	if r, ok := rec.(*netdev.Record); err != nil || !ok || len(r.Steps) == 0 || r.Note == "" {
		t.Errorf("诊断记录：%T %v", rec, err)
	}

	// 记下的主机密钥核对通过，照常收集。
	collectOnce(StateDone)

	// enable 密码错了：认证失败，账号密码没改之前不再登录。
	e.inv.Update([]string{id}, inventory.Patch{Enable: ptr("wrong")})
	task = collectOnce(StateFailed)
	if task.Kind != "认证失败" || device().Status.Login != inventory.LoginAuthFailed {
		t.Errorf("enable 密码错误：%+v", task)
	}
	if _, started, _ := e.m.StartCollect([]string{id}); len(started) != 0 {
		t.Error("认证失败后不应再次登录")
	}
	e.inv.Update([]string{id}, inventory.Patch{Enable: ptr(sim.Enable)})

	// 主机密钥和记录的不一样：不发送密码，提示用户确认后清除。
	e.inv.Update([]string{id}, inventory.Patch{SSHHostKey: ptr("SHA256:" + strings.Repeat("A", 43))})
	logins, failures := srv.Logins(), srv.Failures()
	task = collectOnce(StateFailed)
	if task.Kind != "主机密钥不符" || !strings.Contains(task.Hint, "清除记录的主机密钥") {
		t.Errorf("主机密钥不符：%+v", task)
	}
	if srv.Logins() != logins || srv.Failures() != failures {
		t.Error("主机密钥不符时不应发送密码")
	}
}
