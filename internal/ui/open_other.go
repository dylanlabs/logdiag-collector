// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package ui

import (
	"os/exec"
	"runtime"
)

// OpenWindow 用系统默认浏览器打开界面（非 Windows 平台只用于开发调试）。
func OpenWindow(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start()
}
