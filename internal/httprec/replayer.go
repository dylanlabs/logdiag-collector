// SPDX-License-Identifier: GPL-3.0-or-later

package httprec

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Replayer 是按录制记录返回响应的 http.RoundTripper，不访问网络。
//
// 请求按“方法 + 路径（含查询串）”匹配；同一个键出现多次时按录制顺序依次返回，
// 所以轮询同一个任务地址会依次拿到“运行中 10%”“运行中 60%”“已完成”。
type Replayer struct {
	mu     sync.Mutex
	queues map[string][]*Interaction
	missed []string
}

// NewReplayer 用录制记录创建回放器。
func NewReplayer(c *Cassette) *Replayer {
	p := &Replayer{queues: make(map[string][]*Interaction)}
	for _, it := range c.Interactions {
		k := key(it.Request.Method, it.Request.Path)
		p.queues[k] = append(p.queues[k], it)
	}
	return p
}

// LoadReplayer 读取录制文件并创建回放器。
func LoadReplayer(path string) (*Replayer, error) {
	c, err := Load(path)
	if err != nil {
		return nil, err
	}
	return NewReplayer(c), nil
}

func key(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

// RoundTrip 实现 http.RoundTripper。
func (p *Replayer) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		io.Copy(io.Discard, req.Body)
		req.Body.Close()
	}
	a := addr{host: req.URL.Hostname()}
	k := key(req.Method, a.scrub(scrubPath(req.URL)))

	p.mu.Lock()
	q := p.queues[k]
	if len(q) == 0 {
		p.missed = append(p.missed, k)
		p.mu.Unlock()
		return nil, fmt.Errorf("httprec: 录制记录里没有 %s 的（更多）响应", k)
	}
	it := q[0]
	p.queues[k] = q[1:]
	p.mu.Unlock()

	if it.Error != "" {
		return nil, fmt.Errorf("httprec: 录制时的网络错误：%s", it.Error)
	}
	body, err := it.Response.Body.Bytes()
	if err != nil {
		return nil, err
	}
	base := req.URL.Scheme + "://" + req.URL.Host
	fill := strings.NewReplacer(BasePlaceholder, base, HostPlaceholder, req.URL.Hostname())
	body = []byte(fill.Replace(string(body)))

	header := make(http.Header, len(it.Response.Header))
	for k, vs := range it.Response.Header {
		for _, v := range vs {
			header.Add(k, fill.Replace(v))
		}
	}
	// 替换占位符后长度会变，以实际返回的内容为准。
	header.Del("Content-Length")
	status := it.Response.Status
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// Remaining 返回还没被请求过的录制条目，用于检查流程是否走完。
func (p *Replayer) Remaining() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k, q := range p.queues {
		for range q {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Missed 返回录制记录里找不到的请求。
func (p *Replayer) Missed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.missed...)
}
