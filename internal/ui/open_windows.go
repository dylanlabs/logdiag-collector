// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"fmt"
	"syscall"
	"unsafe"
)

var procShellExecuteW = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")

// OpenWindow 用 Edge 的应用模式打开独立窗口（没有地址栏和标签页），
// Edge 不可用时交给默认浏览器。Windows 10/11 都自带 Edge。
func OpenWindow(url string) error {
	if err := shellExecute("msedge", "--app="+url); err == nil {
		return nil
	}
	return shellExecute(url, "")
}

func shellExecute(file, params string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	f, err := syscall.UTF16PtrFromString(file)
	if err != nil {
		return err
	}
	var p *uint16
	if params != "" {
		if p, err = syscall.UTF16PtrFromString(params); err != nil {
			return err
		}
	}
	const swShowNormal = 1
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(p)), 0, swShowNormal)
	if r <= 32 { // ShellExecute 约定：返回值不大于 32 表示失败
		return fmt.Errorf("无法打开 %s（错误码 %d）", file, r)
	}
	return nil
}
