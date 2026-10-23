// SPDX-License-Identifier: GPL-3.0-or-later

package httprec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Redacted 替换被去掉的凭据。
const Redacted = "REDACTED"

// 整个值都要去掉的请求头和响应头（规范化写法）。
var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"X-Auth-Token":        true,
	"X-Csrftoken":         true, // 浪潮网页接口
	"X-Csrf-Token":        true,
}

// 字段名包含这些词（不区分大小写）时，值视为凭据。
var sensitiveWords = []string{"password", "passwd", "passphrase", "secret", "token", "csrf", "community", "privatekey", "sessionkey"}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, w := range sensitiveWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// addr 是被录制设备的地址，用来把它替换成占位符。
type addr struct {
	base string // https://10.0.0.1:8443
	host string // 10.0.0.1
}

func (a addr) scrub(s string) string {
	if a.base != "" {
		s = strings.ReplaceAll(s, a.base, BasePlaceholder)
	}
	if a.host != "" {
		s = replaceHost(s, a.host, HostPlaceholder)
	}
	return s
}

// replaceHost 只替换完整出现的主机名，避免把 10.0.0.10 里的 10.0.0.1 换掉。
func replaceHost(s, host, repl string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, host)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(host)
		if (i > 0 && isHostChar(s[i-1])) || (end < len(s) && isHostChar(s[end])) {
			b.WriteString(s[:end])
		} else {
			b.WriteString(s[:i])
			b.WriteString(repl)
		}
		s = s[end:]
	}
}

func isHostChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-'
}

func scrubHeader(h http.Header, a addr) http.Header {
	if len(h) == 0 {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		ck := http.CanonicalHeaderKey(k)
		for _, v := range vs {
			if sensitiveHeaders[ck] {
				v = Redacted
			} else {
				v = a.scrub(v)
			}
			out[ck] = append(out[ck], v)
		}
	}
	return out
}

// scrubPath 去掉查询串里的凭据参数，路径本身保持不变。
func scrubPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery == "" {
		return p
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return p + "?" + u.RawQuery
	}
	changed := false
	for k := range q {
		if isSensitiveKey(k) {
			q[k] = []string{Redacted}
			changed = true
		}
	}
	if !changed {
		return p + "?" + u.RawQuery
	}
	return p + "?" + q.Encode()
}

// scrubBody 去掉 JSON 和表单里的凭据字段，再替换设备地址。
func scrubBody(data []byte, contentType string, a addr) []byte {
	if len(data) == 0 {
		return data
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "json") || (ct == "" && json.Valid(data)):
		data = redactJSON(data)
	case strings.Contains(ct, "x-www-form-urlencoded"):
		data = redactForm(data)
	}
	return []byte(a.scrub(string(data)))
}

func redactJSON(data []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return data
	}
	if !redactValue(v) {
		return data
	}
	out, err := json.Marshal(v)
	if err != nil {
		return data
	}
	return out
}

// redactValue 原地替换凭据字段，返回是否有改动。
func redactValue(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			// 只替换字符串：PasswordChangeRequired 这类布尔值和数字不是凭据，保留下来便于排查。
			if s, isStr := child.(string); isStr && s != "" && isSensitiveKey(k) {
				t[k] = Redacted
				changed = true
				continue
			}
			if redactValue(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range t {
			if redactValue(child) {
				changed = true
			}
		}
	}
	return changed
}

func redactForm(data []byte) []byte {
	q, err := url.ParseQuery(string(data))
	if err != nil {
		return data
	}
	changed := false
	for k := range q {
		if isSensitiveKey(k) {
			q[k] = []string{Redacted}
			changed = true
		}
	}
	if !changed {
		return data
	}
	return []byte(q.Encode())
}
