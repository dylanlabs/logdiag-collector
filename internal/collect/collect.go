// SPDX-License-Identifier: GPL-3.0-or-later

// Package collect 是各品牌采集驱动的公共部分：任务阶段与进度、设备信息、
// 日志包的落盘、校验和 manifest。
//
// 每个驱动只负责“触发生成 → 等待 → 下载”，识别设备、保存文件、写 manifest
// 由这里和 internal/drivers 统一处理。
package collect

import (
	"context"
	"net/http"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// Stage 是收集任务的阶段。
type Stage int

const (
	StageQueued   Stage = iota // 排队
	StageProbe                 // 探测：识别品牌、型号和可用的收集方式
	StageTrigger               // 触发：让设备开始生成日志包
	StageGenerate              // 设备生成中
	StageDownload              // 下载
	StageVerify                // 校验文件完整性
	StageDone                  // 完成
)

var stageNames = [...]string{"排队", "探测", "触发", "设备生成中", "下载", "校验", "完成"}

func (s Stage) String() string {
	if s >= 0 && int(s) < len(stageNames) {
		return stageNames[s]
	}
	return "未知"
}

// Event 是报告给界面或命令行的进度。
type Event struct {
	Stage   Stage
	Percent int    // 0–100；-1 表示设备没有给出
	Message string // 给用户看的说明或设备返回的消息
}

// Device 是识别到的设备信息，写进 manifest 和文件夹名。
type Device struct {
	Type       string // server、storage、network、os
	Vendor     string // Huawei、xFusion、Inspur……
	Model      string
	Serial     string
	Hostname   string
	Address    string // 管理口地址
	Controller Controller
}

// Controller 是 BMC 或存储控制器。
type Controller struct {
	Type     string `json:"type"` // iBMC、BMC、iDRAC……
	Firmware string `json:"firmware"`
}

// Log 说明日志包的类型和采集方式，对应 manifest 的 log 字段。
type Log struct {
	Type   string `json:"type"`   // 编码，例如 huawei.ibmc.dump，见 docs/schema/manifest-1.schema.json
	Method string `json:"method"` // redfish-oem、redfish-standard、bmc-web-api……
}

// Job 是一次收集，驱动通过它拿到设备连接、报告进度、保存文件。
type Job struct {
	Device Device

	// Redfish 是已登录的 Redfish 客户端；用不到 Redfish 的驱动应先调用 Logout 释放会话。
	Redfish *redfish.Client
	// Manager 是第一个管理器资源（含 Actions），识别时已读到。
	Manager *redfish.Resource

	// BaseURL 是设备地址，例如 https://10.0.0.10。
	BaseURL string
	// HTTPClient 供非 Redfish 接口使用（例如浪潮的网页接口），与 Redfish 共用证书和 TLS 设置。
	HTTPClient *http.Client
	UserAgent  string
	User       string
	Password   string

	Store *Store
	// Progress 接收进度，可为 nil。
	Progress func(Event)
	// Sleep 是驱动等待设备时用的函数，回放测试用它跳过等待。为 nil 时真实等待。
	Sleep func(context.Context, time.Duration) error
}

// Report 报告进度。
func (j *Job) Report(stage Stage, percent int, message string) {
	if j.Progress != nil {
		j.Progress(Event{Stage: stage, Percent: percent, Message: message})
	}
}

// Wait 等待 d，ctx 取消时提前返回。
func (j *Job) Wait(ctx context.Context, d time.Duration) error {
	if j.Sleep != nil {
		return j.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Driver 是一类设备的采集驱动。
type Driver interface {
	// Name 是驱动名，例如 "huawei-ibmc"，用于命令行 --brand 和诊断记录。
	Name() string
	// Collect 生成并下载日志包，通过 j.Store.Save 保存。
	// 取消时只停止本地等待，不去中断设备端的生成任务。
	Collect(ctx context.Context, j *Job) (*Result, error)
}
