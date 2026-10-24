// SPDX-License-Identifier: GPL-3.0-or-later

package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// 识别要登录，并发少一些，免得同时打开太多 BMC 会话。
const identifyConcurrency = 4

// Check 在后台匿名检查设备的连通状态：先连管理口端口，再读 Redfish 服务根。
// 不登录、不产生审计日志。返回开始检查的设备数。
func (m *Manager) Check(ids []string) int {
	var started []string
	m.mu.Lock()
	for _, id := range ids {
		if m.busy[id] == "" {
			m.busy[id] = "check"
			started = append(started, id)
		}
	}
	m.mu.Unlock()
	for _, id := range started {
		d, ok := m.o.Devices.SetStatus(id, func(s *inventory.Status) { s.Reach = inventory.ReachChecking })
		m.publishDevice(d, ok)
	}
	for _, id := range started {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.done(id)
			m.checks <- struct{}{}
			defer func() { <-m.checks }()
			m.checkOne(id)
		}()
	}
	return len(started)
}

func (m *Manager) done(id string) {
	m.mu.Lock()
	delete(m.busy, id)
	m.mu.Unlock()
}

func (m *Manager) checkOne(id string) {
	dev, ok := m.o.Devices.Get(id)
	if !ok {
		return
	}
	reach, msg := inventory.ReachUnknown, "没有填写管理口 IP"
	if dev.MgmtIP != "" && m.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		hc := redfish.NewHTTPClient(redfish.TransportOptions{Legacy: dev.LegacyTLS})
		reach, msg = m.o.Check(ctx, dev.MgmtIP, hc)
		cancel()
		hc.CloseIdleConnections()
	}
	if m.ctx.Err() != nil {
		reach, msg = inventory.ReachUnknown, ""
	}
	d, ok := m.o.Devices.SetStatus(id, func(s *inventory.Status) {
		s.Reach, s.ReachMessage, s.CheckedAt = reach, msg, time.Now()
	})
	m.publishDevice(d, ok)
}

// Identify 在后台登录设备，读取品牌、型号、序列号、主机名和 BMC 固件回填到设备表，然后退出会话。
// 正在收集、缺少账号密码或上次认证失败（账号密码没改过）的设备不登录，原因在返回值里。
func (m *Manager) Identify(ids []string) (started int, skipped []Skip) {
	type job struct {
		id string
		a  inventory.Access
	}
	var jobs []job
	m.mu.Lock()
	for _, id := range ids {
		a, err := m.o.Devices.Access(id)
		d := a.Device
		switch {
		case errors.Is(err, inventory.ErrNotFound):
			continue
		case err != nil:
			skipped = append(skipped, Skip{id, label(d), err.Error()})
		case m.collecting(id):
			skipped = append(skipped, Skip{id, label(d), "正在收集，收集完成后会自动回填型号和序列号"})
		case m.busy[id] != "":
			skipped = append(skipped, Skip{id, label(d), "正在检查或识别"})
		case d.Status.Login == inventory.LoginAuthFailed:
			skipped = append(skipped, Skip{id, label(d), "上次登录认证失败。为避免账号被锁定，请先修改账号或密码"})
		default:
			m.busy[id] = "identify"
			jobs = append(jobs, job{id, a})
		}
	}
	m.mu.Unlock()
	sem := make(chan struct{}, identifyConcurrency)
	for _, j := range jobs {
		d, ok := m.o.Devices.SetStatus(j.id, func(s *inventory.Status) { s.Login = inventory.LoginChecking })
		m.publishDevice(d, ok)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.done(j.id)
			sem <- struct{}{}
			defer func() { <-sem }()
			m.identifyOne(j.id, j.a)
		}()
	}
	return len(jobs), skipped
}

func (m *Manager) identifyOne(id string, a inventory.Access) {
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
	defer cancel()
	hc := redfish.NewHTTPClient(redfish.TransportOptions{Legacy: a.Device.LegacyTLS})
	defer hc.CloseIdleConnections()
	ident, err := m.o.Identify(ctx, drivers.Request{
		Address: a.Device.MgmtIP, User: a.User, Password: a.Password,
		HTTPClient: hc, UserAgent: buildinfo.UserAgent(), Sleep: m.o.Sleep,
	})
	now := time.Now()
	if err == nil {
		m.o.Devices.SetIdentity(id, inventory.Identity{
			Vendor: ident.Vendor(), Model: ident.Model, Serial: ident.Serial, Hostname: ident.Hostname, Firmware: ident.Firmware,
		})
		msg := ""
		if ident.Brand.Key == "" {
			msg = "已登录，但这个品牌暂不支持一键收集（已支持：" + SupportedText() + "）"
		}
		d, ok := m.o.Devices.SetStatus(id, func(s *inventory.Status) {
			s.Reach, s.ReachMessage, s.CheckedAt = inventory.ReachService, "", now
			s.Login, s.LoginMessage, s.LoginAt = inventory.LoginOK, msg, now
		})
		m.publishDevice(d, ok)
		return
	}
	kind := redfish.KindOf(err)
	d, ok := m.o.Devices.SetStatus(id, func(s *inventory.Status) {
		switch kind {
		case redfish.KindCanceled:
			s.Login = inventory.LoginUnknown
			return
		case redfish.KindAuth:
			s.Login = inventory.LoginAuthFailed
		default:
			s.Login = inventory.LoginFailed
		}
		if kind == redfish.KindNetwork {
			s.Reach, s.ReachMessage, s.CheckedAt = inventory.ReachUnreachable, describeNetErr(err), now
		}
		s.LoginMessage, s.LoginAt = kind.String()+"："+err.Error()+"。"+kind.Hint(), now
	})
	m.publishDevice(d, ok)
}

// checkReach 是默认的匿名检查：TCP 连接管理口端口，再不带凭据读一次 /redfish/v1/。
func checkReach(ctx context.Context, address string, hc *http.Client) (string, string) {
	c, err := redfish.New(address, redfish.Options{HTTPClient: hc})
	if err != nil {
		return inventory.ReachUnreachable, err.Error()
	}
	u, _ := url.Parse(c.BaseURL())
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	cancel()
	if err != nil {
		return inventory.ReachUnreachable, "连不上 " + port + " 端口：" + describeNetErr(err)
	}
	conn.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/redfish/v1/", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	resp, err := hc.Do(req)
	if err != nil {
		msg := describeNetErr(err)
		if s := strings.ToLower(err.Error()); strings.Contains(s, "tls") || strings.Contains(s, "handshake") {
			msg = "HTTPS 握手失败。老设备可在设备里勾选“允许旧版 TLS”后重试"
		}
		return inventory.ReachPort, "端口可达，但服务没有正常响应：" + msg
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
		var root redfish.ServiceRoot
		if json.Unmarshal(body, &root) != nil || root.RedfishVersion == "" {
			return inventory.ReachPort, "端口可达，但 /redfish/v1/ 返回的不是 Redfish 服务根"
		}
		desc := "Redfish " + root.RedfishVersion
		if v := strings.TrimSpace(root.Vendor + " " + root.Product); v != "" {
			desc += "，" + v
		}
		return inventory.ReachService, desc
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return inventory.ReachService, "Redfish 服务可用（服务根需要登录）"
	case resp.StatusCode == http.StatusNotFound:
		return inventory.ReachPort, "端口可达，但没有 Redfish 服务（可能在 BMC 设置里关闭了）"
	}
	return inventory.ReachPort, "端口可达，但 Redfish 服务返回 " + resp.Status
}

// describeNetErr 把网络错误换成给用户看的说法。
func describeNetErr(err error) string {
	var dns *net.DNSError
	var ne net.Error
	s := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &dns):
		return "主机名解析失败"
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return "连接超时，没有响应"
	case strings.Contains(s, "refused"):
		return "端口没有开放（连接被拒绝）"
	case strings.Contains(s, "unreachable"):
		return "网络不通（没有到该地址的路由）"
	}
	return err.Error()
}
