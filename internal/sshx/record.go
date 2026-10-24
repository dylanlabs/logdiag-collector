// SPDX-License-Identifier: GPL-3.0-or-later

package sshx

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dylanlabs/logdiag-collector/internal/httprec"
)

// RecordFormat 是 SSH 诊断记录的格式标识。
const RecordFormat = "logdiag.sshrec/1"

// DefaultMaxOutput 是完整保存一条命令输出的上限。
const DefaultMaxOutput = 256 << 10

// 超过上限的输出（通常是下载的结果文件本身）只保存开头这么多字节，够看出文件类型。
const previewBytes = 512

// Record 是一次 SSH 连接的诊断记录：连接信息和依次执行的命令。
//
// 和 httprec 的诊断记录一样：不含密码，连接的地址替换成 {{host}}；
// 命令的输入不记录（sudo 密码经输入传给服务器），只记是否有输入。
// 输出里的主机名、序列号等设备信息仍会保留，发送前需要用户确认。
type Record struct {
	Format string `json:"format"`
	// Note 说明记录来源，例如“真机录制：Rocky Linux 9.4”或“合成：按 linux-pm.sh 的说明编写”。
	Note          string  `json:"note,omitempty"`
	Tool          string  `json:"tool,omitempty"`
	RecordedAt    string  `json:"recorded_at,omitempty"`
	ServerVersion string  `json:"server_version,omitempty"`
	HostKey       string  `json:"host_key,omitempty"` // 类型和 SHA256 指纹
	Steps         []*Step `json:"steps"`
}

// Step 是记录里的一步。
type Step struct {
	Kind       string `json:"kind"` // connect：连接和登录；exec：一条命令；note：调用方写的说明
	Command    string `json:"command,omitempty"`
	Stdin      bool   `json:"stdin,omitempty"` // 是否向命令输入了内容（内容不记录）
	Stdout     Output `json:"stdout,omitzero"`
	Stderr     Output `json:"stderr,omitzero"`
	Exit       *int   `json:"exit,omitempty"`
	Error      string `json:"error,omitempty"`
	Text       string `json:"text,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Output 是命令输出。超过上限时只保存开头部分，并记下完整大小和 SHA-256。
type Output struct {
	Text      string `json:"text,omitempty"`
	Base64    string `json:"base64,omitempty"`
	Size      int64  `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Bytes 返回保存的输出内容（截断时只是开头部分）。
func (o Output) Bytes() ([]byte, error) {
	if o.Base64 != "" {
		return base64.StdEncoding.DecodeString(o.Base64)
	}
	return []byte(o.Text), nil
}

// Recorder 收集一次连接的诊断记录，可以被多个 goroutine 同时使用。nil 的 Recorder 什么也不记。
type Recorder struct {
	// MaxOutput 是完整保存一条命令输出的字节数上限，0 表示 DefaultMaxOutput。
	MaxOutput int

	mu       sync.Mutex
	r        Record
	host     string
	password string
}

// NewRecorder 返回空的记录器。
func NewRecorder() *Recorder {
	return &Recorder{}
}

// Record 返回到目前为止的记录副本。
func (r *Recorder) Record() *Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.r
	out.Format = RecordFormat
	out.Steps = make([]*Step, len(r.r.Steps))
	for i, s := range r.r.Steps {
		c := *s
		out.Steps[i] = &c
	}
	return &out
}

// Note 在记录里加一条说明，例如上传的脚本名、大小和校验和。
func (r *Recorder) Note(text string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.r.Steps = append(r.r.Steps, &Step{Kind: "note", Text: r.scrub(text)})
}

func (r *Recorder) begin(host, password string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.host = host
	// 太短的密码不做替换，否则会把输出里碰巧相同的字符都换掉，记录没法看。
	if len(password) >= 4 {
		r.password = password
	}
}

func (r *Recorder) connected(c *Client, d time.Duration, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &Step{Kind: "connect", DurationMS: d.Milliseconds()}
	if err != nil {
		s.Error = r.scrub(err.Error())
	} else {
		r.r.ServerVersion = c.serverVersion
		r.r.HostKey = c.hostKeyType + " " + c.hostKey
	}
	r.r.Steps = append(r.r.Steps, s)
}

// scrub 把连接的地址换成占位符、去掉密码。调用方持有 r.mu。
func (r *Recorder) scrub(s string) string {
	if r.password != "" {
		s = strings.ReplaceAll(s, r.password, httprec.Redacted)
	}
	if r.host != "" {
		s = httprec.ReplaceHost(s, r.host, httprec.HostPlaceholder)
	}
	return s
}

// stepRecorder 记录一条命令。nil 表示不记录。
type stepRecorder struct {
	r      *Recorder
	s      *Step
	start  time.Time
	so, se capture // 标准输出、标准错误
}

func (r *Recorder) exec(cmd string, stdin bool) *stepRecorder {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	max := r.MaxOutput
	if max <= 0 {
		max = DefaultMaxOutput
	}
	s := &Step{Kind: "exec", Command: r.scrub(cmd), Stdin: stdin}
	r.r.Steps = append(r.r.Steps, s)
	return &stepRecorder{r: r, s: s, start: time.Now(), so: newCapture(max), se: newCapture(max)}
}

func (sr *stepRecorder) stdout(w io.Writer) io.Writer {
	if sr == nil {
		return w
	}
	return sr.so.tee(w)
}

func (sr *stepRecorder) stderr(w io.Writer) io.Writer {
	if sr == nil {
		return w
	}
	return sr.se.tee(w)
}

func (sr *stepRecorder) finish(exit int, err error) {
	if sr == nil {
		return
	}
	r := sr.r
	r.mu.Lock()
	defer r.mu.Unlock()
	sr.s.DurationMS = time.Since(sr.start).Milliseconds()
	sr.s.Stdout = sr.so.output(r)
	sr.s.Stderr = sr.se.output(r)
	if exit >= 0 {
		sr.s.Exit = &exit
	}
	if err != nil {
		sr.s.Error = r.scrub(err.Error())
	}
}

// capture 边写边记一条输出：保存上限以内的内容，同时计算完整大小和 SHA-256，不把大文件读进内存。
type capture struct {
	max  int
	buf  bytes.Buffer
	size int64
	sum  hash.Hash
}

func newCapture(max int) capture {
	return capture{max: max, sum: sha256.New()}
}

func (c *capture) tee(w io.Writer) io.Writer {
	if w == nil {
		return c
	}
	return io.MultiWriter(w, c)
}

func (c *capture) Write(p []byte) (int, error) {
	c.size += int64(len(p))
	c.sum.Write(p)
	if room := c.max + 1 - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// output 选择保存形式：文本存字符串（去掉密码、替换地址），二进制存 base64；超过上限只存开头。
func (c *capture) output(r *Recorder) Output {
	if c.size == 0 {
		return Output{}
	}
	data := c.buf.Bytes()
	var o Output
	if c.size > int64(c.max) {
		data = data[:min(len(data), previewBytes)]
		o = Output{Size: c.size, SHA256: hex.EncodeToString(c.sum.Sum(nil)), Truncated: true}
	}
	if isText(data) {
		o.Text = r.scrub(string(data))
	} else {
		o.Base64 = base64.StdEncoding.EncodeToString(data)
	}
	return o
}

func isText(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return false
	}
	if utf8.Valid(data) {
		return true
	}
	// 截在多字节字符中间的开头部分也算文本。
	for i := 1; i < utf8.UTFMax && i < len(data); i++ {
		if utf8.Valid(data[:len(data)-i]) {
			return true
		}
	}
	return false
}

// Load 读取诊断记录文件。
func Load(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("sshx: %s 不是有效的诊断记录：%w", path, err)
	}
	if rec.Format != RecordFormat {
		return nil, fmt.Errorf("sshx: %s 的格式是 %q，需要 %q", path, rec.Format, RecordFormat)
	}
	return &rec, nil
}

// Save 把诊断记录写入文件。
func (rec *Record) Save(path string) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
