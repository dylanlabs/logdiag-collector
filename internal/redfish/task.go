// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TaskState 是 Redfish Task 的 TaskState。
type TaskState string

const (
	TaskNew         TaskState = "New"
	TaskStarting    TaskState = "Starting"
	TaskRunning     TaskState = "Running"
	TaskSuspended   TaskState = "Suspended"
	TaskInterrupted TaskState = "Interrupted"
	TaskPending     TaskState = "Pending"
	TaskStopping    TaskState = "Stopping"
	TaskCompleted   TaskState = "Completed"
	TaskKilled      TaskState = "Killed"
	TaskException   TaskState = "Exception"
	TaskService     TaskState = "Service"
	TaskCancelling  TaskState = "Cancelling"
	TaskCancelled   TaskState = "Cancelled"
)

// Terminal 报告任务是否已经结束。
func (s TaskState) Terminal() bool {
	switch s {
	case TaskCompleted, TaskKilled, TaskException, TaskCancelled:
		return true
	}
	return false
}

// Message 是 Redfish 消息。
type Message struct {
	MessageID string      `json:"MessageId"`
	Message   string      `json:"Message"`
	Severity  string      `json:"Severity,omitempty"`
	Args      MessageArgs `json:"MessageArgs,omitempty"`
}

// MessageArgs 是消息参数。有的设备在参数里放数字，统一转成字符串。
type MessageArgs []string

func (a *MessageArgs) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(MessageArgs, 0, len(raw))
	for _, r := range raw {
		var s string
		if json.Unmarshal(r, &s) != nil {
			s = string(r)
		}
		out = append(out, s)
	}
	*a = out
	return nil
}

// Messages 是任务消息列表。华为 iBMC 有消息时返回单个对象而不是数组，这里都接受。
type Messages []Message

func (m *Messages) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '{' {
		var one Message
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*m = Messages{one}
		return nil
	}
	var list []Message
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*m = list
	return nil
}

// Task 是任务资源（DMTF Task，或 Dell 的 Job）。
type Task struct {
	ODataID         string `json:"@odata.id"`
	ID              string `json:"Id"`
	Name            string
	TaskState       TaskState
	TaskStatus      string
	PercentComplete *int
	TaskMonitor     string
	Messages        Messages
	// Oem 里可能有厂商自己的进度，例如华为的 Oem.Huawei.TaskPercentage（"50%"）。
	Oem map[string]json.RawMessage

	// Dell iDRAC 的 Job 资源用 JobState / Message 表示状态。
	JobState string
	Message  string
}

// State 返回统一后的任务状态。
func (t *Task) State() TaskState {
	if t.TaskState != "" {
		return t.TaskState
	}
	switch t.JobState {
	case "Completed", "CompletedWithErrors":
		return TaskCompleted
	case "Failed", "RebootFailed":
		return TaskException
	case "":
		return ""
	}
	return TaskRunning
}

// Percent 返回完成百分比，不知道时为 -1。
func (t *Task) Percent() int {
	if t.PercentComplete != nil {
		return *t.PercentComplete
	}
	for _, raw := range t.Oem {
		var v struct{ TaskPercentage json.RawMessage }
		if json.Unmarshal(raw, &v) != nil || len(v.TaskPercentage) == 0 {
			continue
		}
		if p, ok := parsePercent(v.TaskPercentage); ok {
			return p
		}
	}
	return -1
}

// parsePercent 解析 50、"50"、"50%" 这几种写法。
func parsePercent(raw json.RawMessage) (int, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if err != nil || n < 0 || n > 100 {
		return 0, false
	}
	return n, true
}

// Text 返回最后一条设备消息。
func (t *Task) Text() string {
	if n := len(t.Messages); n > 0 {
		return t.Messages[n-1].Message
	}
	return t.Message
}

func (t *Task) messages() []string {
	var out []string
	for _, m := range t.Messages {
		if m.Message != "" {
			out = append(out, m.Message)
		} else if m.MessageID != "" {
			out = append(out, m.MessageID)
		}
	}
	if len(out) == 0 && t.Message != "" {
		out = append(out, t.Message)
	}
	return out
}

// parseTask 在响应体是任务资源时返回它，否则返回 nil。
func parseTask(resp *Response) *Task {
	if len(resp.Body) == 0 {
		return nil
	}
	var t Task
	if json.Unmarshal(resp.Body, &t) != nil || t.State() == "" {
		return nil
	}
	return &t
}

// ActionResult 是调用动作后的结果。Monitor 为空表示动作已同步完成。
type ActionResult struct {
	Response *Response
	Task     *Task
	Monitor  string
}

// StartAction 调用动作（POST），返回需要轮询的任务地址。
//
// 任务地址依次取 Location 头、任务资源的 TaskMonitor、任务资源的 @odata.id。
// Dell 的 Location 是 Job 地址（iDRAC10 是 TaskService 任务），完成时的响应头里才有文件地址。
func (c *Client) StartAction(ctx context.Context, target string, body any) (*ActionResult, error) {
	resp, err := c.Post(ctx, target, body)
	if err != nil {
		return nil, err
	}
	res := &ActionResult{Response: resp, Task: parseTask(resp)}
	if loc := resp.Location(); loc != "" && resp.StatusCode == http.StatusAccepted {
		res.Monitor = loc
	} else if res.Task != nil && !res.Task.State().Terminal() {
		res.Monitor = pathOf(firstNonEmpty(res.Task.TaskMonitor, res.Task.ODataID))
	}
	if res.Monitor == "" && res.Task == nil && resp.StatusCode == http.StatusAccepted {
		return nil, fmt.Errorf("设备接受了 %s，但没有给出任务地址", pathOf(target))
	}
	return res, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Progress 是轮询时报告给界面的进度。
type Progress struct {
	State   TaskState
	Percent int // -1 表示设备没有给出
	Message string
}

// PollOptions 控制任务轮询。
type PollOptions struct {
	// Interval 是默认轮询间隔，默认 5 秒。设备返回 Retry-After 时按设备要求，但不超过 MaxInterval。
	Interval time.Duration
	// MaxInterval 默认 30 秒。
	MaxInterval time.Duration
	// Timeout 是等待上限，0 表示只受 ctx 控制。生成日志从几分钟到几十分钟不等，按驱动设置。
	Timeout time.Duration
	// MaxErrors 是连续网络错误或 5xx 的容忍次数，默认 3。
	MaxErrors int
	// OnProgress 在状态、百分比或消息变化时调用。
	OnProgress func(Progress)
}

func (o PollOptions) withDefaults() PollOptions {
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.MaxInterval <= 0 {
		o.MaxInterval = 30 * time.Second
	}
	if o.MaxErrors <= 0 {
		o.MaxErrors = 3
	}
	return o
}

// TaskResult 是任务结束时的结果。
type TaskResult struct {
	// Task 是最后一次读到的任务资源；任务监视器直接返回操作结果时为 nil。
	Task *Task
	// Response 是结束时的响应，驱动从它的 Location 头或响应体里找生成的文件。
	Response *Response
}

// Location 返回结束时响应的 Location 头，通常指向生成的日志条目或文件。
// 有的设备只在任务消息里给出文件路径，驱动需要自己从 Task.Messages 里取。
func (r *TaskResult) Location() string {
	if r.Response == nil {
		return ""
	}
	return r.Response.Location()
}

// WaitTask 轮询任务直到结束。
//
// 任务监视器返回 202 表示仍在运行；返回 2xx 且响应体不是任务资源，表示操作已完成，
// 响应就是操作结果；响应体是任务资源时按 TaskState 判断。
// 取消或超时只停止本地等待，不会去中断设备端任务。
func (c *Client) WaitTask(ctx context.Context, monitor string, opts PollOptions) (*TaskResult, error) {
	opts = opts.withDefaults()
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, opts.Timeout, ErrTimeout)
		defer cancel()
	}
	monitor = pathOf(monitor)
	last := Progress{Percent: -2}
	errCount := 0

	for {
		resp, err := c.send(ctx, http.MethodGet, monitor, nil, c.authToken())
		if ctx.Err() != nil {
			return nil, waitErr(ctx, monitor, opts.Timeout)
		}
		var transient error
		switch {
		case err != nil:
			transient = err
		case resp.StatusCode >= 500:
			transient = newHTTPError(http.MethodGet, monitor, resp)
		case resp.StatusCode >= 300:
			return nil, newHTTPError(http.MethodGet, monitor, resp)
		}
		if transient != nil {
			errCount++
			if errCount > opts.MaxErrors {
				return nil, fmt.Errorf("轮询任务 %s 连续失败 %d 次：%w", monitor, errCount, transient)
			}
			if err := c.sleep(ctx, opts.Interval); err != nil {
				return nil, waitErr(ctx, monitor, opts.Timeout)
			}
			continue
		}
		errCount = 0

		task := parseTask(resp)
		if task == nil && resp.StatusCode != http.StatusAccepted {
			return &TaskResult{Response: resp}, nil
		}
		if task != nil {
			if st := task.State(); st.Terminal() {
				res := &TaskResult{Task: task, Response: resp}
				if st != TaskCompleted {
					return res, &TaskError{URI: monitor, State: st, Status: task.TaskStatus, Messages: task.messages()}
				}
				return res, nil
			}
		}

		p := Progress{State: TaskRunning, Percent: -1}
		if task != nil {
			p = Progress{State: task.State(), Percent: task.Percent(), Message: task.Text()}
		}
		if p != last && opts.OnProgress != nil {
			opts.OnProgress(p)
		}
		last = p

		delay := opts.Interval
		if ra := retryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			delay = min(max(ra, time.Second), opts.MaxInterval)
		}
		if err := c.sleep(ctx, delay); err != nil {
			return nil, waitErr(ctx, monitor, opts.Timeout)
		}
	}
}

// RunAction 调用动作并等待任务结束。
func (c *Client) RunAction(ctx context.Context, target string, body any, opts PollOptions) (*TaskResult, error) {
	res, err := c.StartAction(ctx, target, body)
	if err != nil {
		return nil, err
	}
	if res.Monitor == "" {
		return &TaskResult{Task: res.Task, Response: res.Response}, nil
	}
	return c.WaitTask(ctx, res.Monitor, opts)
}

func waitErr(ctx context.Context, monitor string, timeout time.Duration) error {
	if errors.Is(context.Cause(ctx), ErrTimeout) {
		return fmt.Errorf("任务 %s 超过 %s 仍未完成：%w", monitor, timeout, ErrTimeout)
	}
	return ctx.Err()
}

// retryAfter 解析 Retry-After（秒数或 HTTP 日期）。
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}
