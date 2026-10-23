// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// DefaultSessionsURI 是服务根没有给出 Links.Sessions 时使用的会话集合地址。
const DefaultSessionsURI = "/redfish/v1/SessionService/Sessions"

// ODataID 是 {"@odata.id": "..."} 形式的链接。
type ODataID struct {
	ID string `json:"@odata.id"`
}

// ServiceRoot 是 /redfish/v1/ 中识别设备用到的字段。
type ServiceRoot struct {
	RedfishVersion string
	UUID           string
	Vendor         string // Redfish 1.5 起才有，老固件为空
	Product        string
	Systems        ODataID
	Managers       ODataID
	Chassis        ODataID
	SessionService ODataID
	Links          struct {
		Sessions ODataID
	}
	Oem map[string]json.RawMessage
}

// ServiceRoot 匿名读取服务根。规范要求服务根不需要登录，所以后台探测只用这一步，不会刷审计日志。
func (c *Client) ServiceRoot(ctx context.Context) (*ServiceRoot, error) {
	var root ServiceRoot
	if _, err := c.Get(ctx, "/redfish/v1/", &root); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.root = &root
	c.mu.Unlock()
	return &root, nil
}

// Login 创建 Redfish 会话，之后的请求都带 X-Auth-Token。
//
// 认证失败直接返回，不重试：反复用错误密码登录会触发 BMC 的账号锁定。
func (c *Client) Login(ctx context.Context, user, password string) error {
	c.mu.Lock()
	root := c.root
	c.mu.Unlock()
	if root == nil {
		var err error
		root, err = c.ServiceRoot(ctx)
		var httpErr *HTTPError
		if err != nil && !errors.As(err, &httpErr) {
			return err
		}
		// 个别固件的服务根也要求登录（HTTPError），按默认地址继续。
	}
	sessions := DefaultSessionsURI
	if root != nil && root.Links.Sessions.ID != "" {
		sessions = root.Links.Sessions.ID
	}

	body := map[string]string{"UserName": user, "Password": password}
	resp, err := c.send(ctx, http.MethodPost, sessions, body, "")
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return newHTTPError(http.MethodPost, pathOf(sessions), resp)
	}
	token := resp.Header.Get("X-Auth-Token")
	if token == "" {
		return fmt.Errorf("设备接受了登录（%d），但没有返回 X-Auth-Token", resp.StatusCode)
	}
	uri := resp.Location()
	if uri == "" {
		var s ODataID
		if json.Unmarshal(resp.Body, &s) == nil {
			uri = pathOf(s.ID)
		}
	}

	c.mu.Lock()
	c.token, c.sessionURI = token, uri
	c.mu.Unlock()
	return nil
}

// LoggedIn 报告当前是否持有会话。
func (c *Client) LoggedIn() bool {
	return c.authToken() != ""
}

// Logout 删除会话。BMC 的会话数有上限，收集结束、失败或被取消时都要调用。
//
// 即使 ctx 已经取消，也会用一个 15 秒的新期限去删除会话。
// 会话已过期（404/401）视为成功。
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	token, uri := c.token, c.sessionURI
	c.token, c.sessionURI = "", ""
	c.mu.Unlock()
	if token == "" || uri == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	resp, err := c.send(ctx, http.MethodDelete, uri, nil, token)
	if err != nil {
		return fmt.Errorf("退出会话：%w", err)
	}
	switch {
	case resp.StatusCode < 300, resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusUnauthorized:
		return nil
	}
	return newHTTPError(http.MethodDelete, uri, resp)
}
