// SPDX-License-Identifier: GPL-3.0-or-later

package update

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

// ProxyFromSystem 返回访问 req 要用的代理：先看环境变量 HTTPS_PROXY、HTTP_PROXY、NO_PROXY，
// 没有时用 Windows“设置 → 网络和 Internet → 代理”里手动设置的代理服务器。
// 自动配置脚本（PAC）和自动检测（WPAD）不支持。只用于访问 LogDiag 网站和 GitHub，和设备通信不走代理。
func ProxyFromSystem(req *http.Request) (*url.URL, error) {
	if u, err := http.ProxyFromEnvironment(req); u != nil || err != nil {
		return u, err
	}
	return systemProxy().forURL(req.URL)
}

// proxySettings 是 Windows 的手动代理设置（注册表 HKCU\...\Internet Settings 的 ProxyServer、ProxyOverride）。
type proxySettings struct {
	// Server 是 "主机:端口"，或按协议分开写的 "http=主机:端口;https=主机:端口"。
	Server string
	// Override 是不走代理的地址，分号分隔，可以用 * 通配；<local> 表示不带点的主机名。
	Override string
}

// forURL 返回访问 u 要用的代理，不用代理时返回 nil。
func (p *proxySettings) forURL(u *url.URL) (*url.URL, error) {
	if p == nil || strings.TrimSpace(p.Server) == "" || p.bypass(u.Hostname()) {
		return nil, nil
	}
	server := strings.TrimSpace(p.Server)
	if strings.Contains(server, "=") {
		byScheme := map[string]string{}
		for _, part := range strings.Split(server, ";") {
			if k, v, ok := strings.Cut(part, "="); ok {
				byScheme[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
		server = byScheme[u.Scheme]
		if server == "" && u.Scheme == "https" {
			server = byScheme["http"] // 只设置了 HTTP 代理时，Windows 也用它访问 HTTPS 网站
		}
		if server == "" {
			return nil, nil // 只有 SOCKS 等不支持的代理
		}
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	return url.Parse(server)
}

func (p *proxySettings) bypass(host string) bool {
	host = strings.ToLower(host)
	for _, rule := range strings.Split(p.Override, ";") {
		rule = strings.ToLower(strings.TrimSpace(rule))
		if i := strings.Index(rule, "://"); i >= 0 {
			rule = rule[i+3:]
		}
		switch {
		case rule == "":
		case rule == "<local>":
			if !strings.Contains(host, ".") {
				return true
			}
		default:
			if ok, _ := path.Match(rule, host); ok {
				return true
			}
		}
	}
	return false
}
