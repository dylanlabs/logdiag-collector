// SPDX-License-Identifier: GPL-3.0-or-later

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// serve 起一个 HTTPS 服务，按路径返回内容；返回的 Checker 信任它的证书。
func serve(t *testing.T, routes map[string]string) (*httptest.Server, *Checker) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "LogDiagCollector/test" {
			t.Errorf("User-Agent：%q", ua)
		}
		body, ok := routes[r.URL.Path]
		switch {
		case !ok:
			http.NotFound(w, r)
		case strings.HasPrefix(body, "redirect:"):
			http.Redirect(w, r, strings.Replace(strings.TrimPrefix(body, "redirect:"), "SELF", srv.URL, 1), http.StatusFound)
		default:
			w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.CheckRedirect = NewClient().CheckRedirect
	return srv, &Checker{Client: client, UserAgent: "LogDiagCollector/test"}
}

func TestCheck(t *testing.T) {
	example, err := os.ReadFile("../../docs/schema/examples/latest.json")
	if err != nil {
		t.Fatal(err)
	}
	srv, c := serve(t, map[string]string{
		"/latest.json": string(example),
		"/bmckit.json": `{"version": "v0.3.0", "url": "https://logdiag.com/collector/", "notes": "  修复若干问题。 "}`,
		"/github.json": `{"tag_name": "v0.2.1", "html_url": "https://github.com/dylanlabs/logdiag-collector/releases/tag/v0.2.1",
			"published_at": "2026-12-25T02:00:00Z", "body": "## 下载与运行……"}`,
		"/login.html": `<html><body>请先登录上网认证</body></html>`,
		"/evil.json":  `{"version": "9.9.9", "url": "https://evil.example.com/collector.exe"}`,
		"/http.json":  "redirect:http://logdiag.com/collector/latest.json",
		"/loop.json":  "redirect:SELF/loop.json",
	})
	src := func(names ...string) []Source {
		var out []Source
		for _, n := range names {
			out = append(out, Source{Name: n, URL: srv.URL + "/" + n})
		}
		return out
	}
	ctx := context.Background()

	c.Sources = src("latest.json")
	res, err := c.Check(ctx, "0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Newer || !res.Required || res.Dev || res.Latest != "0.1.0" || res.Source != "latest.json" ||
		res.Page != "https://logdiag.com/collector/?utm_medium=app&utm_source=collector" || res.Notes == "" || res.ReleasedAt == "" {
		t.Errorf("比 min_supported 旧：%+v", res)
	}
	if res, _ := c.Check(ctx, "0.1.0"); res.Newer || res.Required {
		t.Errorf("已是最新：%+v", res)
	}
	if res, _ := c.Check(ctx, "0.2.0-beta.1"); res.Newer || res.Required {
		t.Errorf("比最新的正式版还新的测试版：%+v", res)
	}
	if res, _ := c.Check(ctx, "0.0.0-dev.g1a2b3c4"); !res.Newer || res.Required || !res.Dev {
		t.Errorf("开发版本不提示必须升级：%+v", res)
	}

	// 只有 version、url、notes 的 BMCKit 格式也能用。
	c.Sources = src("bmckit.json")
	if res, err := c.Check(ctx, "0.2.0"); err != nil || !res.Newer || res.Required || res.Latest != "0.3.0" || res.Notes != "修复若干问题。" {
		t.Errorf("BMCKit 格式：%+v %v", res, err)
	}

	// logdiag.com 读不到时改读 GitHub；GitHub 的正文不当作更新说明。
	c.Sources = append(src("missing.json"), src("github.json")...)
	res, err = c.Check(ctx, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Newer || res.Latest != "0.2.1" || res.Source != "github.json" || res.Notes != "" ||
		res.Page != "https://github.com/dylanlabs/logdiag-collector/releases/tag/v0.2.1?utm_medium=app&utm_source=collector" {
		t.Errorf("GitHub 兜底：%+v", res)
	}

	// 不是允许的网站：照样提示有新版本，但不给下载页链接。
	c.Sources = src("evil.json")
	if res, err := c.Check(ctx, "0.1.0"); err != nil || !res.Newer || res.Page != "" {
		t.Errorf("不允许的下载页：%+v %v", res, err)
	}

	for _, tc := range []struct{ source, want string }{
		{"missing.json", "HTTP 404"},
		{"login.html", "不是版本信息"},
		{"http.json", "不是 HTTPS"},
		{"loop.json", "重定向次数太多"},
	} {
		c.Sources = src(tc.source)
		if _, err := c.Check(ctx, "0.1.0"); err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "手动设置代理服务器") {
			t.Errorf("%s：%v", tc.source, err)
		}
	}

	// 都连不上时提示检查网络和代理。
	c.Sources = []Source{{Name: "logdiag.com", URL: "https://127.0.0.1:1/latest.json"}, {Name: "GitHub", URL: "https://127.0.0.1:1/github"}}
	if _, err := c.Check(ctx, "0.1.0"); err == nil || !strings.Contains(err.Error(), "logdiag.com：连不上；GitHub：连不上") || !strings.Contains(err.Error(), "手动设置代理服务器") {
		t.Errorf("连不上：%v", err)
	}
	c.Sources = []Source{{Name: "明文", URL: "http://logdiag.com/collector/latest.json"}}
	if _, err := c.Check(ctx, "0.1.0"); err == nil || !strings.Contains(err.Error(), "不是 HTTPS") {
		t.Errorf("明文地址：%v", err)
	}
}

func TestPageLink(t *testing.T) {
	for in, want := range map[string]string{
		"https://logdiag.com/collector/":                                     "https://logdiag.com/collector/?utm_medium=app&utm_source=collector",
		"https://www.logdiag.com/collector/?lang=zh#download":                "https://www.logdiag.com/collector/?lang=zh&utm_medium=app&utm_source=collector#download",
		"https://logdiag.com/collector/?utm_source=site":                     "https://logdiag.com/collector/?utm_source=site",
		"https://github.com/dylanlabs/logdiag-collector/releases/tag/v0.1.0": "https://github.com/dylanlabs/logdiag-collector/releases/tag/v0.1.0?utm_medium=app&utm_source=collector",
		"http://logdiag.com/collector/":                                      "",
		"https://logdiag.com:8443/collector/":                                "",
		"https://user@logdiag.com/":                                          "",
		"https://download.logdiag.com/collector.exe":                         "",
		"https://github.com/someone/else/releases":                           "",
		"https://github.com/dylanlabs/logdiag-collector-fake":                "",
		"https://logdiag.com.evil.example/":                                  "",
		"":                                                                   "",
	} {
		if got := PageLink(in); got != want {
			t.Errorf("PageLink(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestNotesTruncated(t *testing.T) {
	l, err := Parse([]byte(`{"version": "1.0.0", "url": "https://logdiag.com/collector/", "notes": "` + strings.Repeat("新", 700) + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res := compare(l, "0.9.0", "x"); len([]rune(res.Notes)) != maxNotes+1 || !strings.HasSuffix(res.Notes, "…") {
		t.Errorf("更新说明应截到 %d 字：%d", maxNotes, len([]rune(res.Notes)))
	}
	for _, bad := range []string{`{}`, `{"version": "最新"}`, `[]`, `{"tag_name": "release-1"}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s 应报错", bad)
		}
	}
}
