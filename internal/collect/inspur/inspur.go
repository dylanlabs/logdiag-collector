// SPDX-License-Identifier: GPL-3.0-or-later

// Package inspur 是浪潮 M5/M6/M7 一键日志驱动。
//
// 浪潮的一键日志不走 Redfish，而是调用 BMC 网页自己的 REST 接口。各代流程依据浪潮官方
// Python SDK inspursmsdk 2.4.0 的接口行为（该 SDK 为 GPL-3.0，这里只参考接口、不复制代码）：
//
//   - M6/M7：GET /api/logs/onekeylog/progress 查看进度（别人正在生成就等它完成），
//     否则 POST /api/logs/onekeylog/trigger；轮询到 rate 为 100，再 GET /api/logs/onekeylog/logfile（.tar.gz）。
//   - M5 通用：GET /api/logs/onekeylog 触发，设备不报告进度，固定等待约 5 分钟后 GET /tmp/onekeylog.tar。
//   - NF5280M5 一系老固件：GET /api/logs/onekeylog 后 POST /api/logs/exportonekeylog，
//     等待后 GET /api/logs/getonekeylog。
//   - NF5280M5 一系 4.34 起的固件（或上面的接口返回 404 时）：POST /api/triggeronekeylog，
//     每 15 秒 POST /api/checkonekeylogexist，FileName 出现后下载该文件。
//
// 目前只支持账号密码直接登录；要求网页登录加密的固件会在登录时失败（认证失败，不会重试）。
// 每种流程都要在真机上核对。
package inspur

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

const (
	// PollInterval 是 M6/M7 查询进度的间隔，与官方 SDK 相同。
	PollInterval = 5 * time.Second
	// GenerateTimeout 是等待生成的上限，官方 SDK 为 10 分钟，这里留出余量。
	GenerateTimeout = 20 * time.Minute
	// M5Wait 是 M5 不报告进度时的固定等待时间，与官方 SDK 相同。
	M5Wait = 5 * time.Minute
	// CheckInterval 是 NF5280M5 新固件检查文件是否生成的间隔，与官方 SDK 相同。
	CheckInterval = 15 * time.Second

	statusGenerating = 254 // progress 接口里表示正在生成的 status
	maxErrors        = 3
)

// exportModels 是走 exportonekeylog 或 triggeronekeylog 流程的 M5 型号（官方 SDK 的 NF5280M5 一系）。
var exportModels = []string{"NF5280M5", "SA5212M5", "NS3160A6"}

// Driver 是浪潮 BMC 驱动。
type Driver struct{}

func (Driver) Name() string { return "inspur-bmc" }

func (Driver) Collect(ctx context.Context, j *collect.Job) (*collect.Result, error) {
	// 识别设备时建的 Redfish 会话用不到了，先释放，免得占用 BMC 的会话数。
	if j.Redfish != nil {
		j.Redfish.Logout(ctx)
	}
	if j.Device.Vendor == "" {
		j.Device.Vendor = "Inspur"
	}
	if j.Device.Controller.Type == "" {
		j.Device.Controller.Type = "BMC"
	}

	w, err := newWeb(j.BaseURL, j.HTTPClient, j.UserAgent)
	if err != nil {
		return nil, err
	}
	j.Report(collect.StageProbe, -1, "登录 BMC 网页接口")
	if err := w.login(ctx, j.User, j.Password); err != nil {
		return nil, fmt.Errorf("登录 BMC 网页接口：%w", err)
	}
	defer w.logout(ctx)

	c := &run{j: j, w: w}
	var p progress
	err = w.call(ctx, "GET", "/api/logs/onekeylog/progress", nil, &p)
	switch {
	case err == nil:
		return c.m6(ctx, p)
	case redfish.KindOf(err) != redfish.KindUnsupported:
		return nil, fmt.Errorf("查询一键日志进度：%w", err)
	}
	return c.m5(ctx)
}

type progress struct {
	Rate   int `json:"rate"`
	Status int `json:"status"`
}

type run struct {
	j *collect.Job
	w *web
}

var log = collect.Log{Type: "inspur.bmc.onekeylog", Method: "bmc-web-api"}

// m6 是 M6/M7 的流程：触发、按进度轮询、下载 .tar.gz。
func (c *run) m6(ctx context.Context, p progress) (*collect.Result, error) {
	j := c.j
	if p.Status == statusGenerating && p.Rate != 100 {
		j.Report(collect.StageGenerate, p.Rate, "设备上已有一键日志正在生成，等它完成后直接下载")
	} else {
		j.Report(collect.StageTrigger, -1, "触发一键日志")
		if err := c.w.call(ctx, "POST", "/api/logs/onekeylog/trigger", struct{}{}, nil); err != nil {
			return nil, fmt.Errorf("触发一键日志：%w", err)
		}
		p = progress{Rate: -1}
	}

	if err := c.waitDone(ctx, p); err != nil {
		return nil, err
	}
	if err := j.Wait(ctx, 3*time.Second); err != nil {
		return nil, err
	}
	return c.save(ctx, "/api/logs/onekeylog/logfile", "onekeylog.tar.gz")
}

// waitDone 轮询 M6/M7 的进度直到生成完成。
func (c *run) waitDone(parent context.Context, p progress) error {
	j := c.j
	ctx, cancel := context.WithTimeoutCause(parent, GenerateTimeout, redfish.ErrTimeout)
	defer cancel()
	seenRunning := p.Rate >= 0
	errCount := 0
	for polls := 1; ; polls++ {
		if err := j.Wait(ctx, PollInterval); err != nil {
			return waitErr(ctx, err)
		}
		var cur progress
		if err := c.w.call(ctx, "GET", "/api/logs/onekeylog/progress", nil, &cur); err != nil {
			if ctx.Err() != nil {
				return waitErr(ctx, err)
			}
			if k := redfish.KindOf(err); k == redfish.KindAuth || k == redfish.KindPermission || k == redfish.KindUnsupported {
				return fmt.Errorf("查询一键日志进度：%w", err)
			}
			if errCount++; errCount > maxErrors {
				return fmt.Errorf("查询一键日志进度连续失败 %d 次：%w", errCount, err)
			}
			continue
		}
		errCount = 0
		running := cur.Status == statusGenerating || cur.Rate < 100
		if running {
			seenRunning = true
		}
		// 刚触发时进度可能还停在上一次的 100，至少看到一次“生成中”或等满 3 次再认定完成。
		if !running && (seenRunning || polls >= 3) {
			return nil
		}
		if cur != p {
			j.Report(collect.StageGenerate, max(cur.Rate, -1), "设备正在生成一键日志")
			p = cur
		}
	}
}

// m5 是 M5 的流程，按型号和固件版本选官方 SDK 对应的接口。
func (c *run) m5(ctx context.Context) (*collect.Result, error) {
	j := c.j
	family := slices.ContainsFunc(exportModels, func(m string) bool {
		return strings.EqualFold(strings.ReplaceAll(j.Device.Model, " ", ""), m)
	})
	if family && firmwareAtLeast(j.Device.Controller.Firmware, 4, 34) {
		return c.m5Check(ctx)
	}

	j.Report(collect.StageTrigger, -1, "触发一键日志")
	err := c.w.call(ctx, "GET", "/api/logs/onekeylog", nil, nil)
	if err != nil {
		if family && redfish.KindOf(err) == redfish.KindUnsupported {
			return c.m5Check(ctx)
		}
		return nil, fmt.Errorf("触发一键日志：%w", err)
	}
	file, name := "/tmp/onekeylog.tar", "onekeylog.tar"
	if family {
		if err := c.w.call(ctx, "POST", "/api/logs/exportonekeylog", nil, nil); err != nil {
			return nil, fmt.Errorf("导出一键日志：%w", err)
		}
		file = "/api/logs/getonekeylog"
	}
	if err := c.fixedWait(ctx); err != nil {
		return nil, err
	}
	return c.save(ctx, file, name)
}

// fixedWait 等待 M5Wait。M5 不报告进度，官方 SDK 也是固定等待。
func (c *run) fixedWait(ctx context.Context) error {
	const step = 30 * time.Second
	for left := M5Wait; left > 0; left -= step {
		c.j.Report(collect.StageGenerate, -1,
			fmt.Sprintf("这一代设备不报告进度，按官方工具的做法等待，剩余约 %d 分钟", int((left+time.Minute-1)/time.Minute)))
		if err := c.j.Wait(ctx, min(step, left)); err != nil {
			return err
		}
	}
	return nil
}

// m5Check 是 NF5280M5 一系新固件的流程：触发后轮询文件是否生成。
func (c *run) m5Check(ctx context.Context) (*collect.Result, error) {
	j := c.j
	j.Report(collect.StageTrigger, -1, "触发一键日志")
	if err := c.w.call(ctx, "POST", "/api/triggeronekeylog", nil, nil); err != nil {
		return nil, fmt.Errorf("触发一键日志：%w", err)
	}
	j.Report(collect.StageGenerate, -1, "设备正在生成一键日志")
	file, err := c.waitFile(ctx)
	if err != nil {
		return nil, err
	}
	return c.save(ctx, file, path.Base(onlyPath(file)))
}

// waitFile 每 CheckInterval 检查一次文件是否生成，返回文件在设备上的路径。
func (c *run) waitFile(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeoutCause(parent, GenerateTimeout, redfish.ErrTimeout)
	defer cancel()
	errCount := 0
	for {
		if err := c.j.Wait(ctx, CheckInterval); err != nil {
			return "", waitErr(ctx, err)
		}
		var st struct {
			FileExistFlag int
			FileName      string
		}
		if err := c.w.call(ctx, "POST", "/api/checkonekeylogexist", nil, &st); err != nil {
			if ctx.Err() != nil {
				return "", waitErr(ctx, err)
			}
			if errCount++; errCount > maxErrors {
				return "", fmt.Errorf("检查一键日志连续失败 %d 次：%w", errCount, err)
			}
			continue
		}
		errCount = 0
		switch {
		case st.FileExistFlag == 1 && st.FileName != "":
			return st.FileName, nil
		case st.FileExistFlag == 0 && st.FileName == "":
			continue
		}
		return "", fmt.Errorf("设备报告一键日志状态异常（FileExistFlag=%d，FileName=%q）", st.FileExistFlag, st.FileName)
	}
}

func (c *run) save(ctx context.Context, file, name string) (*collect.Result, error) {
	c.j.Report(collect.StageDownload, -1, "下载 "+onlyPath(file))
	return c.j.Save(log, name, func(dst io.Writer) error {
		if err := c.w.download(ctx, file, dst); err != nil {
			return fmt.Errorf("下载一键日志：%w", err)
		}
		return nil
	})
}

func waitErr(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), redfish.ErrTimeout) {
		return fmt.Errorf("一键日志超过 %s 仍未生成：%w", GenerateTimeout, redfish.ErrTimeout)
	}
	return err
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

// firmwareAtLeast 比较 BMC 固件的主、次版本号，例如 "4.35.0" ≥ 4.34。无法解析时返回 false。
func firmwareAtLeast(v string, major, minor int) bool {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a > major || a == major && b >= minor
}
