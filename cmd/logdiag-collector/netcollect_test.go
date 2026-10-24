// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/netsim"
)

func TestCollectNetworkFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--type", "network", "--brand", "huawei"}, "不用 --brand"},
		{[]string{"--type", "storage"}, "--type 只能是"},
		{[]string{"--enable"}, "只用于网络设备"},
	} {
		args := append([]string{"collect", "--host", "10.0.0.10", "--user", "admin"}, c.args...)
		if code, _, errOut := runCmd(t, args...); code != 2 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v：code=%d err=%q", c.args, code, errOut)
		}
	}
}

// TestCollectNetwork 用模拟的 Cisco IOS 交换机走一遍命令行收集：密码和 enable 密码从输入读，
// 保存诊断记录，首次连接显示主机密钥指纹，再用 --host-key 核对。
func TestCollectNetwork(t *testing.T) {
	netTiming = netdev.Timing{Quiet: 30 * time.Millisecond, BigQuiet: 80 * time.Millisecond, Login: 2 * time.Second,
		Command: 3 * time.Second, Big: 5 * time.Second, Progress: 20 * time.Millisecond}
	t.Cleanup(func() { netTiming = netdev.Timing{} })
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

	dir := t.TempDir()
	recPath := filepath.Join(dir, "diag.netrec.json")
	var out, errb bytes.Buffer
	stdin := strings.NewReader(sim.Password + "\n" + sim.Enable + "\n")
	code := run([]string{"collect", "--type", "network", "--host", srv.Addr, "--user", sim.User, "--enable",
		"--out", dir, "--customer", "示例客户", "--record", recPath}, stdin, &out, &errb)
	if code != 0 {
		t.Fatalf("code=%d\n%s\n%s", code, out.String(), errb.String())
	}
	for _, want := range []string{"识别为 Cisco", "show-tech-support.txt", "附带：show-logging.txt", "SSH 主机密钥指纹：SHA256:", "诊断记录已保存"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出里缺少 %q：\n%s", want, out.String())
		}
	}
	data, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), sim.Password) || strings.Contains(string(data), sim.Enable) || strings.Contains(string(data), "127.0.0.1") {
		t.Error("诊断记录里不应有密码和设备地址")
	}

	// 记下的指纹核对通过。
	_, fp, _ := strings.Cut(out.String(), "SSH 主机密钥指纹：")
	fp, _, _ = strings.Cut(fp, "\n")
	t.Setenv("LOGDIAG_PASSWORD", sim.Password)
	t.Setenv("LOGDIAG_ENABLE_PASSWORD", sim.Enable)
	out.Reset()
	errb.Reset()
	code = run([]string{"collect", "--type", "network", "--host", srv.Addr, "--user", sim.User, "--enable",
		"--out", dir, "--host-key", fp}, strings.NewReader(""), &out, &errb)
	if code != 0 || strings.Contains(out.String(), "首次连接") {
		t.Errorf("code=%d\n%s\n%s", code, out.String(), errb.String())
	}
}
