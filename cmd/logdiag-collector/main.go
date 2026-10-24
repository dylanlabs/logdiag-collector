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
	"path/filepath"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/appdata"
	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/secret"
	"github.com/dylanlabs/logdiag-collector/internal/tasks"
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
	args := os.Args[1:]
	owned := prepareConsole(consoleNeedOf(args))
	code := run(args, os.Stdin, os.Stdout, os.Stderr)
	if owned || (code != 0 && ownsConsole()) {
		// 程序自己开的窗口、双击运行的命令行版本，窗口会随程序退出立即关闭，留住它让用户看到结果。
		fmt.Fprint(os.Stderr, "\n按回车键关闭窗口……")
		fmt.Fscanln(os.Stdin)
	}
	os.Exit(code)
}

// consoleNeed 是子命令对控制台的需要，见 prepareConsole。
type consoleNeed int

const (
	consoleNone        consoleNeed = iota // 界面：不要命令行窗口
	consoleOutput                         // 只输出几行：version、help、ui --no-open
	consoleInteractive                    // 要输入密码、看进度：probe、collect
)

func consoleNeedOf(args []string) consoleNeed {
	if len(args) == 0 {
		return consoleNone
	}
	switch args[0] {
	case "probe", "collect":
		return consoleInteractive
	case "ui":
		args = args[1:]
	default:
		if !strings.HasPrefix(args[0], "-") {
			return consoleOutput // version、help 和打错的命令
		}
	}
	for _, a := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && (name == "h" || name == "help" || name == "no-open") {
			return consoleOutput
		}
	}
	return consoleNone
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
	dataDir := fs.String("data", "", "数据文件夹（设备库和设置），默认 %APPDATA%\\LogDiag Collector")
	verbose := fs.Bool("v", false, "输出调试日志")
	if done, code := parseFlags(fs, args, stderr); done {
		return code
	}

	// 双击运行时没有命令行窗口，出错要弹对话框。
	fail := func(msg string) int {
		fmt.Fprintln(stderr, msg)
		if f, ok := stderr.(*os.File); ok && !usable(f) {
			alert(msg)
		}
		return 1
	}
	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		return fail(err.Error())
	}
	if !*noOpen {
		if launch := ui.FindRunning(dir); launch != "" {
			// 程序已经在运行（例如关掉窗口后还在等收集任务完成）：请它再开一个窗口。
			if err := ui.OpenWindow(launch); err != nil {
				return fail(fmt.Sprintf("无法自动打开窗口：%v\n请在浏览器中打开：%s", err, launch))
			}
			return 0
		}
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logOut, closeLog := openLog(dir, stderr)
	defer closeLog()
	logger := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))
	app, err := openApp(dir)
	if err != nil {
		logger.Error("启动失败", "错误", err)
		return fail(err.Error())
	}
	defer app.Devices.Close()
	for _, w := range app.Warnings {
		logger.Warn(w)
	}
	hub := ui.NewHub()
	app.Tasks = tasks.New(tasks.Options{
		Devices:  app.Devices,
		Settings: app.Settings.Get,
		Publish:  hub.Publish,
		Logger:   logger,
	})
	defer app.Tasks.Shutdown(20 * time.Second) // 退出前删除还开着的设备会话
	srv, err := ui.New(ui.Options{Addr: *addr, IdleExit: *idle, Logger: logger, Hub: hub, App: app, Busy: app.Tasks.Busy})
	if err != nil {
		logger.Error("启动失败", "错误", err)
		return fail(err.Error())
	}
	if unregister, err := srv.Register(dir); err != nil {
		logger.Warn("无法登记正在运行的程序，再次运行时会另开一个", "错误", err)
	} else {
		defer unregister()
	}
	logger.Info("已启动", "版本", buildinfo.Version, "数据文件夹", dir)

	launch := srv.LaunchURL()
	if *noOpen {
		fmt.Fprintf(stdout, "%s %s 已启动，请在浏览器中打开：%s\n", buildinfo.ProductName, buildinfo.Version, launch)
	} else if err := ui.OpenWindow(launch); err != nil {
		fail(fmt.Sprintf("无法自动打开窗口：%v\n请在浏览器中打开：%s", err, launch))
	}

	if err := srv.Run(ctx); err != nil {
		logger.Error("界面服务出错", "错误", err)
		return fail(err.Error())
	}
	return 0
}

// resolveDataDir 返回并创建数据文件夹，dir 为空时用默认位置。
func resolveDataDir(dir string) (string, error) {
	if dir == "" {
		var err error
		if dir, err = appdata.DefaultDir(); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("无法创建数据文件夹 %s：%w", dir, err)
	}
	return dir, nil
}

// 运行日志写在数据文件夹里，超过 1 MB 时把旧的改名为 .old.log 再重新开始。
const (
	logName    = "logdiag-collector.log"
	oldLogName = "logdiag-collector.old.log"
	maxLogSize = 1 << 20
)

// openLog 打开运行日志；能写标准错误时（命令行里运行）同时输出到那里。
func openLog(dir string, stderr io.Writer) (io.Writer, func()) {
	path := filepath.Join(dir, logName)
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		os.Rename(path, filepath.Join(dir, oldLogName))
	}
	var out []io.Writer
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		out = append(out, f)
	}
	if sf, ok := stderr.(*os.File); !ok || usable(sf) {
		out = append(out, stderr)
	}
	return io.MultiWriter(out...), func() {
		if f != nil {
			f.Close()
		}
	}
}

// openApp 打开数据文件夹里的设置和设备库。
func openApp(dir string) (*ui.App, error) {
	box, err := secret.New(dir)
	if err != nil {
		return nil, err
	}
	settings, warn, err := appdata.OpenSettings(dir)
	if err != nil {
		return nil, err
	}
	app := &ui.App{Settings: settings, DataDir: dir, SecretMethod: box.Method()}
	if warn != "" {
		app.Warnings = append(app.Warnings, warn)
	}
	devices, warn, err := inventory.Open(dir, box)
	if err != nil {
		return nil, err
	}
	if warn != "" {
		app.Warnings = append(app.Warnings, warn)
	}
	app.Devices = devices
	return app, nil
}
