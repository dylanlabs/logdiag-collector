// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

// ErrTimeout 表示等待设备超过了设定的时间。
var ErrTimeout = errors.New("等待设备超时")

// ErrUnsupported 表示设备没有驱动需要的接口，驱动用 %w 包装它。
var ErrUnsupported = errors.New("设备不支持此操作")

// Kind 是面向用户的失败分类，每类都对应一条“该怎么做”。
type Kind int

const (
	KindUnknown     Kind = iota
	KindNetwork          // 网络不可达
	KindAuth             // 认证失败
	KindPermission       // 权限不足
	KindUnsupported      // 接口不支持
	KindBusy             // 设备忙
	KindTimeout          // 超时
	KindTaskFailed       // 设备端任务失败
	KindCanceled         // 用户取消
	KindHostKey          // SSH 主机密钥和记录的不一致
)

var kindText = map[Kind][2]string{
	KindUnknown:     {"未知错误", "导出诊断记录发给开发者。"},
	KindNetwork:     {"网络不可达", "检查管理口 IP、网线和防火墙，确认本机能访问设备的 HTTPS 端口。"},
	KindAuth:        {"认证失败", "检查账号和密码。为避免账号被锁定，工具不会自动重试。"},
	KindPermission:  {"权限不足", "换用管理员权限的账号。"},
	KindUnsupported: {"接口不支持", "该设备或固件版本不支持此操作，可尝试升级 BMC 固件，或导出诊断记录发给开发者。"},
	KindBusy:        {"设备忙", "设备正在执行其他任务，稍后再试。"},
	KindTimeout:     {"超时", "设备生成日志较慢，可调大超时时间后重试。"},
	KindTaskFailed:  {"设备端任务失败", "查看设备返回的信息；如反复失败，导出诊断记录发给开发者。"},
	KindCanceled:    {"已取消", "设备端可能仍在生成，稍后可再次收集。"},
	KindHostKey:     {"主机密钥不符", "服务器的 SSH 主机密钥和上次记录的不一样：可能重装过系统，也可能有人冒充这台服务器。确认无误后在设备里清除记录的主机密钥再试。"},
}

// String 返回分类名称。
func (k Kind) String() string { return kindText[k][0] }

// Hint 返回给用户的处理建议。
func (k Kind) Hint() string { return kindText[k][1] }

// KindOf 判断错误属于哪一类。
func KindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	var (
		httpErr *HTTPError
		taskErr *TaskError
		netErr  net.Error
		urlErr  *url.Error
		hostKey *sshx.HostKeyError
	)
	switch {
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return KindTimeout
	case errors.Is(err, context.Canceled):
		return KindCanceled
	case errors.Is(err, ErrUnsupported):
		return KindUnsupported
	case errors.As(err, &taskErr):
		return KindTaskFailed
	case errors.Is(err, sshx.ErrAuth):
		return KindAuth
	case errors.As(err, &hostKey):
		return KindHostKey
	case errors.Is(err, sshx.ErrNoPassword), errors.Is(err, sshx.ErrAlgorithms):
		return KindUnsupported
	case errors.As(err, &httpErr):
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			return KindAuth
		case http.StatusForbidden:
			return KindPermission
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
			return KindUnsupported
		case http.StatusConflict, http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return KindBusy
		case http.StatusGatewayTimeout:
			return KindTimeout
		}
		return KindUnknown
	case errors.As(err, &netErr) && netErr.Timeout():
		return KindTimeout
	case errors.As(err, &urlErr), errors.As(err, &netErr):
		return KindNetwork
	}
	return KindUnknown
}

// HTTPError 是设备返回的 4xx/5xx 响应。
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
	MessageID  string // Redfish @Message.ExtendedInfo 里第一条的 MessageId
	Message    string
}

func (e *HTTPError) Error() string {
	s := fmt.Sprintf("%s %s 返回 %d %s", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		s += "：" + e.Message
	}
	if e.MessageID != "" {
		s += "（" + e.MessageID + "）"
	}
	return s
}

func newHTTPError(method, path string, resp *Response) *HTTPError {
	e := &HTTPError{Method: method, Path: path, StatusCode: resp.StatusCode}
	var body struct {
		Error struct {
			Code    string    `json:"code"`
			Message string    `json:"message"`
			Info    []Message `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if json.Unmarshal(resp.Body, &body) == nil {
		e.Message = body.Error.Message
		e.MessageID = body.Error.Code
		if len(body.Error.Info) > 0 {
			if m := body.Error.Info[0]; m.Message != "" || m.MessageID != "" {
				e.Message, e.MessageID = m.Message, m.MessageID
			}
		}
	}
	return e
}

// TaskError 表示设备端任务以失败状态结束。
type TaskError struct {
	URI      string
	State    TaskState
	Status   string
	Messages []string
}

func (e *TaskError) Error() string {
	s := fmt.Sprintf("设备端任务 %s 以 %s 状态结束", e.URI, e.State)
	if len(e.Messages) > 0 {
		s += "：" + strings.Join(e.Messages, "；")
	}
	return s
}
