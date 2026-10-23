// SPDX-License-Identifier: GPL-3.0-or-later

package httprec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultMaxBody 是完整保存响应体的上限。
const DefaultMaxBody = 256 << 10

// 超过上限的响应体（通常是日志包本身）只保存开头这么多字节，够看出文件类型，
// 不把客户日志内容带进诊断记录。
const previewBytes = 512

// Recorder 是记录所有往返的 http.RoundTripper，可以同时被多个 goroutine 使用。
//
// 响应体在调用方读取时边读边记，不会为了录制把整个日志包读进内存。
// 保存前要先关闭所有响应体，否则对应条目只记到已读取的部分。
type Recorder struct {
	// Transport 是实际发送请求的传输层，为 nil 时使用 http.DefaultTransport。
	Transport http.RoundTripper
	// MaxBody 是完整保存响应体的字节数上限，0 表示 DefaultMaxBody。
	MaxBody int

	mu sync.Mutex
	c  Cassette
}

// NewRecorder 返回包装 rt 的录制器。
func NewRecorder(rt http.RoundTripper) *Recorder {
	return &Recorder{Transport: rt}
}

// RoundTrip 实现 http.RoundTripper。
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := r.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}

	var reqBody []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		// RoundTripper 不应修改调用方的请求，所以复制一份再换上读过的请求体。
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(reqBody)), nil }
	}

	a := addr{base: req.URL.Scheme + "://" + req.URL.Host, host: req.URL.Hostname()}
	it := &Interaction{Request: Request{
		Method: req.Method,
		Path:   a.scrub(scrubPath(req.URL)),
		Header: scrubHeader(req.Header, a),
		Body:   newBody(scrubBody(reqBody, req.Header.Get("Content-Type"), a), req.Header.Get("Content-Type"), true),
	}}
	r.mu.Lock()
	r.c.Interactions = append(r.c.Interactions, it)
	r.mu.Unlock()

	start := time.Now()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		r.mu.Lock()
		it.Error = a.scrub(err.Error())
		it.DurationMS = time.Since(start).Milliseconds()
		r.mu.Unlock()
		return nil, err
	}

	r.mu.Lock()
	it.Response.Status = resp.StatusCode
	it.Response.Header = scrubHeader(resp.Header, a)
	r.mu.Unlock()

	limit := r.MaxBody
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	resp.Body = &tapBody{
		rc:    resp.Body,
		rec:   r,
		it:    it,
		addr:  a,
		ct:    resp.Header.Get("Content-Type"),
		limit: limit,
		start: start,
		sum:   sha256.New(),
	}
	return resp, nil
}

// Cassette 返回目前录到的内容。
func (r *Recorder) Cassette() *Cassette {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.c
	c.Format = Format
	c.Interactions = append([]*Interaction(nil), r.c.Interactions...)
	return &c
}

// tapBody 在调用方读取响应体时记录开头部分、总大小和 SHA-256。
type tapBody struct {
	rc    io.ReadCloser
	rec   *Recorder
	it    *Interaction
	addr  addr
	ct    string
	limit int
	start time.Time

	buf  []byte
	size int64
	sum  hash.Hash
	eof  bool
	once sync.Once
}

func (t *tapBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.sum.Write(p[:n])
		t.size += int64(n)
		if room := t.limit - len(t.buf); room > 0 {
			t.buf = append(t.buf, p[:min(n, room)]...)
		}
	}
	if err == io.EOF {
		t.eof = true
		t.finish()
	}
	return n, err
}

func (t *tapBody) Close() error {
	err := t.rc.Close()
	t.finish()
	return err
}

func (t *tapBody) finish() {
	t.once.Do(func() {
		complete := t.eof && int64(len(t.buf)) == t.size
		var body Body
		if complete {
			body = newBody(scrubBody(t.buf, t.ct, t.addr), t.ct, true)
		} else {
			// 截断的内容不再是合法 JSON，只保存开头部分，并记下完整大小和校验值。
			body = newBody(t.buf[:min(len(t.buf), previewBytes)], t.ct, false)
			body.Truncated = true
			body.Size = t.size
			if t.eof {
				body.SHA256 = hex.EncodeToString(t.sum.Sum(nil))
			}
		}
		t.rec.mu.Lock()
		t.it.Response.Body = body
		t.it.DurationMS = time.Since(t.start).Milliseconds()
		t.rec.mu.Unlock()
	})
}
