// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func startServer(t *testing.T, opts Options) (*Server, chan error) {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("服务没有及时退出")
		}
	})
	return s, done
}

// browser 模拟界面窗口：保存 Cookie，不自动跟随重定向。
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestNewRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.10:0", ":8443", "bad"} {
		if s, err := New(Options{Addr: addr}); err == nil {
			s.ln.Close()
			t.Errorf("New(%q) 应拒绝", addr)
		}
	}
}

func TestTokenExchangeAndAuth(t *testing.T) {
	s, _ := startServer(t, Options{})
	b := browser(t)

	if resp, _ := get(t, b, s.URL()); resp.StatusCode != http.StatusForbidden {
		t.Errorf("没有 Cookie 时应拒绝，实际 %d", resp.StatusCode)
	}
	if resp, _ := get(t, b, s.URL()+"?token=wrong"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("错误令牌应拒绝，实际 %d", resp.StatusCode)
	}

	launch := s.LaunchURL()
	resp, _ := get(t, b, launch)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("正确令牌应换 Cookie 并跳转，实际 %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if c := resp.Cookies(); len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("Cookie 应为 HttpOnly + SameSite=Strict：%v", c)
	}

	if resp, _ := get(t, browser(t), launch); resp.StatusCode != http.StatusForbidden {
		t.Errorf("令牌只能用一次，第二次应拒绝，实际 %d", resp.StatusCode)
	}

	resp, body := get(t, b, s.URL())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "LogDiag Collector") {
		t.Fatalf("首页：%d", resp.StatusCode)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("缺少安全响应头 %s", h)
		}
	}

	resp, body = get(t, b, s.URL()+"assets/app.css")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "#0b6e69") {
		t.Errorf("样式表应包含 BMCKit 的强调色：%d", resp.StatusCode)
	}

	resp, body = get(t, b, s.URL()+"api/info")
	var info struct{ Product, Version string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &info) != nil || info.Product != "LogDiag Collector" {
		t.Errorf("/api/info = %d %s", resp.StatusCode, body)
	}
}

func TestRejectsForeignHostAndOrigin(t *testing.T) {
	s, _ := startServer(t, Options{})
	b := browser(t)
	get(t, b, s.LaunchURL())

	// DNS 重绑定：恶意域名解析到 127.0.0.1，浏览器发出的 Host 是那个域名。
	req, _ := http.NewRequest(http.MethodGet, s.URL(), nil)
	req.Host = "evil.example:" + s.port
	if resp, err := b.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("外来 Host 应拒绝：%v %v", resp, err)
	}

	req, _ = http.NewRequest(http.MethodPost, s.URL()+"api/info", nil)
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := b.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("跨站 POST 应拒绝：%v %v", resp, err)
	}
}

func openEvents(t *testing.T, b *http.Client, s *Server) io.ReadCloser {
	t.Helper()
	resp, err := (&http.Client{Jar: b.Jar}).Get(s.URL() + "api/events")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("事件流：%d", resp.StatusCode)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "event: hello") {
		t.Fatalf("事件流第一行 = %q", line)
	}
	return resp.Body
}

func TestIdleExitAfterWindowCloses(t *testing.T) {
	s, done := startServer(t, Options{IdleExit: 100 * time.Millisecond})
	b := browser(t)
	get(t, b, s.LaunchURL())

	// 没打开过界面时不退出。
	select {
	case <-done:
		t.Fatal("还没打开过界面就退出了")
	case <-time.After(300 * time.Millisecond):
	}

	// 刷新页面：短暂断开后马上重连，不应退出。
	openEvents(t, b, s).Close()
	events := openEvents(t, b, s)
	select {
	case <-done:
		t.Fatal("页面重连期间不应退出")
	case <-time.After(300 * time.Millisecond):
	}

	events.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run 返回 %v", err)
		}
		done <- nil // 让 Cleanup 能读到
	case <-time.After(3 * time.Second):
		t.Fatal("窗口关闭后应自动退出")
	}
}
