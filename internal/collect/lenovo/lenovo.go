// SPDX-License-Identifier: GPL-3.0-or-later

// Package lenovo 是联想 ThinkSystem XCC、XCC2、XCC3 的 FFDC（服务数据）驱动。
//
// 流程依据联想官方 python-redfish-lenovo 的 lenovo_export_ffdc_data.py（Apache-2.0，只参考接口行为，
// 不复制代码）和《XCC2 REST API》里 LenovoServiceData 的说明：
//
//  1. 管理器有 Oem.Lenovo.ServiceData 时（XCC、XCC2），POST 它的 #LenovoServiceData.ExportFFDCData
//     {"InitializationNeeded":true,"DataCollectionType":"ProcessorDump"}，即官方脚本默认的完整 FFDC。
//     返回任务；与官方脚本一样轮询任务资源到 Completed，Oem.Lenovo.FFDCForDownloading.Path 就是文件地址，
//     例如 /imm_dump/FFDC/7D8TCTO1WW_7D8T123456_xcc_20240305-193437.tzz。
//  2. 没有时用标准动作（XCC3）：系统 LogServices/DiagnosticLog 的 #LogService.CollectDiagnosticData，
//     POST {"DiagnosticDataType":"Manager"}，任务完成后读日志条目的 AdditionalDataURI。
//  3. 带令牌 GET 下载。不传 ExportURI：文件留在 XCC 上由本工具取回，不推送到别的服务器。
//
// AMD 平台的 SR635、SR655 用的不是 XCC，服务数据只能推送到 SFTP 或 HTTP 服务器，暂不支持。
package lenovo

import (
	"context"
	"encoding/json"
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

// GenerateTimeout 是等待设备生成的上限。
const GenerateTimeout = 30 * time.Minute

// PollInterval 是查询任务的间隔。FFDC 要几分钟，查得太勤只会增加 XCC 的负担。
const PollInterval = 10 * time.Second

// DataCollectionTypes 是 ExportFFDCData 的收集类型，按优先顺序：完整 FFDC，没有时退而求其次。
var DataCollectionTypes = []string{"ProcessorDump", "ServiceDataFile"}

// errNoExport 表示 ServiceData 资源里没有 ExportFFDCData，改用标准动作。
var errNoExport = errors.New("ServiceData 没有 ExportFFDCData 动作")

// Driver 是 XCC 驱动。
type Driver struct{}

func (Driver) Name() string { return "lenovo-xcc" }

func (Driver) Collect(ctx context.Context, j *collect.Job) (*collect.Result, error) {
	if j.Redfish == nil || j.Manager == nil {
		return nil, fmt.Errorf("XCC 需要 Redfish 接口：%w", redfish.ErrUnsupported)
	}
	if j.Device.Controller.Type == "" {
		j.Device.Controller.Type = "XCC"
	}
	if uri := j.Manager.OemLink("ServiceData"); uri != "" {
		res, err := serviceData(ctx, j, uri)
		if !errors.Is(err, errNoExport) {
			return res, err
		}
	}
	if j.System != nil && j.System.LogServices.ID != "" {
		return diagnosticLog(ctx, j, strings.TrimSuffix(j.System.LogServices.ID, "/")+"/DiagnosticLog")
	}
	if _, ok := redfish.FindAction(redfish.ParseActions(j.Manager.Actions), "Manager.DownloadServiceData"); ok {
		return nil, redfish.WithHint(
			fmt.Errorf("管理器 %s 只有 DownloadServiceData（推送到文件服务器）：%w", j.Manager.ODataID, redfish.ErrUnsupported),
			redfish.KindUnsupported, "这台联想服务器的 BMC 只能把服务数据推送到 SFTP 或 HTTP 服务器，暂不支持；可以在 BMC 网页上手动下载服务数据。")
	}
	return nil, unsupported(fmt.Errorf("管理器 %s 没有 FFDC 收集接口：%w", j.Manager.ODataID, redfish.ErrUnsupported))
}

// serviceData 走 XCC 的 OEM 接口 LenovoServiceData.ExportFFDCData。
func serviceData(ctx context.Context, j *collect.Job, uri string) (*collect.Result, error) {
	var sd struct {
		Actions            json.RawMessage
		DataCollectionType json.RawMessage // XCC2 是 "ProcessorDump, ServiceDataFile, …" 这样的字符串
	}
	if _, err := j.Redfish.Get(ctx, uri, &sd); err != nil {
		return nil, fmt.Errorf("读取 ServiceData：%w", err)
	}
	export, ok := redfish.FindAction(redfish.ParseActions(sd.Actions), "LenovoServiceData.ExportFFDCData")
	if !ok {
		return nil, errNoExport
	}
	typ := pickType(append(export.Allowable["DataCollectionType"], splitList(sd.DataCollectionType)...))
	body := map[string]any{"InitializationNeeded": true, "DataCollectionType": typ}

	j.Report(collect.StageTrigger, -1, "调用 FFDC 收集（LenovoServiceData.ExportFFDCData，"+typ+"）")
	_, res, err := run(ctx, j, export.Target, body)
	if err != nil {
		return nil, err
	}
	file := ffdcPath(res.Task)
	if file == "" {
		return nil, errors.New("XCC 报告 FFDC 已生成，但任务里没有文件地址（Oem.Lenovo.FFDCForDownloading）")
	}
	return download(ctx, j, collect.Log{Type: "lenovo.xcc.ffdc", Method: "redfish-oem"}, file)
}

// diagnosticLog 走 DMTF 标准动作 LogService.CollectDiagnosticData。
func diagnosticLog(ctx context.Context, j *collect.Job, uri string) (*collect.Result, error) {
	var ls redfish.Resource
	if _, err := j.Redfish.Get(ctx, uri, &ls); err != nil {
		if redfish.KindOf(err) == redfish.KindUnsupported {
			return nil, unsupported(fmt.Errorf("读取 %s：%w", uri, err))
		}
		return nil, fmt.Errorf("读取诊断日志服务：%w", err)
	}
	collectAction, ok := redfish.FindAction(redfish.ParseActions(ls.Actions), "LogService.CollectDiagnosticData")
	if !ok {
		return nil, unsupported(fmt.Errorf("%s 没有 CollectDiagnosticData 动作：%w", uri, redfish.ErrUnsupported))
	}
	if dt := collectAction.Allowable["DiagnosticDataType"]; len(dt) > 0 && !slices.Contains(dt, "Manager") {
		return nil, unsupported(fmt.Errorf("CollectDiagnosticData 不支持 Manager 类型（可选 %s）：%w", strings.Join(dt, "、"), redfish.ErrUnsupported))
	}

	j.Report(collect.StageTrigger, -1, "调用诊断数据收集（LogService.CollectDiagnosticData）")
	started, res, err := run(ctx, j, collectAction.Target, map[string]string{"DiagnosticDataType": "Manager"})
	if err != nil {
		return nil, err
	}
	entry := entryLocation(started, res)
	if entry == "" {
		return nil, errors.New("XCC 报告诊断数据已生成，但没有给出日志条目地址")
	}
	var le struct{ AdditionalDataURI string }
	if _, err := j.Redfish.Get(ctx, entry, &le); err != nil {
		return nil, fmt.Errorf("读取诊断数据条目：%w", err)
	}
	if le.AdditionalDataURI == "" {
		return nil, fmt.Errorf("诊断数据条目 %s 没有下载地址（AdditionalDataURI）", entry)
	}
	return download(ctx, j, collect.Log{Type: "lenovo.xcc.ffdc", Method: "redfish-standard"}, le.AdditionalDataURI)
}

// run 触发动作并等任务结束。与官方脚本一样轮询任务资源本身（@odata.id），完成后的任务资源里才有文件地址；
// 响应里没有任务资源时轮询 Location 或 TaskMonitor。
func run(ctx context.Context, j *collect.Job, target string, body any) (*redfish.ActionResult, *redfish.TaskResult, error) {
	started, err := j.Redfish.StartAction(ctx, target, body)
	if err != nil {
		return nil, nil, fmt.Errorf("触发 FFDC 收集：%w", err)
	}
	poll := started.Monitor
	if started.Task != nil && started.Task.ODataID != "" {
		poll = started.Task.ODataID
	}
	if poll == "" {
		return started, &redfish.TaskResult{Task: started.Task, Response: started.Response}, nil
	}
	j.Report(collect.StageGenerate, -1, "XCC 正在生成服务数据（FFDC），通常需要 3 到 10 分钟")
	res, err := j.Redfish.WaitTask(ctx, poll, redfish.PollOptions{
		Interval: PollInterval,
		Timeout:  GenerateTimeout,
		OnProgress: func(p redfish.Progress) {
			j.Report(collect.StageGenerate, p.Percent, p.Message)
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("等待 FFDC 收集：%w", err)
	}
	return started, res, nil
}

func download(ctx context.Context, j *collect.Job, log collect.Log, file string) (*collect.Result, error) {
	name := path.Base(strings.SplitN(file, "?", 2)[0])
	if !strings.Contains(name, ".") {
		name = "xcc_ffdc.tzz"
	}
	j.Report(collect.StageDownload, -1, "下载 "+name)
	return j.Save(log, name, func(w io.Writer) error {
		if _, _, err := j.Redfish.Download(ctx, file, w); err != nil {
			return fmt.Errorf("下载 FFDC：%w", err)
		}
		return nil
	})
}

// pickType 按优先顺序选设备支持的收集类型；设备没说支持哪些时用完整 FFDC。
func pickType(supported []string) string {
	for _, t := range DataCollectionTypes {
		if len(supported) == 0 || slices.Contains(supported, t) {
			return t
		}
	}
	return DataCollectionTypes[0]
}

// splitList 把 "ProcessorDump, ServiceDataFile" 或 ["ProcessorDump"] 变成列表。
func splitList(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			list = append(list, v)
		}
	}
	return list
}

// ffdcPath 返回任务 Oem.Lenovo.FFDCForDownloading.Path 里的文件地址。
func ffdcPath(t *redfish.Task) string {
	if t == nil {
		return ""
	}
	for _, raw := range t.Oem {
		var v struct{ FFDCForDownloading struct{ Path string } }
		if json.Unmarshal(raw, &v) == nil && v.FFDCForDownloading.Path != "" {
			return v.FFDCForDownloading.Path
		}
	}
	return ""
}

// entryLocation 找生成的日志条目地址：结束时响应的 Location 头（DMTF 任务监视器），
// 任务的 Payload.HttpHeaders，或触发时响应体里的 Location（联想官方脚本按这个取）。
func entryLocation(started *redfish.ActionResult, res *redfish.TaskResult) string {
	if loc := res.Location(); loc != "" {
		return loc
	}
	if res.Response != nil {
		var t struct {
			Payload struct {
				HTTPHeaders []string `json:"HttpHeaders"`
			}
		}
		if json.Unmarshal(res.Response.Body, &t) == nil {
			for _, h := range t.Payload.HTTPHeaders {
				if k, v, ok := strings.Cut(h, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Location") {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	var body struct{ Location string }
	if json.Unmarshal(started.Response.Body, &body) == nil {
		return body.Location
	}
	return ""
}

func unsupported(err error) error {
	return redfish.WithHint(err, redfish.KindUnsupported,
		"这台联想服务器的 BMC 没有提供 FFDC 收集接口，可先升级 XCC 固件再试；IMM2（System x）计划在 v0.2 支持。")
}
