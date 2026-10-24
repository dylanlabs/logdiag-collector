// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// netTiming 是网络设备命令行的等待时间，测试时替换以加快；零值表示默认。
var netTiming netdev.Timing

// netCollect 是 collect --type network 的参数。
type netCollect struct {
	host, user, hostKey   string
	enable                bool
	out, customer, record string
	timeout               time.Duration
}

// runNetCollect 通过 SSH 登录一台网络设备，收集诊断信息和日志缓冲区。与界面的任务中心用同一套驱动。
func runNetCollect(ctx context.Context, o netCollect, stdin io.Reader, stdout, stderr io.Writer) int {
	password, err := askPassword(o.user, stdin, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "读取密码失败：%v\n", err)
		return 1
	}
	var enable string
	if o.enable {
		if enable, err = askSecret("enable 密码：", "LOGDIAG_ENABLE_PASSWORD", stdin, stderr); err != nil {
			fmt.Fprintf(stderr, "读取 enable 密码失败：%v\n", err)
			return 1
		}
	}
	var rec *netdev.Recorder
	if o.record != "" {
		rec = netdev.NewRecorder()
	}
	info := buildinfo.Get()
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	fmt.Fprintf(stdout, "收集 %s（网络设备，SSH）\n", o.host)
	res, err := drivers.Collect(ctx, drivers.Request{
		Type:        netdev.DeviceType,
		Address:     o.host,
		User:        o.user,
		Password:    password,
		Enable:      enable,
		HostKey:     o.hostKey,
		NetRecorder: rec,
		NetTiming:   netTiming,
		Store: &collect.Store{
			Dir:      o.out,
			Customer: o.customer,
			Tool:     collect.Tool{Name: info.Product, Version: info.Version},
		},
		Progress: progressPrinter(stdout),
	})

	if rec != nil {
		r := rec.Record()
		r.Note = "真机录制：logdiag-collector collect --type network"
		r.Tool = info.String()
		r.RecordedAt = time.Now().Format(time.RFC3339)
		if serr := r.Save(o.record); serr != nil {
			fmt.Fprintf(stderr, "保存诊断记录失败：%v\n", serr)
		} else {
			fmt.Fprintf(stdout, "\n诊断记录已保存到 %s（%d 步）。\n"+
				"记录里已去掉密码和设备地址，口令、密钥和团体名已遮盖，但仍有型号、序列号、主机名和配置等设备信息，发送前请先看一遍。\n",
				o.record, len(r.Steps))
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "\n失败（%s）：%v\n%s\n", redfish.KindOf(err), err, redfish.Advice(err))
		if o.record == "" {
			fmt.Fprintln(stderr, "加 --record diag.netrec.json 再运行一次，可把命令行会话保存下来发给开发者。")
		}
		return 1
	}

	f := res.Manifest.Files[0]
	fmt.Fprintf(stdout, "\n完成：%s\n  大小：%s\n  SHA-256：%s\n", res.Path, sizeText(f.Size), f.SHA256)
	var extras []string
	for _, x := range res.Manifest.Files[1:] {
		extras = append(extras, x.Name)
	}
	if len(extras) > 0 {
		fmt.Fprintf(stdout, "  附带：%s\n", strings.Join(extras, "、"))
	}
	fmt.Fprintf(stdout, "  说明文件：%s\n", filepath.Join(res.Dir, "manifest.json"))
	if o.hostKey == "" && res.SSHHostKey != "" {
		fmt.Fprintf(stdout, "\nSSH 主机密钥指纹：%s\n（首次连接，请记下；下次用 --host-key 传入即可核对是否被冒充）\n", res.SSHHostKey)
	}
	return 0
}

// sizeText 是文本文件的大小：不到 1 MB 的按 KB 显示。
func sizeText(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}
