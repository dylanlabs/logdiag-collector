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
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/httprec"
)

// replayServer 用录制记录模拟一台 HTTPS 设备。
func replayServer(t *testing.T, cassette string) (*httptest.Server, *httprec.Replayer) {
	t.Helper()
	p, err := httprec.LoadReplayer(cassette)
	if err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(srv.Close)
	return srv, p
}

func TestCollectRequiresHostAndUser(t *testing.T) {
	if code, _, errOut := runCmd(t, "collect", "--host", "10.0.0.10"); code != 2 || !strings.Contains(errOut, "--user") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
	if code, _, errOut := runCmd(t, "collect", "--host", "10.0.0.10", "--user", "a", "--brand", "contoso"); code != 2 || !strings.Contains(errOut, "huawei") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestCollectHuawei(t *testing.T) {
	collectSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { collectSleep = nil })
	srv, p := replayServer(t, filepath.Join("..", "..", "internal", "drivers", "testdata", "huawei_dump.json"))

	const password = "Pa55-word-for-test"
	t.Setenv("LOGDIAG_PASSWORD", password)
	dir := t.TempDir()
	record := filepath.Join(dir, "collect-record.json")
	code, out, errOut := runCmd(t, "collect", "--host", srv.URL, "--user", "admin", "--out", dir, "--record", record)
	if code != 0 {
		t.Fatalf("code=%d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"[探测] 识别为 Huawei 2288H V5", "[设备生成中]", "（40%）", "完成：", "dump_info.tar.gz", "诊断记录已保存"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "Huawei_2288HV5_*", "manifest.json"))
	if len(matches) != 1 {
		t.Errorf("manifest.json：%v", matches)
	}
	data, err := os.ReadFile(record)
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

func TestCollectFailureHint(t *testing.T) {
	collectSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { collectSleep = nil })
	srv, _ := replayServer(t, filepath.Join("..", "..", "internal", "drivers", "testdata", "inspur_auth_failed.json"))
	const password = "Wrong-Pa55-for-test"
	t.Setenv("LOGDIAG_PASSWORD", password)
	code, _, errOut := runCmd(t, "collect", "--host", srv.URL, "--user", "admin", "--out", t.TempDir())
	if code != 1 || !strings.Contains(errOut, "认证失败") || !strings.Contains(errOut, "不会自动重试") || !strings.Contains(errOut, "--record") {
		t.Errorf("code=%d err=%s", code, errOut)
	}

	// 失败时也保存诊断记录，网页接口的表单登录同样去掉密码。
	srv, _ = replayServer(t, filepath.Join("..", "..", "internal", "drivers", "testdata", "inspur_auth_failed.json"))
	record := filepath.Join(t.TempDir(), "diag.json")
	if code, _, _ := runCmd(t, "collect", "--host", srv.URL, "--user", "admin", "--out", t.TempDir(), "--record", record); code != 1 {
		t.Errorf("code=%d", code)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(password)) || bytes.Contains(data, []byte("Pa55")) {
		t.Error("诊断记录里不应出现密码")
	}
	if !bytes.Contains(data, []byte("/api/session")) {
		t.Error("诊断记录里应有网页接口的登录请求")
	}
}
