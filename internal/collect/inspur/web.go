// SPDX-License-Identifier: GPL-3.0-or-later

package inspur

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// web 是浪潮 BMC 网页自己的 REST 接口（/api/...）的客户端：
// 登录后用会话 Cookie 加 X-CSRFToken 请求头认证。
type web struct {
	base      string
	hc        *http.Client
	userAgent string
	csrf      string
}

func newWeb(base string, hc *http.Client, userAgent string) (*web, error) {
	if base == "" {
		return nil, errors.New("缺少设备地址")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	c := *hc
	c.Jar = jar
	return &web{base: strings.TrimRight(base, "/"), hc: &c, userAgent: userAgent}, nil
}

// login 用账号密码登录。认证失败直接返回，不重试，避免账号被锁定。
func (w *web) login(ctx context.Context, user, password string) error {
	form := url.Values{"username": {user}, "password": {password}}
	resp, err := w.send(ctx, http.MethodPost, "/api/session", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(http.MethodPost, "/api/session", resp); err != nil {
		return err
	}
	var body struct {
		CSRFToken string
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || body.CSRFToken == "" {
		return errors.New("设备接受了登录，但没有返回 CSRFToken")
	}
	w.csrf = body.CSRFToken
	return nil
}

// logout 删除会话。BMC 的会话数有上限，结束、失败或取消时都要调用。
func (w *web) logout(ctx context.Context) error {
	if w.csrf == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	resp, err := w.send(ctx, http.MethodDelete, "/api/session", "", nil)
	w.csrf = ""
	if err != nil {
		return fmt.Errorf("退出会话：%w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusNotFound {
		return checkStatus(http.MethodDelete, "/api/session", resp)
	}
	return nil
}

// call 发送请求（body 为 nil 时不带请求体），2xx 时把 JSON 响应解析到 out（out 可为 nil）。
func (w *web) call(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	ctype := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr, ctype = bytes.NewReader(data), "application/json"
	}
	resp, err := w.send(ctx, method, path, ctype, rdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(method, path, resp); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取 %s %s 的响应：%w", method, path, err)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析 %s 的响应：%w", path, err)
		}
	}
	return nil
}

// download 把文件流式写入 dst。
func (w *web) download(ctx context.Context, path string, dst io.Writer) error {
	resp, err := w.send(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkStatus(http.MethodGet, path, resp)
	}
	n, err := io.Copy(dst, resp.Body)
	if err != nil {
		return fmt.Errorf("下载 %s 中断（已收到 %d 字节）：%w", path, n, err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return fmt.Errorf("下载 %s 不完整：收到 %d 字节，设备声明 %d 字节", path, n, resp.ContentLength)
	}
	return nil
}

func (w *web) send(ctx context.Context, method, path, ctype string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.base+onlyPath(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", w.userAgent)
	req.Header.Set("Accept", "application/json, */*")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if w.csrf != "" {
		req.Header.Set("X-CSRFToken", w.csrf)
	}
	return w.hc.Do(req)
}

// onlyPath 去掉设备给出的地址里的协议和主机，保证会话只发给用户指定的设备。
func onlyPath(ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return "/" + strings.TrimLeft(ref, "/")
	}
	p := "/" + strings.TrimLeft(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// checkStatus 把 4xx/5xx 转成 *redfish.HTTPError，便于统一分类（401 认证失败、404 不支持……）。
func checkStatus(method, path string, resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	e := &redfish.HTTPError{Method: method, Path: path, StatusCode: resp.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error string `json:"error"`
		Msg   string `json:"msg"`
	}
	if json.Unmarshal(data, &body) == nil {
		e.Message = body.Error
		if e.Message == "" {
			e.Message = body.Msg
		}
	}
	return e
}
