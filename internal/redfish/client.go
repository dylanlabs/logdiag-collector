// SPDX-License-Identifier: GPL-3.0-or-later

// Package redfish 是采集驱动共用的 Redfish 客户端：会话、请求、任务轮询和文件下载。
//
// 各品牌驱动在此之上实现自己的触发和下载步骤，例如 Dell 的 SupportAssistCollection、
// 联想的 CollectDiagnosticData、华为的 Manager.Dump。
package redfish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxBody 是普通 JSON 响应的大小上限；日志包用 Download 流式下载，不受此限制。
const maxBody = 32 << 20

// Options 是 New 的可选参数。
type Options struct {
	// HTTPClient 为 nil 时使用 NewHTTPClient(TransportOptions{})。
	HTTPClient *http.Client
	// UserAgent 为空时使用 "LogDiagCollector"。
	UserAgent string
}

// Client 是一台设备的 Redfish 客户端，可以被多个 goroutine 同时使用。
type Client struct {
	base      *url.URL
	hc        *http.Client
	userAgent string
	retries   int
	sleep     func(context.Context, time.Duration) error // 测试时替换，免得真的等待

	mu         sync.Mutex
	token      string
	sessionURI string
	root       *ServiceRoot // 最近一次读到的服务根，登录时复用
}

// New 创建客户端。address 可以是 "10.0.0.10"、"10.0.0.10:8443"、"fe80::1" 或完整的 https:// 地址。
func New(address string, opts Options) (*Client, error) {
	u, err := parseAddress(address)
	if err != nil {
		return nil, err
	}
	c := &Client{
		base:      u,
		hc:        opts.HTTPClient,
		userAgent: opts.UserAgent,
		retries:   3,
		sleep:     sleepCtx,
	}
	if c.hc == nil {
		c.hc = NewHTTPClient(TransportOptions{})
	}
	if c.userAgent == "" {
		c.userAgent = "LogDiagCollector"
	}
	return c, nil
}

func parseAddress(address string) (*url.URL, error) {
	s := strings.TrimSpace(address)
	if s == "" {
		return nil, errors.New("管理口地址为空")
	}
	if ip := net.ParseIP(s); ip != nil && strings.Contains(s, ":") {
		s = "[" + s + "]" // 不带方括号的 IPv6 地址
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("管理口地址 %q 无效：%w", address, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("管理口地址 %q 的协议必须是 https 或 http", address)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("管理口地址 %q 缺少主机名或 IP", address)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// BaseURL 返回设备地址，例如 https://10.0.0.10。
func (c *Client) BaseURL() string { return c.base.String() }

// Response 是一次请求的结果。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Location 返回 Location 头，绝对地址只保留路径和查询串。
func (r *Response) Location() string {
	return pathOf(r.Header.Get("Location"))
}

// Decode 把 JSON 响应体解析到 v。
func (r *Response) Decode(v any) error {
	return json.Unmarshal(r.Body, v)
}

// pathOf 去掉绝对地址里的协议和主机。
func pathOf(ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	p := u.EscapedPath()
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// resolve 把路径拼到设备地址上。即使设备在 Location 或 @odata.id 里给了别的主机，
// 也只取路径，保证令牌只发给用户指定的设备。
func (c *Client) resolve(ref string) string {
	return c.base.String() + pathOf(ref)
}

func (c *Client) authToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// send 发送一次请求并读完响应体，不重试、不把状态码转成错误。
func (c *Client) send(ctx context.Context, method, ref string, body any, token string) (*Response, error) {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.resolve(ref), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OData-Version", "4.0")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("读取 %s %s 的响应：%w", method, pathOf(ref), err)
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("%s %s 的响应超过 %d MB", method, pathOf(ref), maxBody>>20)
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// Do 发送请求，4xx/5xx 转成 *HTTPError（同时返回响应，便于驱动查看设备给的细节）。
// 不重试：POST 重发可能让设备生成两次日志。
func (c *Client) Do(ctx context.Context, method, ref string, body any) (*Response, error) {
	resp, err := c.send(ctx, method, ref, body, c.authToken())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return resp, newHTTPError(method, pathOf(ref), resp)
	}
	return resp, nil
}

// Get 读取资源并解析到 v（v 可为 nil）。网络错误和 502/503/504 按 1、2、4 秒退避重试 3 次。
func (c *Client) Get(ctx context.Context, ref string, v any) (*Response, error) {
	var (
		resp *Response
		err  error
	)
	for attempt := 0; ; attempt++ {
		resp, err = c.send(ctx, http.MethodGet, ref, nil, c.authToken())
		if !retryable(ctx, resp, err) || attempt >= c.retries {
			break
		}
		if serr := c.sleep(ctx, time.Duration(1<<attempt)*time.Second); serr != nil {
			return nil, serr
		}
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return resp, newHTTPError(http.MethodGet, pathOf(ref), resp)
	}
	if v != nil {
		if err := resp.Decode(v); err != nil {
			return resp, fmt.Errorf("解析 %s 的响应：%w", pathOf(ref), err)
		}
	}
	return resp, nil
}

func retryable(ctx context.Context, resp *Response, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Post 调用动作或创建资源，不重试。
func (c *Client) Post(ctx context.Context, ref string, body any) (*Response, error) {
	if body == nil {
		body = struct{}{}
	}
	return c.Do(ctx, http.MethodPost, ref, body)
}

// Download 把文件流式写入 w，返回字节数和 SHA-256。
func (c *Client) Download(ctx context.Context, ref string, w io.Writer) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(ref), nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", c.userAgent)
	if tok := c.authToken(); tok != "" {
		req.Header.Set("X-Auth-Token", tok)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return 0, "", newHTTPError(http.MethodGet, pathOf(ref), &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: data})
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), resp.Body)
	if err != nil {
		return n, "", fmt.Errorf("下载 %s 中断（已收到 %d 字节）：%w", pathOf(ref), n, err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return n, "", fmt.Errorf("下载 %s 不完整：收到 %d 字节，设备声明 %d 字节", pathOf(ref), n, resp.ContentLength)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
