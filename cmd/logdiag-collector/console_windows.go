// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"syscall"
	"unsafe"
)

var procGetConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// ownsConsole 报告控制台窗口是否只属于本程序，即用户是双击运行的，而不是在 cmd 或 PowerShell 里。
func ownsConsole() bool {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}
