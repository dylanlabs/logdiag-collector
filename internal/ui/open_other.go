// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package ui

import (
	"os"
	"os/exec"
	"path/filepath"
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

// OpenFolder 用系统的文件管理器打开文件夹；path 是文件时打开所在文件夹。
func OpenFolder(path string) error {
	if st, err := os.Stat(path); err != nil {
		return err
	} else if !st.IsDir() {
		path = filepath.Dir(path)
	}
	return OpenWindow(path)
}
