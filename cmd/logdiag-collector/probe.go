// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// runProbe 只读探测一台设备：服务根、系统和管理器的型号与固件、可用的动作、日志服务和各厂商的收集入口。
// 用于阶段 0 真机验证：把 --record 保存的诊断记录发回，就能据此编写驱动和回放测试。
func runProbe(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	host := fs.String("host", "", "管理口地址，例如 10.0.0.10 或 10.0.0.10:8443（必填）")
	user := fs.String("user", "", "管理口账号；不填则只做匿名探测，不登录、不产生审计日志")
	record := fs.String("record", "", "把接口往返保存到此文件（已去掉密码和令牌）")
	legacy := fs.Bool("legacy-tls", false, "老设备：允许 TLS 1.0/1.1 和旧加密套件")
	timeout := fs.Duration("timeout", 3*time.Minute, "整体超时")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "用法：logdiag-collector probe --host <地址> [--user <账号>] [--record <文件>]\n\n"+
			"只调用读取类接口，不触发收集、不修改设备。密码会提示输入，也可以放在环境变量 LOGDIAG_PASSWORD 里。\n\n选项：\n")
		fs.PrintDefaults()
	}
	if done, code := parseFlags(fs, args, stderr); done {
		return code
	}
	if *host == "" {
		fmt.Fprintln(stderr, "缺少 --host。")
		fs.Usage()
		return 2
	}

	var password string
	if *user != "" {
		var err error
		if password, err = askPassword(*user, stdin, stderr); err != nil {
			fmt.Fprintf(stderr, "读取密码失败：%v\n", err)
			return 1
		}
	}

	conn := newDeviceConn(*legacy, *record != "")
	c, err := redfish.New(*host, redfish.Options{HTTPClient: conn.hc, UserAgent: buildinfo.UserAgent()})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	fmt.Fprintf(stdout, "探测 %s\n", c.BaseURL())
	err = probe(ctx, c, *user, password, stdout)
	conn.finish(*record, "真机录制：logdiag-collector probe", stdout, stderr)

	if err != nil {
		fmt.Fprintf(stderr, "\n失败（%s）：%v\n%s\n", redfish.KindOf(err), err, redfish.Advice(err))
		return 1
	}
	return 0
}

// probe 读取并打印设备信息。只发 GET 请求（登录和退出会话除外）。
func probe(ctx context.Context, c *redfish.Client, user, password string, w io.Writer) error {
	root, err := c.ServiceRoot(ctx)
	if err != nil {
		return fmt.Errorf("读取服务根：%w", err)
	}
	fmt.Fprintln(w, "\n[服务根（匿名读取）]")
	kv(w, "Redfish 版本", root.RedfishVersion)
	kv(w, "厂商", root.Vendor)
	kv(w, "产品", root.Product)
	kv(w, "UUID", root.UUID)
	kv(w, "OEM 扩展", strings.Join(sortedKeys(root.Oem), "、"))
	if user == "" {
		fmt.Fprintln(w, "\n未提供账号，只做了匿名探测。加 --user 可读取型号、序列号、固件版本和可用的日志动作。")
		return nil
	}

	if err := c.Login(ctx, user, password); err != nil {
		return fmt.Errorf("登录：%w", err)
	}
	fmt.Fprintln(w, "\n登录成功。")
	defer func() {
		if err := c.Logout(ctx); err != nil {
			fmt.Fprintf(w, "\n退出会话失败：%v\n", err)
		} else {
			fmt.Fprintln(w, "\n已退出会话。")
		}
	}()

	for _, coll := range []struct{ name, uri string }{{"系统", root.Systems.ID}, {"管理器", root.Managers.ID}} {
		if coll.uri == "" {
			continue
		}
		members, err := c.Members(ctx, coll.uri)
		if err != nil {
			fmt.Fprintf(w, "\n读取%s列表失败：%v\n", coll.name, err)
			continue
		}
		for _, uri := range members[:min(len(members), 4)] {
			describe(ctx, c, coll.name, uri, w)
		}
	}
	return nil
}

func describe(ctx context.Context, c *redfish.Client, kind, uri string, w io.Writer) {
	var r redfish.Resource
	fmt.Fprintf(w, "\n[%s %s]\n", kind, uri)
	if _, err := c.Get(ctx, uri, &r); err != nil {
		fmt.Fprintf(w, "  读取失败：%v\n", err)
		return
	}
	kv(w, "制造商", r.Manufacturer)
	kv(w, "型号", r.Model)
	kv(w, "序列号", r.SerialNumber)
	kv(w, "主机名", r.HostName)
	kv(w, "管理器类型", r.ManagerType)
	kv(w, "固件版本", r.FirmwareVersion)
	kv(w, "BIOS 版本", r.BiosVersion)
	kv(w, "电源状态", r.PowerState)
	kv(w, "OEM 扩展", strings.Join(sortedKeys(r.Oem), "、"))
	printActions(w, "  ", redfish.ParseActions(r.Actions))
	if kind == "管理器" {
		describeCollection(ctx, c, &r, w)
	}

	if r.LogServices.ID == "" {
		return
	}
	services, err := c.Members(ctx, r.LogServices.ID)
	if err != nil {
		fmt.Fprintf(w, "  读取日志服务失败：%v\n", err)
		return
	}
	fmt.Fprintf(w, "  日志服务（%d 个）：\n", len(services))
	for _, s := range services {
		var ls redfish.Resource
		if _, err := c.Get(ctx, s, &ls); err != nil {
			fmt.Fprintf(w, "    %s 读取失败：%v\n", s, err)
			continue
		}
		fmt.Fprintf(w, "    %s\n", s)
		printActions(w, "      ", redfish.ParseActions(ls.Actions))
	}
}

// describeCollection 列出管理器上各厂商收集日志包的入口和动作：Dell 的 DellLCService、
// HPE 的 ActiveHealthSystem、联想的 ServiceData。只读，真机核对驱动时看这里。
func describeCollection(ctx context.Context, c *redfish.Client, m *redfish.Resource, w io.Writer) {
	type entry struct{ name, uri string }
	var entries []entry
	if uri := redfish.Link(m.Links, "Oem", "Dell", "DellLCService"); uri != "" {
		entries = append(entries, entry{"Dell DellLCService", uri})
	} else if _, ok := m.Oem["Dell"]; ok {
		entries = append(entries, entry{"Dell DellLCService", strings.TrimSuffix(m.ODataID, "/") + "/Oem/Dell/DellLCService"})
	}
	if uri := m.OemLink("Links", "ActiveHealthSystem"); uri != "" {
		entries = append(entries, entry{"HPE ActiveHealthSystem", uri})
	}
	if uri := m.OemLink("ServiceData"); uri != "" {
		entries = append(entries, entry{"联想 ServiceData", uri})
	}
	for _, e := range entries {
		fmt.Fprintf(w, "  %s：%s\n", e.name, e.uri)
		var r redfish.Resource
		if _, err := c.Get(ctx, e.uri, &r); err != nil {
			fmt.Fprintf(w, "    读取失败：%v\n", err)
			continue
		}
		printActions(w, "    ", redfish.ParseActions(r.Actions))
		for _, k := range []string{"AHSLocation", "RecentWeek"} {
			if link := redfish.Link(r.Links, k); link != "" {
				fmt.Fprintf(w, "    %s：%s\n", k, link)
			}
		}
	}
}

// printActions 打印动作和参数的可选值，真机核对驱动参数时看这里。
func printActions(w io.Writer, indent string, actions []redfish.Action) {
	if len(actions) == 0 {
		return
	}
	fmt.Fprintf(w, "%s动作：\n", indent)
	for _, a := range actions {
		fmt.Fprintf(w, "%s  %s  %s\n", indent, a.Name, a.Target)
		params := make([]string, 0, len(a.Allowable))
		for p := range a.Allowable {
			params = append(params, p)
		}
		slices.Sort(params)
		for _, p := range params {
			fmt.Fprintf(w, "%s    %s 可选：%s\n", indent, p, strings.Join(a.Allowable[p], "、"))
		}
	}
}

func kv(w io.Writer, key, value string) {
	if value != "" {
		fmt.Fprintf(w, "  %s：%s\n", key, value)
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
