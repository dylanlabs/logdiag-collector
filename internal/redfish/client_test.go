// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return c, srv
}

func TestParseAddress(t *testing.T) {
	cases := map[string]string{
		"10.0.0.10":                    "https://10.0.0.10",
		" 10.0.0.10:8443 ":             "https://10.0.0.10:8443",
		"fe80::1":                      "https://[fe80::1]",
		"[fe80::1]:443":                "https://[fe80::1]:443",
		"https://bmc.example/redfish/": "https://bmc.example",
		"http://10.0.0.10":             "http://10.0.0.10",
	}
	for in, want := range cases {
		u, err := parseAddress(in)
		if err != nil || u.String() != want {
			t.Errorf("parseAddress(%q) = %v, %v；应为 %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://10.0.0.1", "https://"} {
		if _, err := parseAddress(bad); err == nil {
			t.Errorf("parseAddress(%q) 应报错", bad)
		}
	}
}

// 设备在 Location 里给出别的主机时，令牌也只发给用户指定的设备。
func TestTokenStaysOnDevice(t *testing.T) {
	var (
		mu     sync.Mutex
		tokens []string
	)
	record := func(r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("X-Auth-Token"))
		mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /redfish/v1/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Links":{"Sessions":{"@odata.id":"/redfish/v1/SessionService/Sessions"}}}`)
	})
	mux.HandleFunc("POST /redfish/v1/SessionService/Sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Auth-Token", "tok")
		w.Header().Set("Location", "https://attacker.invalid/redfish/v1/SessionService/Sessions/9")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /redfish/v1/Actions/Do", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://attacker.invalid/redfish/v1/TaskService/TaskMonitors/1")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /redfish/v1/TaskService/TaskMonitors/1", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /redfish/v1/SessionService/Sessions/9", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	c, _ := testClient(t, mux)
	ctx := context.Background()
	if err := c.Login(ctx, "u", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunAction(ctx, "/redfish/v1/Actions/Do", nil, PollOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(tokens, ",") != "tok,tok" {
		t.Errorf("轮询和退出都应发到原设备并带令牌，实际 %v", tokens)
	}
}

func TestLoginWithoutToken(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			return
		}
		io.WriteString(w, `{}`)
	}))
	if err := c.Login(context.Background(), "u", "p"); err == nil || !strings.Contains(err.Error(), "X-Auth-Token") {
		t.Errorf("没有令牌时应报错，实际 %v", err)
	}
}

func TestLoginFallsBackWhenRootNeedsAuth(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/redfish/v1/":
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodPost && r.URL.Path == DefaultSessionsURI:
			w.Header().Set("X-Auth-Token", "tok")
			io.WriteString(w, `{"@odata.id":"/redfish/v1/SessionService/Sessions/3"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	if err := c.Login(context.Background(), "u", "p"); err != nil {
		t.Fatal(err)
	}
	if c.sessionURI != "/redfish/v1/SessionService/Sessions/3" {
		t.Errorf("没有 Location 时应取响应体的 @odata.id，实际 %q", c.sessionURI)
	}
}

// 用户取消后，Logout 仍要删除会话，避免占满 BMC 的会话数。
func TestLogoutAfterCancel(t *testing.T) {
	var deleted atomic.Bool
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted.Store(true)
		}
		w.WriteHeader(http.StatusNotFound) // 会话已过期也算成功
	}))
	c.token, c.sessionURI = "tok", "/redfish/v1/SessionService/Sessions/1"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if !deleted.Load() {
		t.Error("取消后也应发送 DELETE")
	}
}

func TestGetRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"Id":"1"}`)
	}))
	var v struct{ Id string }
	if _, err := c.Get(context.Background(), "/redfish/v1/Systems/1", &v); err != nil || v.Id != "1" {
		t.Fatalf("Get = %v, %v", v, err)
	}
	if calls.Load() != 3 {
		t.Errorf("应请求 3 次，实际 %d", calls.Load())
	}
}

func TestPostDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := c.Post(context.Background(), "/redfish/v1/Actions/Collect", nil)
	if KindOf(err) != KindBusy || calls.Load() != 1 {
		t.Errorf("POST 不应重试：calls=%d err=%v", calls.Load(), err)
	}
}

func TestWaitTaskToleratesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1, 2, 3:
			w.WriteHeader(http.StatusBadGateway)
		default:
			io.WriteString(w, `{"TaskState":"Completed"}`)
		}
	}))
	if _, err := c.WaitTask(context.Background(), "/t", PollOptions{}); err != nil {
		t.Errorf("连续 3 次 5xx 应能容忍，实际 %v", err)
	}
}

func TestWaitTaskGivesUpAfterMaxErrors(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := c.WaitTask(context.Background(), "/t", PollOptions{MaxErrors: 2})
	if err == nil || KindOf(err) != KindBusy {
		t.Errorf("超过容忍次数应失败并归为设备忙，实际 %v", err)
	}
}

func TestWaitTaskTimeout(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	c.sleep = sleepCtx
	start := time.Now()
	_, err := c.WaitTask(context.Background(), "/t", PollOptions{Interval: 10 * time.Millisecond, Timeout: 80 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || KindOf(err) != KindTimeout {
		t.Errorf("应超时，实际 %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("超时后应及时返回，用了 %v", time.Since(start))
	}
}

// 取消只停止本地等待，不去删除或中断设备端任务。
func TestWaitTaskCancelLeavesDeviceTaskAlone(t *testing.T) {
	var (
		mu      sync.Mutex
		methods []string
	)
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		n := len(methods)
		mu.Unlock()
		if n == 2 {
			cancel()
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	_, err := c.WaitTask(ctx, "/t", PollOptions{})
	if !errors.Is(err, context.Canceled) || KindOf(err) != KindCanceled {
		t.Errorf("应返回取消，实际 %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("取消后不应向设备发送 %s", m)
		}
	}
}

func TestWaitTaskDellJobStates(t *testing.T) {
	var calls atomic.Int32
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			io.WriteString(w, `{"Id":"JID_1","JobState":"Running","PercentComplete":40,"Message":"Collecting"}`)
			return
		}
		io.WriteString(w, `{"Id":"JID_1","JobState":"Failed","Message":"Unable to collect"}`)
	}))
	var got []Progress
	_, err := c.WaitTask(context.Background(), "/redfish/v1/Managers/iDRAC.Embedded.1/Jobs/JID_1",
		PollOptions{OnProgress: func(p Progress) { got = append(got, p) }})
	var te *TaskError
	if !errors.As(err, &te) || te.Messages[0] != "Unable to collect" {
		t.Errorf("JobState=Failed 应视为任务失败，实际 %v", err)
	}
	if len(got) != 1 || got[0] != (Progress{TaskRunning, 40, "Collecting"}) {
		t.Errorf("progress = %v", got)
	}
}

func TestStartActionSynchronous(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	res, err := c.RunAction(context.Background(), "/a", nil, PollOptions{})
	if err != nil || res.Response.StatusCode != http.StatusNoContent {
		t.Errorf("同步完成的动作应直接返回，实际 %+v, %v", res, err)
	}
}

func TestStartActionAcceptedWithoutMonitor(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	if _, err := c.StartAction(context.Background(), "/a", nil); err == nil {
		t.Error("202 却没有任务地址时应报错")
	}
}

func TestRetryAfter(t *testing.T) {
	if got := retryAfter("7"); got != 7*time.Second {
		t.Errorf("retryAfter(7) = %v", got)
	}
	date := time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(date); got < 15*time.Second || got > 21*time.Second {
		t.Errorf("retryAfter(%s) = %v", date, got)
	}
	for _, v := range []string{"", "soon", "-3"} {
		if got := retryAfter(v); got != 0 {
			t.Errorf("retryAfter(%q) = %v", v, got)
		}
	}
}

func TestDownload(t *testing.T) {
	payload := bytes.Repeat([]byte{0x1f, 0x8b, 0, 1}, 1000)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("GET /short", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)+10))
		w.Write(payload)
	})
	mux.HandleFunc("GET /missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	c, _ := testClient(t, mux)
	ctx := context.Background()

	var buf bytes.Buffer
	n, sum, err := c.Download(ctx, "/ok", &buf)
	want := sha256.Sum256(payload)
	if err != nil || n != int64(len(payload)) || sum != hex.EncodeToString(want[:]) || !bytes.Equal(buf.Bytes(), payload) {
		t.Errorf("Download(/ok) = %d, %s, %v", n, sum, err)
	}
	if _, _, err := c.Download(ctx, "/short", io.Discard); err == nil {
		t.Error("长度不足应报错")
	}
	if _, _, err := c.Download(ctx, "/missing", io.Discard); KindOf(err) != KindUnsupported {
		t.Errorf("404 应归为接口不支持，实际 %v", err)
	}
}

func TestKindOf(t *testing.T) {
	cases := []struct {
		err  error
		want Kind
	}{
		{&HTTPError{StatusCode: 401}, KindAuth},
		{&HTTPError{StatusCode: 403}, KindPermission},
		{&HTTPError{StatusCode: 405}, KindUnsupported},
		{&HTTPError{StatusCode: 503}, KindBusy},
		{&HTTPError{StatusCode: 500}, KindUnknown},
		{fmt.Errorf("wrap: %w", &TaskError{State: TaskKilled}), KindTaskFailed},
		{fmt.Errorf("wrap: %w", ErrTimeout), KindTimeout},
		{context.Canceled, KindCanceled},
		{errors.New("other"), KindUnknown},
	}
	for _, tc := range cases {
		if got := KindOf(tc.err); got != tc.want {
			t.Errorf("KindOf(%v) = %v，应为 %v", tc.err, got, tc.want)
		}
	}
	if KindAuth.Hint() == "" || KindAuth.String() != "认证失败" {
		t.Error("每个分类都应有名称和处理建议")
	}
}

func TestNetworkErrorKind(t *testing.T) {
	c, err := New("127.0.0.1:1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	c.retries = 0 // Windows 上连接被拒绝要等约 2 秒，不必重试
	_, err = c.ServiceRoot(context.Background())
	if KindOf(err) != KindNetwork {
		t.Errorf("连接被拒绝应归为网络不可达，实际 %v（%v）", KindOf(err), err)
	}
}

func TestCertificatePinning(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"RedfishVersion":"1.17.0"}`)
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	fp := hex.EncodeToString(sum[:])

	var seen string
	ok, _ := New(srv.URL, Options{HTTPClient: NewHTTPClient(TransportOptions{
		PinnedSHA256:  strings.ToUpper(fp[:2]) + ":" + fp[2:], // 允许大写和冒号
		OnCertificate: func(s string) { seen = s },
	})})
	if _, err := ok.ServiceRoot(context.Background()); err != nil {
		t.Fatalf("指纹一致时应能连接：%v", err)
	}
	if seen != fp {
		t.Errorf("OnCertificate 收到 %q，应为 %q", seen, fp)
	}

	bad, _ := New(srv.URL, Options{HTTPClient: NewHTTPClient(TransportOptions{PinnedSHA256: strings.Repeat("0", 64)})})
	bad.retries = 0
	if _, err := bad.ServiceRoot(context.Background()); err == nil || !strings.Contains(err.Error(), "指纹") {
		t.Errorf("指纹不一致时应拒绝连接，实际 %v", err)
	}
}

func TestLegacyCipherSuites(t *testing.T) {
	if len(legacyCipherSuites()) == 0 {
		t.Error("老设备模式应包含 TLS 1.2 及以下的套件")
	}
}
