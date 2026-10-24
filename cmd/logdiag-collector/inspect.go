// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/oscheck"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

// inspectSleep 占位，与 collect 对称；巡检脚本的等待在设备端，这里没有需要替换的本地等待。

// runInspect 对一台服务器做一次 OS 巡检：上传网站同款脚本、以 root 或 sudo 运行、取回结果。
// 用于真机验证，界面的任务中心也会调用同一套 oscheck.Inspect。
func runInspect(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	host := fs.String("host", "", "系统地址，例如 10.0.0.10 或 10.0.0.10:2222（必填）")
	user := fs.String("user", "", "登录账号（必填）")
	osName := fs.String("os", "linux", "系统类型："+scriptList())
	sudo := fs.Bool("sudo", true, "非 root 账号用 sudo 提权（需要该账号的 sudo 权限）")
	keyPath := fs.String("key", "", "私钥文件（可选，不能带口令）；填了优先用私钥登录")
	hostKey := fs.String("host-key", "", "期望的主机密钥指纹（SHA256:…）；不填则首次连接记录并显示指纹")
	out := fs.String("out", ".", "保存位置；会在其中按 [客户]/日期/设备 建文件夹")
	customer := fs.String("customer", "", "客户名称，可以只写代号")
	record := fs.String("record", "", "把 SSH 会话保存到此文件（已去掉密码），失败时发给开发者")
	timeout := fs.Duration("timeout", 15*time.Minute, "整体超时")
	showScript := fs.Bool("show-script", false, "打印将要执行的脚本和它的 SHA-256 后退出，不连接设备")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "用法：logdiag-collector inspect --host <地址> --user <账号> [--os linux] [--record <文件>]\n\n"+
			"通过 SSH 上传并运行 LogDiag 的 OS 巡检脚本，取回结果压缩包，旁边写一份 manifest.json。\n"+
			"脚本只做读取类收集，不改系统配置。结束、失败或取消时都会删除上传的脚本和临时文件。\n"+
			"密码会提示输入，也可以放在环境变量 LOGDIAG_PASSWORD 里。\n\n选项：\n")
		fs.PrintDefaults()
	}
	if done, code := parseFlags(fs, args, stderr); done {
		return code
	}

	script, ok := oscheck.ScriptFor(*osName)
	if !ok {
		fmt.Fprintf(stderr, "不支持的系统类型 %q，可选：%s\n", *osName, scriptList())
		return 2
	}
	if *showScript {
		fmt.Fprintf(stdout, "# %s 巡检脚本 %s\n# SHA-256：%s\n# 出处：%s\n\n%s",
			script.Title, script.Name, script.SHA256(), oscheck.ScriptSource, script.Content)
		return 0
	}
	if *host == "" || *user == "" {
		fmt.Fprintln(stderr, "缺少 --host 或 --user。")
		fs.Usage()
		return 2
	}

	var key []byte
	if *keyPath != "" {
		var err error
		if key, err = os.ReadFile(*keyPath); err != nil {
			fmt.Fprintf(stderr, "读取私钥失败：%v\n", err)
			return 1
		}
	}
	// 用私钥且没要密码时不提示输入密码。
	var password string
	if key == nil || os.Getenv("LOGDIAG_PASSWORD") != "" {
		var err error
		if password, err = askPassword(*user, stdin, stderr); err != nil {
			fmt.Fprintf(stderr, "读取密码失败：%v\n", err)
			return 1
		}
	}

	var rec *sshx.Recorder
	if *record != "" {
		rec = sshx.NewRecorder()
	}
	info := buildinfo.Get()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	fmt.Fprintf(stdout, "巡检 %s（%s）\n", *host, script.Title)
	res, err := oscheck.Inspect(ctx, oscheck.Request{
		Address:  *host,
		User:     *user,
		Password: password,
		Key:      key,
		HostKey:  *hostKey,
		Elevate:  *sudo,
		Device:   collect.Device{Type: "os"},
		Store: &collect.Store{
			Dir:      *out,
			Customer: *customer,
			Tool:     collect.Tool{Name: info.Product, Version: info.Version},
		},
		Progress: progressPrinter(stdout),
		Recorder: rec,
	}, script)

	if rec != nil {
		r := rec.Record()
		r.Note = "真机录制：logdiag-collector inspect"
		r.Tool = info.String()
		r.RecordedAt = time.Now().Format(time.RFC3339)
		if serr := r.Save(*record); serr != nil {
			fmt.Fprintf(stderr, "保存诊断记录失败：%v\n", serr)
		} else {
			fmt.Fprintf(stdout, "\n诊断记录已保存到 %s。\n"+
				"记录里已去掉密码，地址替换成占位符，但结果文件开头和主机名、序列号等仍在，发送前请先看一遍。\n", *record)
		}
	}

	if err != nil {
		kind := redfish.KindOf(err)
		fmt.Fprintf(stderr, "\n失败（%s）：%v\n%s\n", kind, err, kind.Hint())
		return 1
	}
	if hk := res.HostKey; *hostKey == "" && hk != "" {
		fmt.Fprintf(stdout, "\n主机密钥指纹：%s\n（首次连接，请记下；下次用 --host-key 传入即可核对是否被冒充）\n", hk)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(stdout, "提示：%s\n", w)
	}
	f := res.Manifest.Files[0]
	fmt.Fprintf(stdout, "\n完成：%s\n  大小：%.1f MB\n  SHA-256：%s\n  说明文件：%s\n",
		res.Path, float64(f.Size)/(1<<20), f.SHA256, filepath.Join(res.Dir, "manifest.json"))
	return 0
}

// scriptList 返回内置脚本的系统类型，例如 "linux"。
func scriptList() string {
	var ss []string
	for _, s := range oscheck.Scripts() {
		ss = append(ss, s.OS)
	}
	return strings.Join(ss, "、")
}
