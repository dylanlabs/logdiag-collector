// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procShellExecuteW         = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
	user32                    = syscall.NewLazyDLL("user32.dll")
	procSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
	procGetDpiForSystem       = user32.NewProc("GetDpiForSystem")
)

// OpenWindow 用 Edge 的应用模式打开独立窗口（没有地址栏和标签页），
// Edge 不可用时交给默认浏览器。Windows 10/11 都自带 Edge。
//
// 窗口用单独的 Edge 用户数据文件夹：用户自己的 Edge 通常已在后台运行（启动加速），
// 这时窗口大小参数会被忽略；单独的文件夹也不会混进用户的浏览记录、扩展和保存的密码。
func OpenWindow(url string) error {
	args := []string{"--app=" + url, "--no-first-run", "--no-default-browser-check"}
	if dir, err := os.UserCacheDir(); err == nil {
		args = append(args, "--user-data-dir="+filepath.Join(dir, "LogDiag Collector", "Edge"))
	}
	if x, y, w, h, ok := windowBounds(); ok {
		args = append(args, fmt.Sprintf("--window-position=%d,%d", x, y), fmt.Sprintf("--window-size=%d,%d", w, h))
	}
	for i, a := range args {
		args[i] = syscall.EscapeArg(a)
	}
	if err := shellExecute("msedge", strings.Join(args, " ")); err == nil {
		return nil
	}
	return shellExecute(url, "")
}

// windowBounds 返回主屏幕上居中的窗口位置和大小，单位是 Edge 命令行参数用的逻辑像素。
func windowBounds() (x, y, w, h int, ok bool) {
	var r struct{ Left, Top, Right, Bottom int32 }
	const spiGetWorkArea = 0x0030
	if ret, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0); ret == 0 {
		return 0, 0, 0, 0, false
	}
	// 程序声明了按显示器感知 DPI，取到的是物理像素，按系统缩放比例换算。
	dpi := 96
	if procGetDpiForSystem.Find() == nil {
		if d, _, _ := procGetDpiForSystem.Call(); d != 0 {
			dpi = int(d)
		}
	}
	scale := func(v int32) int { return int(v) * 96 / dpi }
	left, top := scale(r.Left), scale(r.Top)
	areaW, areaH := scale(r.Right)-left, scale(r.Bottom)-top
	if areaW < 640 || areaH < 480 {
		return 0, 0, 0, 0, false
	}
	w, h = windowSize(areaW, areaH)
	return left + (areaW-w)/2, top + (areaH-h)/2, w, h, true
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

// OpenURL 在系统默认浏览器里打开外部网页。
func OpenURL(url string) error { return shellExecute(url, "") }

// OpenFolder 在资源管理器里打开文件夹；path 是文件时打开所在文件夹并选中它。
func OpenFolder(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return shellExecute(path, "")
	}
	return shellExecute("explorer.exe", `/select,"`+path+`"`)
}
