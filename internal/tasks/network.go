// SPDX-License-Identifier: GPL-3.0-or-later

package tasks

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/appdata"
	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/drivers"
	"github.com/dylanlabs/logdiag-collector/internal/inventory"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

// networkVendors 是已支持一键收集的网络设备厂商：中文名和设备表里常见的写法（小写）。
// 品牌只用来提前挡住明显不支持的设备，连上以后按系统识别，所以 HPE 也放行：
// HPE（原 H3C）的 FlexNetwork 交换机是 Comware 系统，Aruba 交换机会在识别时报不支持。
var networkVendors = []struct {
	name    string
	aliases []string
}{
	{"思科", []string{"cisco", "思科"}},
	{"华为", []string{"huawei", "华为"}},
	{"新华三", []string{"h3c", "新华三", "华三", "hpe", "hewlett", "慧与", "惠普", "hp"}},
}

// NetworkSupported 报告设备表里的品牌写法是否是已支持的网络设备厂商；品牌为空时返回 false（收集时会自动识别）。
func NetworkSupported(vendor string) bool {
	v := strings.ToLower(strings.TrimSpace(vendor))
	if v == "" {
		return false
	}
	for _, b := range networkVendors {
		for _, a := range b.aliases {
			if strings.Contains(v, a) {
				return true
			}
		}
	}
	return false
}

// NetworkSupportedText 返回已支持的网络设备厂商，例如“思科、华为、新华三”。
func NetworkSupportedText() string {
	names := make([]string, len(networkVendors))
	for i, b := range networkVendors {
		names[i] = b.name
	}
	return strings.Join(names, "、")
}

// collectNetwork 通过 SSH 登录网络设备收集诊断信息。账号不在特权模式时用设备里填的 enable 密码；
// 设备记录过主机密钥时核对，不一致就不登录。
func (m *Manager) collectNetwork(ctx context.Context, t *task, a inventory.Access, settings appdata.Settings) (*collect.Result, func() any, error) {
	var rec *netdev.Recorder
	if settings.Record {
		rec = netdev.NewRecorder()
	}
	m.o.Logger.Info("开始收集", "设备", t.MgmtIP, "客户", t.Customer, "方式", "SSH")
	res, err := m.o.Collect(ctx, drivers.Request{
		Type:        netdev.DeviceType,
		Address:     a.Device.MgmtIP,
		User:        a.User,
		Password:    a.Password,
		Enable:      a.Enable,
		HostKey:     a.Device.SSHHostKey,
		NetRecorder: rec,
		Store:       newStore(a, t, settings),
		Progress:    func(e collect.Event) { m.progress(t, e) },
	})
	if rec == nil {
		return res, nil, err
	}
	return res, func() any {
		r := rec.Record()
		r.Note, r.Tool, r.RecordedAt = recordNote, buildinfo.Get().String(), time.Now().Format(time.RFC3339)
		return r
	}, err
}

// firmwareOf 是设备表“固件”一栏：服务器写 BMC 固件版本，网络设备写系统和版本，例如“NX-OS 9.3(8)”。
func firmwareOf(md collect.ManifestDevice) string {
	if md.Type == netdev.DeviceType {
		return strings.TrimSpace(md.Controller.Type + " " + md.Controller.Firmware)
	}
	return md.Controller.Firmware
}

// checkSSH 是网络设备的匿名检查：TCP 连接 SSH 端口，读服务器的版本标识（SSH-2.0-…）。
// 不登录，设备上最多留下一条连接记录。
func checkSSH(ctx context.Context, address string) (string, string) {
	addr, _, err := sshx.HostPort(address)
	if err != nil {
		return inventory.ReachUnreachable, err.Error()
	}
	_, port, _ := net.SplitHostPort(addr)
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dctx, "tcp", addr)
	cancel()
	if err != nil {
		return inventory.ReachUnreachable, "连不上 SSH 端口 " + port + "：" + describeNetErr(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	// 版本标识前面允许有别的文字行（RFC 4253 第 4.2 节），最多看几行。
	r := bufio.NewReader(io.LimitReader(conn, 8<<10))
	for range 10 {
		line, err := r.ReadString('\n')
		if v := strings.TrimSpace(line); strings.HasPrefix(v, "SSH-") {
			return inventory.ReachService, "SSH 服务可用（" + v + "）"
		}
		if err != nil {
			break
		}
	}
	return inventory.ReachPort, "端口 " + port + " 可达，但没有收到 SSH 服务的响应"
}
