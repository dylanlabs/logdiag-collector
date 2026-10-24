// SPDX-License-Identifier: GPL-3.0-or-later

package update

import "golang.org/x/sys/windows/registry"

// systemProxy 读当前用户的手动代理设置；没有启用时返回 nil。每次检查更新都重新读，用户改了设置不用重启程序。
func systemProxy() *proxySettings {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	if enabled, _, err := k.GetIntegerValue("ProxyEnable"); err != nil || enabled == 0 {
		return nil
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil || server == "" {
		return nil
	}
	override, _, _ := k.GetStringValue("ProxyOverride")
	return &proxySettings{Server: server, Override: override}
}
