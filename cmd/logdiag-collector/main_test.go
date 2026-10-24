// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionAndHelp(t *testing.T) {
	if code, out, _ := runCmd(t, "version"); code != 0 || !strings.Contains(out, "LogDiag Collector") {
		t.Errorf("version: code=%d out=%q", code, out)
	}
	for _, arg := range []string{"help", "-h", "--help"} {
		if code, out, _ := runCmd(t, arg); code != 0 || !strings.Contains(out, "用法") {
			t.Errorf("%s: code=%d out=%q", arg, code, out)
		}
	}
	if code, _, errOut := runCmd(t, "bogus"); code != 2 || !strings.Contains(errOut, "未知命令") {
		t.Errorf("bogus: code=%d err=%q", code, errOut)
	}
}

func TestUIRejectsLANAddress(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCmd(t, "ui", "--no-open", "--data", dir, "--addr", "0.0.0.0:0")
	if code != 1 || !strings.Contains(errOut, "回环地址") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, logName)); !strings.Contains(string(log), "回环地址") {
		t.Errorf("启动失败应写进运行日志：%q", log)
	}
}

func TestConsoleNeed(t *testing.T) {
	for _, c := range []struct {
		args string
		want consoleNeed
	}{
		{"", consoleNone},
		{"ui", consoleNone},
		{"-v", consoleNone},
		{"ui --data D:\\x", consoleNone},
		{"ui --no-open", consoleOutput},
		{"-no-open=true", consoleOutput},
		{"ui -h", consoleOutput},
		{"--help", consoleOutput},
		{"version", consoleOutput},
		{"bogus", consoleOutput},
		{"probe --host 10.0.0.1", consoleInteractive},
		{"collect -h", consoleInteractive},
	} {
		if got := consoleNeedOf(strings.Fields(c.args)); got != c.want {
			t.Errorf("%q: %d，应为 %d", c.args, got, c.want)
		}
	}
}

func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("x"), maxLogSize+1)
	if err := os.WriteFile(filepath.Join(dir, logName), big, 0o600); err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	w, closeLog := openLog(dir, &errb)
	io.WriteString(w, "新的一行\n")
	closeLog()
	if st, err := os.Stat(filepath.Join(dir, oldLogName)); err != nil || st.Size() != int64(len(big)) {
		t.Errorf("超过 1 MB 的日志应改名保留：%v", err)
	}
	if cur, _ := os.ReadFile(filepath.Join(dir, logName)); string(cur) != "新的一行\n" || errb.String() != "新的一行\n" {
		t.Errorf("新日志 %q，标准错误 %q", cur, errb.String())
	}
}

func TestProbeRequiresHost(t *testing.T) {
	if code, _, errOut := runCmd(t, "probe"); code != 2 || !strings.Contains(errOut, "--host") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func replayer(t *testing.T) *httprec.Replayer {
	t.Helper()
	p, err := httprec.LoadReplayer(filepath.Join("testdata", "probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeOutput(t *testing.T) {
	p := replayer(t)
	c, err := redfish.New("bmc.replay.test", redfish.Options{HTTPClient: &http.Client{Transport: p}})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := probe(context.Background(), c, "admin", "pw", &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Redfish 版本：1.17.0",
		"序列号：437XR1138R2",
		"固件版本：4.4.6521",
		"#LogService.CollectDiagnosticData  /redfish/v1/Systems/437XR1138R2/LogServices/DiagnosticLog/Actions/LogService.CollectDiagnosticData",
		"#Manager.Dump  /redfish/v1/Managers/BMC/Actions/Oem/Contoso/Manager.Dump",
		"已退出会话",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出里缺少 %q：\n%s", want, out.String())
		}
	}
	if left := p.Remaining(); len(left) > 0 {
		t.Errorf("没走到的请求：%v", left)
	}
}

func TestProbeAnonymous(t *testing.T) {
	c, _ := redfish.New("bmc.replay.test", redfish.Options{HTTPClient: &http.Client{Transport: replayer(t)}})
	var out bytes.Buffer
	if err := probe(context.Background(), c, "", "", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "只做了匿名探测") || strings.Contains(out.String(), "登录成功") {
		t.Errorf("不填账号时不应登录：\n%s", out.String())
	}
}

// 端到端：用回放记录模拟一台 HTTPS 设备，跑完整的 probe --record，检查保存的诊断记录不含密码。
func TestProbeRecord(t *testing.T) {
	p := replayer(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Scheme, r.URL.Host = "https", r.Host
		resp, err := p.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for k, vs := range resp.Header {
			w.Header()[k] = vs
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer srv.Close()

	const password = "Pa55-word-for-test"
	t.Setenv("LOGDIAG_PASSWORD", password)
	path := filepath.Join(t.TempDir(), "probe-record.json")
	code, out, errOut := runCmd(t, "probe", "--host", srv.URL, "--user", "admin", "--record", path)
	if code != 0 {
		t.Fatalf("code=%d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "证书指纹（SHA-256）：") || !strings.Contains(out, "诊断记录已保存") {
		t.Errorf("输出：\n%s", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "https://")
	for _, secret := range []string{password, host} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("诊断记录里不应出现 %q", secret)
		}
	}
	if left := p.Remaining(); len(left) > 0 {
		t.Errorf("没走到的请求：%v", left)
	}
}
