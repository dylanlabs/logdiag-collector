// SPDX-License-Identifier: GPL-3.0-or-later

// Package httprec 录制和回放 HTTP 往返。
//
// 同一份录制记录有两个用途：
//   - 诊断记录：用户收集失败时导出发回，开发者不用到现场就能看到设备的真实响应；
//   - 回放测试：把记录放进 testdata，用 Replayer 代替真实设备做自动回归。
//
// 录制时会去掉密码、会话令牌、Cookie 等凭据，并把设备地址替换成 {{base}} / {{host}}。
// 设备型号、序列号、主机名等信息仍会保留，发送前需要用户确认。
package httprec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"
)

// Format 是录制文件的格式标识。
const Format = "logdiag.httprec/1"

// 回放时替换成实际地址的占位符。
const (
	BasePlaceholder = "{{base}}" // scheme://host[:port]
	HostPlaceholder = "{{host}}" // 不含端口的主机名或 IP
)

// Cassette 是一次会话的全部 HTTP 往返。
type Cassette struct {
	Format string `json:"format"`
	// Note 说明记录来源，例如“真机录制：Dell R750 iDRAC9 7.10”或“合成：按 DMTF 文档编写”。
	Note         string         `json:"note,omitempty"`
	Tool         string         `json:"tool,omitempty"`
	RecordedAt   string         `json:"recorded_at,omitempty"`
	Interactions []*Interaction `json:"interactions"`
}

// Interaction 是一次请求和它的响应。Error 非空表示请求没有拿到响应（如连接被拒绝）。
type Interaction struct {
	Request    Request  `json:"request"`
	Response   Response `json:"response,omitzero"`
	Error      string   `json:"error,omitempty"`
	DurationMS int64    `json:"duration_ms,omitempty"`
}

// Request 只保存路径和查询串，不保存主机。
type Request struct {
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Header http.Header `json:"header,omitempty"`
	Body   Body        `json:"body,omitzero"`
}

// Response 是设备返回的状态码、响应头和响应体。
type Response struct {
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
	Body   Body        `json:"body,omitzero"`
}

// Body 按内容选择一种保存形式：JSON 原样嵌入，文本存字符串，二进制存 base64。
// 超过录制上限的响应体只保存开头部分，并记下完整大小和 SHA-256。
type Body struct {
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
	Base64    string          `json:"base64,omitempty"`
	Size      int64           `json:"size,omitempty"`
	SHA256    string          `json:"sha256,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// Bytes 返回保存的响应体内容。
func (b Body) Bytes() ([]byte, error) {
	switch {
	case len(b.JSON) > 0:
		return []byte(b.JSON), nil
	case b.Base64 != "":
		data, err := base64.StdEncoding.DecodeString(b.Base64)
		if err != nil {
			return nil, fmt.Errorf("httprec: base64 响应体无法解码：%w", err)
		}
		return data, nil
	default:
		return []byte(b.Text), nil
	}
}

// newBody 选择保存形式。complete 为 false 时内容只是开头部分，不能当 JSON 保存。
func newBody(data []byte, contentType string, complete bool) Body {
	if len(data) == 0 {
		return Body{}
	}
	if complete && isJSONType(contentType) && json.Valid(data) {
		var buf bytes.Buffer
		if json.Compact(&buf, data) == nil {
			return Body{JSON: buf.Bytes()}
		}
	}
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		return Body{Text: string(data)}
	}
	return Body{Base64: base64.StdEncoding.EncodeToString(data)}
}

func isJSONType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "json")
}

// Load 读取录制文件。
func Load(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("httprec: %s 不是有效的录制文件：%w", path, err)
	}
	if c.Format != Format {
		return nil, fmt.Errorf("httprec: %s 的格式是 %q，需要 %q", path, c.Format, Format)
	}
	return &c, nil
}

// Save 把录制记录写入文件。
func (c *Cassette) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
