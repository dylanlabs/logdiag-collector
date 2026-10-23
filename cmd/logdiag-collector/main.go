// SPDX-License-Identifier: GPL-3.0-or-later

// LogDiag Collector：数据中心设备管理口信息管理与一键日志收集工具。
//
// 双击运行打开本机界面；也可以在命令行里使用子命令。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/ui"
)

const usage = `LogDiag Collector：服务器、存储、网络设备日志一键收集工具

用法：
  logdiag-collector               启动本机界面（与 ui 相同）
  logdiag-collector ui [选项]      启动本机界面
  logdiag-collector probe [选项]   只读探测一台设备的 Redfish 接口，可保存诊断记录
  logdiag-collector collect [选项] 对一台设备一键收集日志包（华为、超聚变、浪潮）
  logdiag-collector version       显示版本

查看子命令的选项：logdiag-collector <子命令> -h
`

func main() {
	code := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if code != 0 && ownsConsole() {
		// 双击运行时窗口会随程序退出立即关闭，留住它让用户看到错误。
		fmt.Fprint(os.Stderr, "\n按回车键关闭窗口……")
		fmt.Fscanln(os.Stdin)
	}
	os.Exit(code)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "ui"
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd {
	case "ui":
		return runUI(ctx, args, stdout, stderr)
	case "probe":
		return runProbe(ctx, args, stdin, stdout, stderr)
	case "collect":
		return runCollect(ctx, args, stdin, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, buildinfo.Get())
		return 0
	case "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "未知命令 %q。\n\n%s", cmd, usage)
	return 2
}

// parseFlags 解析选项；-h 打印帮助后返回 done=true。
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) (done bool, code int) {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, 0
		}
		return true, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "多余的参数：%s\n", strings.Join(fs.Args(), " "))
		return true, 2
	}
	return false, 0
}

func runUI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:0", "监听地址，只允许本机回环地址；端口 0 表示随机")
	noOpen := fs.Bool("no-open", false, "不自动打开窗口，只打印地址")
	idle := fs.Duration("idle-exit", 30*time.Second, "所有界面窗口关闭后多久退出，0 表示不自动退出")
	verbose := fs.Bool("v", false, "输出调试日志")
	if done, code := parseFlags(fs, args, stderr); done {
		return code
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	srv, err := ui.New(ui.Options{Addr: *addr, IdleExit: *idle, Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	fmt.Fprintf(stdout, "%s %s 已启动。\n", buildinfo.ProductName, buildinfo.Version)
	launch := srv.LaunchURL()
	if *noOpen {
		fmt.Fprintf(stdout, "请在浏览器中打开：%s\n", launch)
	} else if err := ui.OpenWindow(launch); err != nil {
		fmt.Fprintf(stderr, "无法自动打开窗口：%v\n请在浏览器中打开：%s\n", err, launch)
	} else if *idle > 0 {
		fmt.Fprintln(stdout, "界面已在新窗口中打开。关闭界面窗口后程序会自动退出，也可以按 Ctrl+C 退出。")
	} else {
		fmt.Fprintln(stdout, "界面已在新窗口中打开。按 Ctrl+C 退出。")
	}

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
