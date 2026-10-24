// SPDX-License-Identifier: GPL-3.0-or-later

package netdev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

// RecordFormat 是网络设备命令行诊断记录的格式标识。
const RecordFormat = "logdiag.netrec/1"

// 每一步的输出完整保存的上限，超过时只保存开头和结尾（结尾里有提示符，回放时要用）。
const (
	headBytes  = 64 << 10
	tailBytes  = 4 << 10
	totalBytes = 8 << 20 // 整个记录完整保存的输出上限，超过后每步只保存结尾
	thinTail   = 256     // 超过总上限后每步保存的结尾，够放下分页提示或提示符
)

// Record 是一次网络设备命令行会话的诊断记录：工具发给设备的内容和设备的原始输出，按先后排列。
//
// 不含登录密码和 enable 密码（只记“发送了密码”），连接地址换成 {{host}}，设备输出按保存日志时的规则
// 遮盖口令、密钥和团体名。输出里的主机名、序列号、配置等设备信息仍会保留，发送前需要用户确认。
type Record struct {
	Format string `json:"format"`
	// Note 说明记录来源，例如“真机录制：Cisco C9300-48P IOS-XE 17.03.04”或“合成：依据……”。
	Note          string  `json:"note,omitempty"`
	Tool          string  `json:"tool,omitempty"`
	RecordedAt    string  `json:"recorded_at,omitempty"`
	ServerVersion string  `json:"server_version,omitempty"`
	HostKey       string  `json:"host_key,omitempty"` // 类型和 SHA256 指纹
	Error         string  `json:"error,omitempty"`    // 连接或收集失败的原因
	Steps         []*Step `json:"steps"`
}

// Step 是一次发送和随后收到的输出。第一步没有发送：登录后设备主动输出的提示信息和提示符。
type Step struct {
	Send   string   `json:"send,omitempty"`
	Secret bool     `json:"secret,omitempty"` // 发送的是密码，内容不记录
	AtMS   int64    `json:"at_ms"`            // 距登录成功的毫秒数
	Output Output   `json:"output,omitzero"`
	Notes  []string `json:"notes,omitempty"` // 工具在这一步写的说明，例如跳过了哪条命令
}

// Output 是设备的原始输出（含控制字符）。太长时只保存开头和结尾，记下完整大小和 SHA-256。
type Output struct {
	Text      string `json:"text,omitempty"`
	Tail      string `json:"tail,omitempty"`
	Size      int64  `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Recorder 收集一次会话的诊断记录，可以被多个 goroutine 同时使用。nil 的 Recorder 什么也不记。
type Recorder struct {
	mu      sync.Mutex
	r       Record
	start   time.Time
	cur     *Step
	cap     *capture
	host    string
	secrets []string
	budget  int
	red     redactor
}

// NewRecorder 返回空的记录器。
func NewRecorder() *Recorder {
	return &Recorder{budget: totalBytes}
}

// Record 返回到目前为止的记录副本，当前这一步的输出也整理进去。
func (r *Recorder) Record() *Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.r
	out.Format = RecordFormat
	out.Steps = make([]*Step, len(r.r.Steps))
	for i, s := range r.r.Steps {
		c := *s
		c.Notes = append([]string(nil), s.Notes...)
		if s == r.cur && r.cap != nil {
			c.Output, _ = r.cap.output(r)
		}
		out.Steps[i] = &c
	}
	return &out
}

// begin 记下要从记录里去掉的地址和密码。
func (r *Recorder) begin(host string, secrets ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.host = host
	for _, s := range secrets {
		// 太短的密码不做替换，否则会把输出里碰巧相同的字符都换掉，记录没法看。
		if len(s) >= 4 {
			r.secrets = append(r.secrets, s)
		}
	}
}

// connected 记下连接信息，开始第一步。
func (r *Recorder) connected(c *Conn) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.r.ServerVersion = c.ServerVersion
	if c.HostKey != "" {
		r.r.HostKey = strings.TrimSpace(c.HostKeyType + " " + c.HostKey)
	}
	r.start = time.Now()
	r.next(&Step{})
}

// fail 记下失败的原因。
func (r *Recorder) fail(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.r.Error = r.scrub(err.Error())
}

// Note 在当前这一步加一条说明。
func (r *Recorder) Note(text string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		r.next(&Step{})
	}
	r.cur.Notes = append(r.cur.Notes, r.scrub(text))
}

// next 结束当前一步，开始新的一步。调用方持有 r.mu。
func (r *Recorder) next(s *Step) {
	if r.cur != nil && r.cap != nil {
		var kept int
		r.cur.Output, kept = r.cap.output(r)
		r.budget -= kept
	}
	if !r.start.IsZero() {
		s.AtMS = time.Since(r.start).Milliseconds()
	}
	r.r.Steps = append(r.r.Steps, s)
	r.cur = s
	head := max(0, min(headBytes, r.budget))
	tail := tailBytes
	if r.budget <= 0 {
		tail = thinTail
	}
	r.cap = &capture{headMax: head, tailMax: tail, sum: sha256.New()}
}

func (r *Recorder) send(p string, secret bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &Step{Secret: secret}
	if !secret {
		s.Send = r.scrub(p)
	}
	r.next(s)
}

func (r *Recorder) output(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		r.next(&Step{})
	}
	r.cap.write(p)
}

// scrub 去掉密码、把连接地址换成占位符。调用方持有 r.mu。
func (r *Recorder) scrub(s string) string {
	for _, p := range r.secrets {
		s = strings.ReplaceAll(s, p, httprec.Redacted)
	}
	if r.host != "" {
		s = httprec.ReplaceHost(s, r.host, httprec.HostPlaceholder)
	}
	return s
}

// wrap 返回记录读写内容的命令行。
func (r *Recorder) wrap(sh io.ReadWriteCloser) io.ReadWriteCloser {
	if r == nil {
		return sh
	}
	return &recShell{sh: sh, r: r}
}

type recShell struct {
	sh io.ReadWriteCloser
	r  *Recorder
}

func (s *recShell) Read(p []byte) (int, error) {
	n, err := s.sh.Read(p)
	if n > 0 {
		s.r.output(p[:n])
	}
	return n, err
}

func (s *recShell) Write(p []byte) (int, error) {
	s.r.send(string(p), false)
	return s.sh.Write(p)
}

// WriteSecret 发送密码，记录里只写“发送了密码”。
func (s *recShell) WriteSecret(p []byte) (int, error) {
	s.r.send("", true)
	if sw, ok := s.sh.(secretWriter); ok {
		return sw.WriteSecret(p)
	}
	return s.sh.Write(p)
}

func (s *recShell) Close() error { return s.sh.Close() }

// capture 边收边记一步的输出：保存开头和结尾，同时计算完整大小和 SHA-256。
type capture struct {
	headMax, tailMax int
	head, tail       []byte
	size             int64
	sum              hash.Hash
}

func (c *capture) write(p []byte) {
	c.size += int64(len(p))
	c.sum.Write(p)
	if room := c.headMax - len(c.head); room > 0 {
		n := min(room, len(p))
		c.head = append(c.head, p[:n]...)
		p = p[n:]
	}
	if len(p) == 0 {
		return
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > 2*c.tailMax {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-c.tailMax:]...)
	}
}

// output 整理成记录里的形式，返回保存的字节数：脱敏、去掉密码和地址；被截断时开头和结尾都切在
// 行边界上，免得把半行配置（例如团体名所在的行）切成认不出的样子而漏掉脱敏。调用方持有 r.mu。
func (c *capture) output(r *Recorder) (Output, int) {
	if c.size == 0 {
		return Output{}, 0
	}
	tail := c.tail
	if len(tail) > c.tailMax {
		tail = tail[len(tail)-c.tailMax:]
	}
	if int64(len(c.head)+len(tail)) == c.size {
		data := string(c.head) + string(tail)
		return Output{Text: r.scrub(r.red.text(data))}, len(data)
	}
	head := c.head
	if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
		head = head[:i+1]
	} else {
		head = nil
	}
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return Output{
		Text:      r.scrub(r.red.text(string(head))),
		Tail:      r.scrub(r.red.text(string(tail))),
		Size:      c.size,
		SHA256:    hex.EncodeToString(c.sum.Sum(nil)),
		Truncated: true,
	}, len(head) + len(tail)
}

// LoadRecord 读取诊断记录文件。
func LoadRecord(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("netdev: %s 不是有效的诊断记录：%w", path, err)
	}
	if rec.Format != RecordFormat {
		return nil, fmt.Errorf("netdev: %s 的格式是 %q，需要 %q", path, rec.Format, RecordFormat)
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

// Replay 按诊断记录回放设备：依次核对工具发送的内容，送出设备当时的输出。
// 用作 Request.Dial，测试时代替真实设备。
type Replay struct {
	rec *Record

	mu     sync.Mutex
	cond   *sync.Cond
	next   int    // 下一个要核对发送内容的步骤
	in     string // 本步已收到的发送内容
	out    []byte // 还没被读走的输出
	err    error
	closed bool
}

// NewReplay 返回回放器。
func NewReplay(rec *Record) *Replay {
	p := &Replay{rec: rec}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Dial 实现 Dialer：返回回放的命令行，连接信息来自记录。和真实连接一样核对 cfg.HostKey。
func (p *Replay) Dial(ctx context.Context, cfg sshx.Config) (*Conn, error) {
	if len(p.rec.Steps) == 0 {
		return nil, fmt.Errorf("回放：录制时没有连上设备（%s）", p.rec.Error)
	}
	typ, fp, _ := strings.Cut(p.rec.HostKey, " ")
	if want := sshx.NormalizeFingerprint(cfg.HostKey); want != "" && want != fp {
		return nil, &sshx.HostKeyError{Want: want, Got: fp, Type: typ}
	}
	p.mu.Lock()
	if len(p.rec.Steps) > 0 && p.rec.Steps[0].Send == "" && !p.rec.Steps[0].Secret {
		p.out = append(p.out, p.rec.Steps[0].Output.replay()...)
		p.next = 1
	}
	p.mu.Unlock()
	return &Conn{Shell: (*replayShell)(p), ServerVersion: p.rec.ServerVersion, HostKey: fp, HostKeyType: typ}, nil
}

// Remaining 返回还没走到的步骤（发送内容），测试结束时应为空。
func (p *Replay) Remaining() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var left []string
	for _, s := range p.rec.Steps[min(p.next, len(p.rec.Steps)):] {
		if s.Secret {
			left = append(left, "（密码）")
		} else {
			left = append(left, s.Send)
		}
	}
	return left
}

// Err 返回回放中发现的不一致。
func (p *Replay) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (o Output) replay() []byte {
	if !o.Truncated {
		return []byte(o.Text)
	}
	omitted := o.Size - int64(len(o.Text)+len(o.Tail))
	return []byte(o.Text + fmt.Sprintf("\r\n……诊断记录省略了 %d 字节……\r\n", omitted) + o.Tail)
}

type replayShell Replay

func (s *replayShell) Read(b []byte) (int, error) {
	p := (*Replay)(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.out) == 0 && p.err == nil && !p.closed {
		p.cond.Wait()
	}
	switch {
	case len(p.out) > 0:
		n := copy(b, p.out)
		p.out = p.out[n:]
		return n, nil
	case p.err != nil:
		return 0, p.err
	}
	return 0, io.EOF
}

func (s *replayShell) Write(b []byte) (int, error) {
	return s.write(string(b), false)
}

func (s *replayShell) WriteSecret(b []byte) (int, error) {
	return s.write(string(b), true)
}

func (s *replayShell) write(in string, secret bool) (int, error) {
	p := (*Replay)(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.cond.Broadcast()
	switch {
	case p.closed:
		return 0, io.ErrClosedPipe
	case p.err != nil:
		return 0, p.err
	case p.next >= len(p.rec.Steps):
		p.err = fmt.Errorf("回放：记录已经结束，工具还发送了 %q", shown(in, secret))
		return 0, p.err
	}
	step := p.rec.Steps[p.next]
	if secret || step.Secret {
		if secret != step.Secret || p.in != "" {
			p.err = fmt.Errorf("回放：第 %d 步记录发送的是 %q，工具发送的是 %q", p.next, shown(step.Send, step.Secret), shown(in, secret))
			return 0, p.err
		}
	} else {
		p.in += in
		if !strings.HasPrefix(step.Send, p.in) {
			p.err = fmt.Errorf("回放：第 %d 步记录发送的是 %q，工具发送的是 %q", p.next, step.Send, p.in)
			return 0, p.err
		}
		if p.in != step.Send {
			return len(in), nil
		}
	}
	p.in = ""
	p.next++
	p.out = append(p.out, step.Output.replay()...)
	return len(in), nil
}

func shown(s string, secret bool) string {
	if secret {
		return "（密码）"
	}
	return s
}

func (s *replayShell) Close() error {
	p := (*Replay)(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
	return nil
}
