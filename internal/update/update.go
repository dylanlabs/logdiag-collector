// SPDX-License-Identifier: GPL-3.0-or-later

// Package update 在用户点击“检查更新”时读一次最新版本信息，和当前版本比较。
//
// 不自动检查、不下载、不安装：有新版本时由界面询问，用户同意后才用浏览器打开下载页。
// 先读 logdiag.com 上的 latest.json（格式见 docs/schema/latest-1.schema.json，和 BMCKit 兼容），
// 连不上时改读 GitHub 上的最新 Release。请求只带 User-Agent（程序名和版本号），不发送任何本机信息。
// 和设备通信不同，这里走系统代理：公司网络往往要通过代理才能上外网。
package update

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
)

// Schema 是 latest.json 的格式版本。
const Schema = "logdiag.latest/1"

// Latest 是 latest.json 的内容，见 docs/schema/latest-1.schema.json。
type Latest struct {
	Schema       string     `json:"schema,omitempty"`
	Product      string     `json:"product,omitempty"`
	Version      string     `json:"version"`
	ReleasedAt   string     `json:"released_at,omitempty"`
	URL          string     `json:"url"`
	Notes        string     `json:"notes,omitempty"`
	MinSupported string     `json:"min_supported,omitempty"`
	Downloads    []Download `json:"downloads,omitempty"`
}

// Download 是一个下载文件，供网站下载页显示；Collector 自己不下载。
type Download struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
	Mirror string `json:"mirror,omitempty"`
}

// Source 是一个版本信息地址。
type Source struct {
	Name string // 给人看的名字，出错时显示
	URL  string
}

// DefaultSources 是默认的版本信息地址，按顺序读，读到一个就停。
var DefaultSources = []Source{
	{Name: "logdiag.com", URL: "https://logdiag.com/collector/latest.json"},
	{Name: "GitHub", URL: "https://api.github.com/repos/dylanlabs/logdiag-collector/releases/latest"},
}

// Result 是检查结果，界面据此显示。
type Result struct {
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	Newer      bool   `json:"newer"`    // 有新版本
	Required   bool   `json:"required"` // 当前版本低于 min_supported，必须升级
	Dev        bool   `json:"dev"`      // 当前是开发版本，不提示必须升级
	Page       string `json:"page"`     // 下载页（已加 utm），不是允许打开的网址时为空
	Notes      string `json:"notes"`
	ReleasedAt string `json:"released_at"`
	Source     string `json:"source"`
}

// Checker 检查更新。零值可用。
type Checker struct {
	Client    *http.Client // 为 nil 时用走系统代理、超时 15 秒的客户端
	Sources   []Source     // 为 nil 时用 DefaultSources
	UserAgent string       // 为空时用 buildinfo.UserAgent()
}

const (
	maxBody  = 1 << 20
	maxNotes = 600
	timeout  = 15 * time.Second
)

var (
	errNotHTTPS        = errors.New("被转到了不是 HTTPS 的网址，已停止")
	errTooManyRedirect = errors.New("重定向次数太多")
	errNotVersion      = errors.New("回应不是版本信息，可能被网络代理或登录页拦截")
)

// NewClient 返回检查更新用的 HTTP 客户端：走系统代理，只允许 HTTPS，最多跟随 3 次重定向，超时 15 秒。
func NewClient() *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 ProxyFromSystem,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errNotHTTPS
			}
			if len(via) >= 3 {
				return errTooManyRedirect
			}
			return nil
		},
	}
}

// Check 读最新版本信息，和 current 比较。所有地址都读不到时返回给人看的错误。
func (c *Checker) Check(ctx context.Context, current string) (*Result, error) {
	sources := c.Sources
	if sources == nil {
		sources = DefaultSources
	}
	var reasons []string
	offline := true
	for _, src := range sources {
		l, via, err := c.fetch(ctx, src.URL)
		if err == nil {
			return compare(l, current, src.Name), nil
		}
		why, unreachable := describe(err)
		if via != "" {
			why += "，经代理 " + via
		}
		offline = offline && unreachable
		reasons = append(reasons, src.Name+"："+why)
	}
	msg := "无法检查更新。" + strings.Join(reasons, "；") + "。"
	if offline {
		msg += "请确认这台电脑能上网；公司网络要通过代理上网时，在 Windows 设置 → 网络和 Internet → 代理里手动设置代理服务器（暂不支持自动配置脚本）。"
	}
	return nil, errors.New(msg)
}

// fetch 读一个地址；via 是用到的代理（用默认客户端时才知道），出错时一起告诉用户，便于排查。
func (c *Checker) fetch(ctx context.Context, rawURL string) (l *Latest, via string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, "", errNotHTTPS
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	ua := c.UserAgent
	if ua == "" {
		ua = buildinfo.UserAgent()
	}
	req.Header.Set("User-Agent", ua)
	client := c.Client
	if client == nil {
		client = NewClient()
		if p, err := ProxyFromSystem(req); err == nil && p != nil {
			via = p.Host
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, via, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, via, errors.New("还没有版本信息（HTTP 404）")
	case resp.StatusCode != http.StatusOK:
		return nil, via, fmt.Errorf("暂时没有回应（HTTP %d）", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, via, err
	}
	l, err = Parse(body)
	return l, via, err
}

// Parse 解析 latest.json；也接受 GitHub releases/latest 的回应（tag_name、html_url）。
func Parse(data []byte) (*Latest, error) {
	var r struct {
		Latest
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errNotVersion
	}
	l := r.Latest
	if r.TagName != "" {
		// GitHub Release 的正文是安装说明和提交列表，不当作更新说明。
		l = Latest{Version: r.TagName, URL: r.HTMLURL, ReleasedAt: r.PublishedAt}
	}
	if _, ok := ParseVersion(l.Version); !ok {
		return nil, errNotVersion
	}
	return &l, nil
}

func compare(l *Latest, current, source string) *Result {
	latest, _ := ParseVersion(l.Version)
	res := &Result{
		Current:    current,
		Latest:     latest.String(),
		Page:       PageLink(l.URL),
		Notes:      truncate(strings.TrimSpace(l.Notes), maxNotes),
		ReleasedAt: l.ReleasedAt,
		Source:     source,
	}
	cur, ok := ParseVersion(current)
	res.Dev = !ok || cur.Dev()
	res.Newer = latest.Compare(cur) > 0
	if min, ok := ParseVersion(l.MinSupported); ok && !res.Dev {
		res.Required = cur.Compare(min) < 0
	}
	return res
}

// PageLink 只接受 logdiag.com 和本项目 GitHub 上的 HTTPS 网页（和界面能打开的外部网页一致），
// 没有 utm_source 时加上 utm_source=collector&utm_medium=app，让网站知道访问来自本程序；其他网址返回空字符串。
func PageLink(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return ""
	}
	switch {
	case u.Host == "logdiag.com" || u.Host == "www.logdiag.com":
	case u.Host == "github.com" && (u.Path == "/dylanlabs/logdiag-collector" || strings.HasPrefix(u.Path, "/dylanlabs/logdiag-collector/")):
	default:
		return ""
	}
	if q := u.Query(); q.Get("utm_source") == "" {
		q.Set("utm_source", "collector")
		q.Set("utm_medium", "app")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// describe 把请求错误换成给人看的原因；unreachable 表示网络层面连不上（没有网络、超时、被代理拒绝等）。
func describe(err error) (why string, unreachable bool) {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var netErr net.Error
	var certErr *tls.CertificateVerificationError
	for _, e := range []error{errNotHTTPS, errTooManyRedirect, errNotVersion} {
		if errors.Is(err, e) {
			return e.Error(), false
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "已取消", false
	case errors.As(err, &certErr):
		return "网站证书校验没有通过，可能被公司网络的安全设备拦截", false
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "连接超时", true
	case errors.As(err, &dnsErr), errors.As(err, &opErr):
		return "连不上", true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "连接失败：" + ue.Err.Error(), true // 去掉 Get "网址": 前缀；代理拒绝连接时只有一个 Forbidden
	}
	return err.Error(), false
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
