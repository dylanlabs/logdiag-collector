// SPDX-License-Identifier: GPL-3.0-or-later

// Package dell 是 Dell iDRAC9、iDRAC10 的 SupportAssist 收集（TSR）驱动。
//
// 流程依据 Dell 官方脚本 SupportAssistCollectionLocalREDFISH.py（iDRAC-Redfish-Scripting v18，
// 只参考接口行为，不复制代码）和 Dell OpenManage Ansible idrac_support_assist 模块的说明：
//
//  1. 读 DellLCService（管理器的 Links.Oem.Dell.DellLCService，没有时用 <管理器>/Oem/Dell/DellLCService），
//     确认有 #DellLCService.SupportAssistCollection。
//  2. POST SupportAssistCollection {"ShareType":"Local","DataSelectorArrayIn":["HWData","TTYLogs"]}，
//     返回 202，Location 头是任务地址（iDRAC9 是 Job，iDRAC10 是 TaskService 任务）。
//  3. 轮询该地址，直到响应的 Location 头指向 sacollect.zip（iDRAC9 随 200 返回，iDRAC10 随 204 返回），
//     再带令牌 GET 下载。Job 完成却没给文件地址时，按老版脚本的做法再查一次 TaskService/Tasks/<JobID>。
//
// iDRAC 还没接受 SupportAssist 许可协议（EULA）时，第 2 步返回 SRV085。Job.AcceptEULA 为 true 时
// 调用 #DellLCService.SupportAssistAcceptEULA 后重新触发一次，否则停下来提示用户。
//
// iDRAC7/8 不支持本地导出（只能推送到网络共享），计划在 1.1 用别的方式支持。
package dell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// DataSelectors 是要收集的内容：硬件数据（含 Lifecycle 日志、SEL）和存储控制器的 TTY 日志。
// DebugLogs 只供 Dell 技术支持使用；OSAppData 需要主机装 iDRAC Service Module，都不收。
var DataSelectors = []string{"HWData", "TTYLogs"}

// GenerateTimeout 是等待设备生成的上限。Dell 官方脚本等 30 分钟。
const GenerateTimeout = 30 * time.Minute

// FileName 是 iDRAC 给出的收集包文件名。
const FileName = "sacollect.zip"

// EULAHint 是 iDRAC 没接受 SupportAssist 许可协议时给用户的提示。
const EULAHint = "这台 iDRAC 还没有接受 Dell SupportAssist 许可协议（EULA），接受后才能导出收集包。" +
	"可以先在 iDRAC 网页的 SupportAssist 页面接受；或者在设置里勾选“代为接受 Dell SupportAssist 许可协议”" +
	"（命令行加 --accept-eula）后重试。"

// Driver 是 iDRAC 驱动。
type Driver struct{}

func (Driver) Name() string { return "dell-idrac" }

func (Driver) Collect(ctx context.Context, j *collect.Job) (*collect.Result, error) {
	if j.Redfish == nil || j.Manager == nil {
		return nil, fmt.Errorf("iDRAC 需要 Redfish 接口：%w", redfish.ErrUnsupported)
	}
	if j.Device.Controller.Type == "" {
		j.Device.Controller.Type = "iDRAC"
	}
	log := collect.Log{Type: "dell.idrac.tsr", Method: "redfish-oem"}

	lcURI := redfish.Link(j.Manager.Links, "Oem", "Dell", "DellLCService")
	if lcURI == "" {
		lcURI = strings.TrimSuffix(j.Manager.ODataID, "/") + "/Oem/Dell/DellLCService"
	}
	var lc redfish.Resource
	if _, err := j.Redfish.Get(ctx, lcURI, &lc); err != nil {
		if redfish.KindOf(err) == redfish.KindUnsupported {
			return nil, unsupported(fmt.Errorf("读取 DellLCService：%w", err))
		}
		return nil, fmt.Errorf("读取 DellLCService：%w", err)
	}
	actions := redfish.ParseActions(lc.Actions)
	sa, ok := redfish.FindAction(actions, "DellLCService.SupportAssistCollection")
	if !ok {
		return nil, unsupported(fmt.Errorf("%s 没有 SupportAssistCollection 动作：%w", lcURI, redfish.ErrUnsupported))
	}
	if st := sa.Allowable["ShareType"]; len(st) > 0 && !slices.Contains(st, "Local") {
		return nil, unsupported(fmt.Errorf("这台 iDRAC 的 SupportAssist 收集只能推送到网络共享（可选 %s）：%w",
			strings.Join(st, "、"), redfish.ErrUnsupported))
	}
	body := map[string]any{"ShareType": "Local", "DataSelectorArrayIn": selectors(sa.Allowable["DataSelectorArrayIn"])}

	j.Report(collect.StageTrigger, -1, "调用 SupportAssist 收集（DellLCService.SupportAssistCollection）")
	started, err := j.Redfish.StartAction(ctx, sa.Target, body)
	if err != nil && isEULAError(err) {
		if !j.AcceptEULA {
			return nil, redfish.WithHint(fmt.Errorf("触发 SupportAssist 收集：%w", err), redfish.KindConsent, EULAHint)
		}
		target := lcURI + "/Actions/DellLCService.SupportAssistAcceptEULA"
		if a, ok := redfish.FindAction(actions, "DellLCService.SupportAssistAcceptEULA"); ok {
			target = a.Target
		}
		j.Report(collect.StageTrigger, -1, "接受 SupportAssist 许可协议（设置里已允许）")
		if _, err := j.Redfish.Post(ctx, target, nil); err != nil {
			return nil, fmt.Errorf("接受 SupportAssist 许可协议：%w", err)
		}
		j.Report(collect.StageTrigger, -1, "重新调用 SupportAssist 收集")
		started, err = j.Redfish.StartAction(ctx, sa.Target, body)
	}
	if err != nil {
		return nil, fmt.Errorf("触发 SupportAssist 收集：%w", err)
	}
	if started.Monitor == "" {
		return nil, fmt.Errorf("iDRAC 接受了 SupportAssist 收集，但没有返回任务地址（%d）", started.Response.StatusCode)
	}

	j.Report(collect.StageGenerate, -1, "iDRAC 正在生成 SupportAssist 收集包，通常需要 5 到 15 分钟")
	res, err := j.Redfish.WaitTask(ctx, started.Monitor, redfish.PollOptions{
		Timeout: GenerateTimeout,
		OnProgress: func(p redfish.Progress) {
			j.Report(collect.StageGenerate, p.Percent, p.Message)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("等待 SupportAssist 收集：%w", err)
	}
	file := zipLocation(res.Location())
	if file == "" {
		file, err = taskServiceLocation(ctx, j, started.Monitor)
		if err != nil {
			return nil, err
		}
	}
	if res.Task != nil && res.Task.JobState == "CompletedWithErrors" {
		j.Report(collect.StageGenerate, -1, "iDRAC 报告收集完成但有错误，部分内容可能缺失："+res.Task.Text())
	}

	j.Report(collect.StageDownload, -1, "下载 "+file)
	name := path.Base(file)
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name = FileName
	}
	return j.Save(log, name, func(w io.Writer) error {
		if _, _, err := j.Redfish.Download(ctx, file, w); err != nil {
			return fmt.Errorf("下载 SupportAssist 收集包：%w", err)
		}
		return nil
	})
}

// selectors 按动作给出的可选值筛掉设备不认的数据项；设备没给可选值时原样发送。
func selectors(allowable []string) []string {
	if len(allowable) == 0 {
		return DataSelectors
	}
	var out []string
	for _, s := range DataSelectors {
		if slices.Contains(allowable, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{"HWData"}
	}
	return out
}

// isEULAError 判断触发失败是不是因为没接受许可协议（消息 SRV085）。
func isEULAError(err error) bool {
	var he *redfish.HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return strings.HasSuffix(he.MessageID, "SRV085") || strings.Contains(he.Message, "EULA")
}

// zipLocation 在 Location 指向收集包时返回它。
func zipLocation(loc string) string {
	p, _, _ := strings.Cut(strings.ToLower(loc), "?")
	if strings.HasSuffix(p, ".zip") || strings.Contains(p, "sacollect") {
		return loc
	}
	return ""
}

// taskServiceLocation 在 Job 完成却没给文件地址时，按老版官方脚本的做法查 TaskService/Tasks/<JobID>。
func taskServiceLocation(ctx context.Context, j *collect.Job, monitor string) (string, error) {
	missing := fmt.Errorf("iDRAC 报告收集完成，但没有给出 %s 的下载地址", FileName)
	jobID := path.Base(strings.TrimSuffix(monitor, "/"))
	uri := "/redfish/v1/TaskService/Tasks/" + jobID
	if !strings.HasPrefix(jobID, "JID_") || strings.TrimSuffix(monitor, "/") == uri {
		return "", missing
	}
	resp, err := j.Redfish.Get(ctx, uri, nil)
	if err != nil {
		return "", fmt.Errorf("%w：%w", missing, err)
	}
	if file := zipLocation(resp.Location()); file != "" {
		return file, nil
	}
	return "", missing
}

func unsupported(err error) error {
	return redfish.WithHint(err, redfish.KindUnsupported,
		"这台 iDRAC 不支持通过 Redfish 本地导出 SupportAssist 收集。iDRAC9 可先升级 iDRAC 固件再试；iDRAC7/8 计划在 1.1 支持。")
}
