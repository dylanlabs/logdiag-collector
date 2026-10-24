// SPDX-License-Identifier: GPL-3.0-or-later

package main

// 发布版按 Windows 图形程序编译（-H windowsgui），双击运行时不出现命令行窗口。
// 命令行子命令要输入输出时，在这里接上控制台。

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procAllocConsole          = kernel32.NewProc("AllocConsole")
)

// consoleProcesses 返回连在本程序控制台上的进程数，没有控制台时为 0。
func consoleProcesses() int {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return int(n)
}

// ownsConsole 报告控制台窗口是否只属于本程序：双击运行的命令行版本，或者程序自己新开的控制台。
func ownsConsole() bool { return consoleProcesses() == 1 }

// redirected 报告标准句柄是否被重定向到了文件或管道（脚本、Git Bash、CI）。
func redirected(f *os.File) bool {
	if f == nil {
		return false
	}
	t, err := windows.GetFileType(windows.Handle(f.Fd()))
	return err == nil && (t == windows.FILE_TYPE_DISK || t == windows.FILE_TYPE_PIPE)
}

// usable 报告标准句柄能不能写：重定向了，或者程序有控制台。
func usable(f *os.File) bool { return redirected(f) || consoleProcesses() > 0 }

// prepareConsole 按子命令的需要接上控制台，返回 true 表示新开了控制台窗口（结束前要等用户按回车）。
//
// 在 cmd、PowerShell 里运行图形程序时，命令行不会等它结束，马上又能输入命令；
// 这时接到原来的窗口上，输入的密码可能被命令行当成命令。所以要输入密码、看进度的 probe、collect
// 另开一个窗口，只输出几行的 version、help 接到原来的窗口上。
func prepareConsole(need consoleNeed) (owned bool) {
	if need == consoleNone || consoleProcesses() > 0 {
		return false // 开发时编译的命令行版本本来就有控制台
	}
	in, out, errOut := !redirected(os.Stdin), !redirected(os.Stdout), !redirected(os.Stderr)
	switch {
	case need == consoleOutput && (out || errOut):
		if r, _, _ := procAttachConsole.Call(uintptr(^uint32(0))); r == 0 { // ATTACH_PARENT_PROCESS
			return false
		}
		in = false
	case need == consoleInteractive && (in || out):
		if r, _, _ := procAllocConsole.Call(); r == 0 {
			return false
		}
		owned = true
	default:
		return false
	}
	reopen := func(name string, std uint32, target **os.File) {
		f, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			return
		}
		windows.SetStdHandle(std, windows.Handle(f.Fd()))
		*target = f
	}
	if in {
		reopen("CONIN$", windows.STD_INPUT_HANDLE, &os.Stdin)
	}
	if out {
		reopen("CONOUT$", windows.STD_OUTPUT_HANDLE, &os.Stdout)
	}
	if errOut {
		reopen("CONOUT$", windows.STD_ERROR_HANDLE, &os.Stderr)
	}
	return owned
}

// alert 用对话框显示启动错误：双击运行时没有命令行窗口，错误只能这样告诉用户。
func alert(msg string) {
	text, _ := windows.UTF16PtrFromString(msg)
	title, _ := windows.UTF16PtrFromString("LogDiag Collector")
	windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND)
}
