// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// runProbe 只读探测一台设备：服务根、系统和管理器的型号与固件、可用的动作和日志服务。
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
		password = os.Getenv("LOGDIAG_PASSWORD")
		if password == "" {
			fmt.Fprintf(stderr, "%s 的密码：", *user)
			var err error
			password, err = readPassword(stdin)
			fmt.Fprintln(stderr)
			if err != nil {
				fmt.Fprintf(stderr, "读取密码失败：%v\n", err)
				return 1
			}
		}
	}

	var fingerprint atomic.Value // 第一次握手时的证书指纹
	hc := redfish.NewHTTPClient(redfish.TransportOptions{
		Legacy:        *legacy,
		OnCertificate: func(fp string) { fingerprint.CompareAndSwap(nil, fp) },
	})
	var rec *httprec.Recorder
	if *record != "" {
		rec = httprec.NewRecorder(hc.Transport)
		hc.Transport = rec
	}
	c, err := redfish.New(*host, redfish.Options{HTTPClient: hc, UserAgent: buildinfo.UserAgent()})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	fmt.Fprintf(stdout, "探测 %s\n", c.BaseURL())
	err = probe(ctx, c, *user, password, stdout)
	if fp, _ := fingerprint.Load().(string); fp != "" {
		fmt.Fprintf(stdout, "\n证书指纹（SHA-256）：%s\n", fp)
	}

	if rec != nil {
		cas := rec.Cassette()
		cas.Note = "真机录制：logdiag-collector probe"
		cas.Tool = buildinfo.Get().String()
		cas.RecordedAt = time.Now().Format(time.RFC3339)
		if serr := cas.Save(*record); serr != nil {
			fmt.Fprintf(stderr, "保存诊断记录失败：%v\n", serr)
		} else {
			fmt.Fprintf(stdout, "\n诊断记录已保存到 %s（%d 次请求）。\n"+
				"记录里已去掉密码、会话令牌和设备 IP，但仍有型号、序列号、主机名等设备信息，发送前请先看一遍。\n",
				*record, len(cas.Interactions))
		}
	}

	if err != nil {
		kind := redfish.KindOf(err)
		fmt.Fprintf(stderr, "\n失败（%s）：%v\n%s\n", kind, err, kind.Hint())
		return 1
	}
	return 0
}

func readPassword(stdin io.Reader) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		return string(b), err
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
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
		members, err := listMembers(ctx, c, coll.uri)
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
	var r struct {
		Manufacturer    string
		Model           string
		SerialNumber    string
		HostName        string
		ManagerType     string
		FirmwareVersion string
		BiosVersion     string
		PowerState      string
		Actions         json.RawMessage
		LogServices     redfish.ODataID
		Oem             map[string]json.RawMessage
	}
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
	printActions(w, "  ", listActions(r.Actions))

	if r.LogServices.ID == "" {
		return
	}
	services, err := listMembers(ctx, c, r.LogServices.ID)
	if err != nil {
		fmt.Fprintf(w, "  读取日志服务失败：%v\n", err)
		return
	}
	fmt.Fprintf(w, "  日志服务（%d 个）：\n", len(services))
	for _, s := range services {
		var ls struct {
			ID      string `json:"Id"`
			Actions json.RawMessage
		}
		if _, err := c.Get(ctx, s, &ls); err != nil {
			fmt.Fprintf(w, "    %s 读取失败：%v\n", s, err)
			continue
		}
		fmt.Fprintf(w, "    %s\n", s)
		printActions(w, "      ", listActions(ls.Actions))
	}
}

func listMembers(ctx context.Context, c *redfish.Client, uri string) ([]string, error) {
	var coll struct {
		Members []redfish.ODataID
	}
	if _, err := c.Get(ctx, uri, &coll); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range coll.Members {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

type action struct{ name, target string }

// listActions 列出 Actions 里的标准动作和 Oem 下各厂商的动作。
func listActions(raw json.RawMessage) []action {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	var out []action
	add := func(name string, body json.RawMessage) {
		var a struct {
			Target string `json:"target"`
		}
		json.Unmarshal(body, &a)
		out = append(out, action{name, a.Target})
	}
	for k, v := range top {
		if strings.HasPrefix(k, "#") {
			add(k, v)
			continue
		}
		if k != "Oem" {
			continue
		}
		var oem map[string]json.RawMessage
		json.Unmarshal(v, &oem)
		for k2, v2 := range oem {
			if strings.HasPrefix(k2, "#") {
				add(k2, v2)
				continue
			}
			var vendor map[string]json.RawMessage
			json.Unmarshal(v2, &vendor)
			for k3, v3 := range vendor {
				if strings.HasPrefix(k3, "#") {
					add(k3, v3)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b action) int { return strings.Compare(a.name, b.name) })
	return out
}

func printActions(w io.Writer, indent string, actions []action) {
	if len(actions) == 0 {
		return
	}
	fmt.Fprintf(w, "%s动作：\n", indent)
	for _, a := range actions {
		fmt.Fprintf(w, "%s  %s  %s\n", indent, a.name, a.target)
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
