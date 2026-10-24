// SPDX-License-Identifier: GPL-3.0-or-later

package drivers

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/netsim"
)

var updateNet = flag.Bool("update-net", false, "用 netsim 模拟设备重新生成 testdata 里网络设备的合成记录")

// netCases 是网络设备的回放测试，每种系统一份合成记录。记录由 netsim 模拟设备生成（go test -run TestNetworkReplay
// -update-net），输出是按厂商文档简化的样例，不是真机输出；拿到真机录制后另存一份、note 写明型号和版本。
var netCases = []struct {
	kind, file, logType, vendor, os, note string
}{
	{"ios", "cisco_ios_showtech.netrec.json", "cisco.ios.showtech", "Cisco", "IOS",
		"合成：netsim 模拟的 Cisco Catalyst 2960-X（IOS 15.2），不是真机。命令和输出格式依据 Cisco IOS 命令参考" +
			"（terminal length 0、show version、show inventory、show logging、show tech-support）。账号登录后在普通模式，用 enable 密码进入特权模式。"},
	{"iosxe", "cisco_iosxe_showtech.netrec.json", "cisco.ios.showtech", "Cisco", "IOS-XE",
		"合成：netsim 模拟的 Cisco Catalyst 9300（IOS XE 17.3），不是真机。依据 Cisco IOS XE 命令参考" +
			"（show version、show inventory、show logging、show tech-support）。账号登录后直接在特权模式。"},
	{"nxos", "cisco_nxos_showtech.netrec.json", "cisco.nxos.showtech", "Cisco", "NX-OS",
		"合成：netsim 模拟的 Cisco Nexus 93180YC-EX（NX-OS 9.3），不是真机。依据 Cisco Nexus 9000 NX-OS 命令参考" +
			"（terminal length 0、show version、show inventory、show logging logfile、show tech-support）。"},
	{"mds", "cisco_mds_showtech.netrec.json", "cisco.mds.showtech", "Cisco", "NX-OS",
		"合成：netsim 模拟的 Cisco MDS 9148S（NX-OS 8.4），不是真机。依据 Cisco MDS 9000 NX-OS 命令参考" +
			"（show version、show inventory、show logging logfile、show tech-support details）。"},
	{"asa", "cisco_asa_showtech.netrec.json", "cisco.asa.showtech", "Cisco", "ASA",
		"合成：netsim 模拟的 Cisco ASA 5516-X（ASA 9.12），不是真机。依据 Cisco ASA 命令参考" +
			"（terminal pager 0、show version、show inventory、show tech-support）。账号登录后在普通模式，用 enable 密码进入特权模式。"},
	{"huawei", "huawei_vrp_diag.netrec.json", "huawei.vrp.diag", "Huawei", "VRP",
		"合成：netsim 模拟的华为 CE6850（VRP V200R005），不是真机。依据华为 CloudEngine 交换机命令参考" +
			"（screen-length 0 temporary、display version、display esn、display logbuffer、display trapbuffer、display diagnostic-information）。" +
			"登录后提示修改密码时答 N，诊断信息命令确认继续时答 Y。"},
	{"h3c", "h3c_comware_diag.netrec.json", "h3c.comware.diag", "H3C", "Comware",
		"合成：netsim 模拟的新华三 S6850-56HF（Comware 7.1.070），不是真机。依据新华三 Comware 7 基础配置命令参考" +
			"（screen-length disable、display version、display device manuinfo、display logbuffer、display diagnostic-information）。" +
			"诊断信息命令询问保存还是显示时答 N（显示）。"},
}

// netFast 是回放用的等待时间：记录里的输出是一次给完的，几十毫秒足够判断提示符。
var netFast = netdev.Timing{Quiet: 30 * time.Millisecond, BigQuiet: 80 * time.Millisecond, Login: 2 * time.Second,
	Command: 3 * time.Second, Big: 5 * time.Second, Progress: 20 * time.Millisecond}

func netStore(t *testing.T) *collect.Store {
	return &collect.Store{
		Dir:      t.TempDir(),
		Customer: "示例客户",
		BatchID:  "6c3f1d2a-5b4e-4f7a-9c8d-0e1f2a3b4c5d",
		Tool:     collect.Tool{Name: "LogDiag Collector", Version: "0.0.0-test"},
		Now:      func() time.Time { return time.Date(2026, 10, 26, 14, 3, 11, 0, time.FixedZone("CST", 8*3600)) },
	}
}

// recordNet 用模拟设备收集一次，把命令行会话保存为合成记录。
func recordNet(t *testing.T, kind, path, note string) {
	t.Helper()
	sim, err := netsim.New(kind)
	if err != nil {
		t.Fatal(err)
	}
	sim.Ports, sim.Pause = 8, 50*time.Millisecond // 端口少一些，记录小一些
	srv, err := netsim.Listen("127.0.0.1:0", sim)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rec := netdev.NewRecorder()
	if _, err := Collect(context.Background(), Request{
		Type: netdev.DeviceType, Address: srv.Addr, User: sim.User, Password: sim.Password, Enable: sim.Enable,
		NetRecorder: rec, NetTiming: netFast, Store: netStore(t),
	}); err != nil {
		t.Fatal(err)
	}
	r := rec.Record()
	r.Note, r.Tool, r.RecordedAt = note, "netsim", "2026-10-24T10:00:00+08:00"
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
}

// TestNetworkReplay 按合成记录回放每种系统的整机收集：识别、关分页、进特权模式、诊断信息和日志缓冲区、脱敏、manifest。
func TestNetworkReplay(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range netCases {
		covered[c.logType] = true
		t.Run(c.kind, func(t *testing.T) {
			path := filepath.Join("testdata", c.file)
			if *updateNet {
				recordNet(t, c.kind, path, c.note)
			}
			rec, err := netdev.LoadRecord(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(rec.Note, "合成：") {
				t.Errorf("合成记录的 note 应以“合成：”开头：%q", rec.Note)
			}
			sim, _ := netsim.New(c.kind)
			p := netdev.NewReplay(rec)
			res, err := Collect(context.Background(), Request{
				Type: netdev.DeviceType, Address: "192.0.2.10", User: sim.User, Password: sim.Password, Enable: sim.Enable,
				NetDial: p.Dial, NetTiming: netFast, Store: netStore(t),
			})
			if err != nil {
				t.Fatalf("回放收集：%v（%v）", err, p.Err())
			}
			if left := p.Remaining(); len(left) > 0 {
				t.Errorf("记录里还有没走到的步骤：%q", left)
			}
			m := res.Manifest
			d := m.Device
			if m.Log.Type != c.logType || m.Log.Method != "ssh" || d.Type != "network" || d.Vendor != c.vendor ||
				d.Controller.Type != c.os || d.Controller.Firmware == "" || d.Model == "" || d.Serial == "" || d.MgmtIP != "192.0.2.10" {
				t.Errorf("manifest：%+v %+v", m.Log, d)
			}
			if !m.Redaction.Applied || len(m.Redaction.Rules) == 0 {
				t.Errorf("manifest 应写明脱敏规则：%+v", m.Redaction)
			}
			if len(m.Files) < 2 || m.Files[0].Role != "main" || m.Files[1].Role != "extra" {
				t.Errorf("应有诊断信息和日志缓冲区两类文件：%+v", m.Files)
			}
			for _, f := range m.Files {
				data, err := os.ReadFile(filepath.Join(res.Dir, f.Name))
				if err != nil {
					t.Fatal(err)
				}
				if int64(len(data)) != f.Size || strings.Contains(string(data), "S3cr3t") {
					t.Errorf("%s：大小 %d（manifest %d），模拟设备的口令应已遮盖", f.Name, len(data), f.Size)
				}
			}
		})
	}
	for lt := range netdev.LogTypes {
		if !covered[lt] {
			t.Errorf("日志类型 %s 没有回放测试", lt)
		}
	}
}

// TestNetworkReplayHostKey 检查记录过的主机密钥和设备给出的不一致时，在登录前就停下。
func TestNetworkReplayHostKey(t *testing.T) {
	rec, err := netdev.LoadRecord(filepath.Join("testdata", netCases[0].file))
	if err != nil {
		t.Fatal(err)
	}
	p := netdev.NewReplay(rec)
	_, err = Collect(context.Background(), Request{
		Type: netdev.DeviceType, Address: "192.0.2.10", User: "admin", Password: "Admin@123",
		HostKey: "SHA256:" + strings.Repeat("A", 43), NetDial: p.Dial, NetTiming: netFast, Store: netStore(t),
	})
	if err == nil || !strings.Contains(err.Error(), "主机密钥") {
		t.Fatalf("应报主机密钥不一致：%v", err)
	}
}
