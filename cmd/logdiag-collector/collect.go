// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// collectSleep 是驱动等待设备时用的函数，测试时替换以跳过等待；nil 表示真实等待。
var collectSleep func(context.Context, time.Duration) error

// runCollect 对一台设备做一次一键收集。用于阶段 0 真机验证，界面的任务中心也会调用同一套驱动。
func runCollect(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	host := fs.String("host", "", "管理口地址，例如 10.0.0.10 或 10.0.0.10:8443（必填）")
	user := fs.String("user", "", "管理口账号（必填）")
	brand := fs.String("brand", "", "品牌："+brandList()+"；不填则自动识别")
	out := fs.String("out", ".", "保存位置；会在其中按 [客户]/日期/设备 建文件夹")
	customer := fs.String("customer", "", "客户名称，可以只写代号")
	record := fs.String("record", "", "把接口往返保存到此文件（已去掉密码和令牌），失败时发给开发者")
	legacy := fs.Bool("legacy-tls", false, "老设备：允许 TLS 1.0/1.1 和旧加密套件")
	acceptEULA := fs.Bool("accept-eula", false, "Dell：iDRAC 还没接受 SupportAssist 许可协议时代为接受（会改动设备设置）")
	timeout := fs.Duration("timeout", 40*time.Minute, "整体超时")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "用法：logdiag-collector collect --host <地址> --user <账号> [--brand <品牌>] [--record <文件>]\n\n"+
			"让设备生成一键日志包并下载，旁边写一份 manifest.json。不清除日志、不重启设备。\n"+
			"密码会提示输入，也可以放在环境变量 LOGDIAG_PASSWORD 里。按 Ctrl+C 取消时会退出会话，设备端的生成任务不受影响。\n\n选项：\n")
		fs.PrintDefaults()
	}
	if done, code := parseFlags(fs, args, stderr); done {
		return code
	}
	if *host == "" || *user == "" {
		fmt.Fprintln(stderr, "缺少 --host 或 --user。")
		fs.Usage()
		return 2
	}
	if *brand != "" {
		if _, ok := drivers.BrandByKey(*brand); !ok {
			fmt.Fprintf(stderr, "不认识品牌 %q，可选：%s\n", *brand, brandList())
			return 2
		}
	}
	password, err := askPassword(*user, stdin, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "读取密码失败：%v\n", err)
		return 1
	}

	conn := newDeviceConn(*legacy, *record != "")
	info := buildinfo.Get()
	store := &collect.Store{
		Dir:      *out,
		Customer: *customer,
		Tool:     collect.Tool{Name: info.Product, Version: info.Version},
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	fmt.Fprintf(stdout, "收集 %s\n", *host)
	res, err := drivers.Collect(ctx, drivers.Request{
		Address:    *host,
		User:       *user,
		Password:   password,
		Brand:      *brand,
		AcceptEULA: *acceptEULA,
		HTTPClient: conn.hc,
		UserAgent:  buildinfo.UserAgent(),
		Store:      store,
		Progress:   progressPrinter(stdout),
		Sleep:      collectSleep,
	})
	conn.finish(*record, "真机录制：logdiag-collector collect", stdout, stderr)

	if err != nil {
		fmt.Fprintf(stderr, "\n失败（%s）：%v\n%s\n", redfish.KindOf(err), err, redfish.Advice(err))
		if *record == "" {
			fmt.Fprintln(stderr, "加 --record diag.json 再运行一次，可把接口往返保存下来发给开发者。")
		}
		return 1
	}
	f := res.Manifest.Files[0]
	fmt.Fprintf(stdout, "\n完成：%s\n  大小：%.1f MB\n  SHA-256：%s\n  说明文件：%s\n",
		res.Path, float64(f.Size)/(1<<20), f.SHA256, filepath.Join(res.Dir, "manifest.json"))
	return 0
}

// progressPrinter 打印阶段变化和进度，相同的内容不重复打印。
func progressPrinter(w io.Writer) func(collect.Event) {
	var last collect.Event
	return func(e collect.Event) {
		if e == last {
			return
		}
		last = e
		line := fmt.Sprintf("[%s] %s", e.Stage, e.Message)
		if e.Percent >= 0 {
			line += fmt.Sprintf("（%d%%）", e.Percent)
		}
		fmt.Fprintf(w, "%s %s\n", time.Now().Format("15:04:05"), line)
	}
}

func brandList() string {
	s := ""
	for i, b := range drivers.Brands {
		if i > 0 {
			s += "、"
		}
		s += b.Key
	}
	return s
}
