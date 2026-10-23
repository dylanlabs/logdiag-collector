// SPDX-License-Identifier: GPL-3.0-or-later

// Package huawei 是华为、超聚变 iBMC 的一键收集驱动。
//
// 流程依据华为《iBMC Redfish 接口说明》和华为官方 PowerShell 模块 Huawei-iBMC-Cmdlets
// （Export-iBMCMaintenanceInfo、Invoke-iBMCFileDownload）：
//
//  1. 在管理器的 Actions.Oem.<厂商> 里找到 #Manager.Dump 和 #Manager.GeneralDownload。
//     厂商键不写死，超聚变新固件可能不是 "Huawei"。
//  2. POST Manager.Dump {"Type":"URI","Content":"/tmp/web/dump_info.tar.gz"}，
//     设备在 BMC 临时目录生成一键收集包，返回任务（最长约 10 分钟）。
//  3. 任务完成后 POST Manager.GeneralDownload {"TransferProtocol":"HTTPS","Path":...}，
//     响应体就是文件。
//
// 参数需在真机上核对：probe 会列出这两个动作及其参数的可选值。
package huawei

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// DumpPath 是一键收集包在 BMC 上的位置。iBMC 只允许从 /tmp/ 下载文件。
const DumpPath = "/tmp/web/dump_info.tar.gz"

// GenerateTimeout 是等待设备生成的上限。官方说明生成最多约 10 分钟，这里留出余量。
const GenerateTimeout = 20 * time.Minute

// Driver 是 iBMC 驱动。
type Driver struct{}

func (Driver) Name() string { return "huawei-ibmc" }

func (Driver) Collect(ctx context.Context, j *collect.Job) (*collect.Result, error) {
	if j.Redfish == nil || j.Manager == nil {
		return nil, fmt.Errorf("iBMC 需要 Redfish 接口：%w", redfish.ErrUnsupported)
	}
	actions := redfish.ParseActions(j.Manager.Actions)
	dump, ok := redfish.FindAction(actions, "Manager.Dump")
	if !ok {
		return nil, fmt.Errorf("管理器 %s 没有一键收集动作 Manager.Dump：%w", j.Manager.ODataID, redfish.ErrUnsupported)
	}
	download, ok := redfish.FindAction(actions, "Manager.GeneralDownload")
	if !ok {
		return nil, fmt.Errorf("管理器 %s 没有文件下载动作 Manager.GeneralDownload：%w", j.Manager.ODataID, redfish.ErrUnsupported)
	}
	if j.Device.Vendor == "" {
		j.Device.Vendor = dump.Vendor
	}
	if j.Device.Controller.Type == "" {
		j.Device.Controller.Type = "iBMC"
	}
	log := collect.Log{Type: "huawei.ibmc.dump", Method: "redfish-oem"}
	if strings.EqualFold(j.Device.Vendor, "xFusion") {
		log.Type = "xfusion.ibmc.dump"
	}

	j.Report(collect.StageTrigger, -1, "调用一键收集（Manager.Dump）")
	started, err := j.Redfish.StartAction(ctx, dump.Target, map[string]string{"Type": "URI", "Content": DumpPath})
	if err != nil {
		return nil, fmt.Errorf("触发一键收集：%w", err)
	}
	if started.Monitor != "" {
		j.Report(collect.StageGenerate, -1, "设备正在生成一键收集包，通常需要 3 到 10 分钟")
		_, err := j.Redfish.WaitTask(ctx, started.Monitor, redfish.PollOptions{
			Timeout: GenerateTimeout,
			OnProgress: func(p redfish.Progress) {
				j.Report(collect.StageGenerate, p.Percent, p.Message)
			},
		})
		if err != nil {
			return nil, fmt.Errorf("等待一键收集：%w", err)
		}
	}

	j.Report(collect.StageDownload, -1, "下载 "+DumpPath)
	body := map[string]string{"TransferProtocol": "HTTPS", "Path": DumpPath}
	return j.Save(log, "dump_info.tar.gz", func(w io.Writer) error {
		_, _, err := j.Redfish.DownloadAction(ctx, download.Target, body, w)
		if err != nil {
			return fmt.Errorf("下载一键收集包：%w", err)
		}
		return nil
	})
}
