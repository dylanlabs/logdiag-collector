// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package update

// systemProxy 在 Windows 以外只用环境变量里的代理。
func systemProxy() *proxySettings { return nil }
