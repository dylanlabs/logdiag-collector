// SPDX-License-Identifier: GPL-3.0-or-later

// netsim 在本机启动模拟的网络设备（SSH 命令行），用来演示和试用网络设备收集，不需要真交换机。
// 输出是按厂商文档编写的合成内容，不能当作真机验证结果。不随发布的 exe 提供。
//
//	go run ./cmd/netsim                       每种系统一台，从 127.0.0.1:2201 起依次占用端口
//	go run ./cmd/netsim -kind iosxe,huawei    只启动这两种
//	go run ./cmd/netsim -listen 0.0.0.0:2201  让别的电脑也能连（注意防火墙和账号是公开的示例账号）
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/dylanlabs/logdiag-collector/internal/netsim"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:2201", "第一台设备的监听地址，后面的设备依次用下一个端口")
	kinds := flag.String("kind", strings.Join(netsim.Kinds, ","), "要模拟的系统，逗号分隔："+strings.Join(netsim.Kinds, "、"))
	ports := flag.Int("ports", 0, "诊断信息里的端口数，调大可以模拟几十 MB 的输出；0 表示 48")
	flag.Parse()

	host, p, err := net.SplitHostPort(*listen)
	port, perr := strconv.Atoi(p)
	if err != nil || perr != nil {
		fmt.Fprintf(os.Stderr, "监听地址 %q 不对，应为 主机:端口\n", *listen)
		os.Exit(2)
	}
	fmt.Println("模拟设备（合成输出，不是真机）：")
	for i, kind := range strings.Split(*kinds, ",") {
		d, err := netsim.New(strings.TrimSpace(kind))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		d.Ports = *ports
		srv, err := netsim.Listen(net.JoinHostPort(host, strconv.Itoa(port+i)), d)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer srv.Close()
		line := fmt.Sprintf("  %-7s %-21s 账号 %s 密码 %s", d.Kind, srv.Addr, d.User, d.Password)
		if d.Enable != "" {
			line += " enable 密码 " + d.Enable
		}
		fmt.Println(line)
	}
	fmt.Println("在 LogDiag Collector 里添加设备：类型选“网络”，管理口 IP 填上面的地址（带端口）。按 Ctrl+C 停止。")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}
