// SPDX-License-Identifier: GPL-3.0-or-later

package netdev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/redfish"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

// secretWriter 由记录器实现：写入的是密码，诊断记录里隐去。
type secretWriter interface {
	WriteSecret(p []byte) (int, error)
}

// Timing 是读设备输出时的各种等待时间，零值用默认值。测试时调小。
type Timing struct {
	// Quiet 是提示符或提问出现后要静默多久才算数：设备可能还在输出，只是这一块刚好停在一行开头。
	Quiet time.Duration
	// BigQuiet 是诊断信息这类长命令结束时的静默时间。输出里偶尔会有和提示符一样的行，等久一点更稳。
	BigQuiet time.Duration
	// Login 是登录后等第一个提示符的时间。
	Login time.Duration
	// Command 是普通命令多久没有任何输出算卡住。
	Command time.Duration
	// Big 是诊断信息这类长命令多久没有任何输出算卡住。设备生成某些部分时会停很久。
	Big time.Duration
	// Progress 是长命令报告接收进度的间隔。
	Progress time.Duration
}

func (t Timing) withDefaults() Timing {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&t.Quiet, 500*time.Millisecond)
	def(&t.BigQuiet, 2*time.Second)
	def(&t.Login, 30*time.Second)
	def(&t.Command, 2*time.Minute)
	def(&t.Big, 10*time.Minute)
	def(&t.Progress, 2*time.Second)
	return t
}

var (
	// Cisco 风格的提示符：主机名后跟 > 或 #，可带 (config) 这类模式、ASA 的 /pri/act。
	ciscoPrompt = regexp.MustCompile(`^[A-Za-z0-9][\w.\-/:@()]*[>#]$`)
	// 华为、新华三的提示符：用户视图 <主机名>，系统视图 [主机名]（VRP8 带 ~ 或 *）。
	vrpPrompt = regexp.MustCompile(`^[<\[][~*]?[^\s<>\[\]]+[>\]]$`)

	// 分页提示：Cisco --More--，华为、新华三 ---- More ----，ASA <--- More --->。
	morePrompt = regexp.MustCompile(`(?i)^\s*<?-+\s*\(?more(\s+\d+%)?\)?\s*-+>?$`)
	// 末尾是 [Y/N]、(y/n)、[yes/no]、[confirm] 的提问。
	question = regexp.MustCompile(`(?i)(\[\s*y(es)?\s*/\s*n(o)?\s*\]|\(\s*y(es)?\s*/\s*n(o)?\s*\)|\[confirm\])\s*[:?]?$`)

	userPrompt     = regexp.MustCompile(`(?i)(user\s*name|login)\s*:$`)
	passwordPrompt = regexp.MustCompile(`(?i)password\s*:$`)
	// 设备强制要求改密码（首次登录、密码过期）。工具不改密码，只能停下来请用户处理。
	forcedChange = regexp.MustCompile(`(?i)(old|new|current|retype|confirm)\s+password\s*:$|enter (the )?(old|new) password`)
)

// answer 是对设备提问的固定回答。
type answer struct {
	re    *regexp.Regexp
	reply string
}

// loginAnswers 是登录后、出现提示符之前可能遇到的提问。一律不改密码。
var loginAnswers = []answer{
	// 华为：“The password needs to be changed. Change now? [Y/N]:”（初始密码、密码快过期）
	{regexp.MustCompile(`(?i)change\s+(it\s+)?now\?\s*\[y/n\]\s*:?$`), "N\n"},
	{regexp.MustCompile(`(?i)(change|modify|update)\b.*password.*\[y/n\]\s*:?$`), "N\n"},
	{regexp.MustCompile(`(?i)press\s+(any\s+key|return|enter)\b.*$`), "\n"},
}

// authError 是进入命令行之后的认证问题（再次要求密码、enable 密码不对、要求修改密码），
// 和 SSH 登录失败一样按认证失败处理：不重试，账号密码改过之前不再登录。
type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

func (e *authError) Is(target error) bool { return target == sshx.ErrAuth }

// idleError 表示设备在规定时间内没有任何输出。
type idleError struct {
	wait    time.Duration
	partial string
}

func (e *idleError) Error() string {
	s := fmt.Sprintf("设备 %s 没有任何输出", fmtDuration(e.wait))
	if e.partial != "" {
		s += "，最后显示的是“" + clip(e.partial, 120) + "”"
	}
	return s
}

func (e *idleError) Unwrap() error { return redfish.ErrTimeout }

// term 是和设备命令行的一次交互：发送命令、读输出、认提示符、自动翻页、回答提问。
type term struct {
	sh   io.ReadWriteCloser
	tm   Timing
	ch   chan []byte
	quit chan struct{}
	once sync.Once
	rerr error // 读协程结束的原因，ch 关闭后才可读

	cl     cleaner
	prompt string
	lineFn func(line []byte) error // 当前命令的完整输出行去向
	recv   int64                   // 当前命令收到的原始字节数
	lines  int64                   // 到目前为止的完整行数
	moreAt int64                   // 上次回答分页提示时的行数
}

func newTerm(sh io.ReadWriteCloser, tm Timing) *term {
	t := &term{sh: sh, tm: tm.withDefaults(), ch: make(chan []byte, 64), quit: make(chan struct{}), moreAt: -1}
	go func() {
		defer close(t.ch)
		for {
			buf := make([]byte, 32<<10)
			n, err := sh.Read(buf)
			if n > 0 {
				select {
				case t.ch <- buf[:n]:
				case <-t.quit:
					return
				}
			}
			if err != nil {
				t.rerr = err
				return
			}
		}
	}()
	return t
}

// close 停止读协程。连接由调用方关闭。
func (t *term) close() {
	t.once.Do(func() { close(t.quit) })
}

func (t *term) send(s string) error {
	if _, err := io.WriteString(t.sh, s); err != nil {
		return fmt.Errorf("向设备发送命令：%w", err)
	}
	return nil
}

// sendSecret 发送密码，诊断记录里隐去。
func (t *term) sendSecret(s string) error {
	var err error
	if sw, ok := t.sh.(secretWriter); ok {
		_, err = sw.WriteSecret([]byte(s))
	} else {
		_, err = io.WriteString(t.sh, s)
	}
	if err != nil {
		return fmt.Errorf("向设备发送密码：%w", err)
	}
	return nil
}

// verdict 是对末行（还没换行的部分）的判断。
type verdict struct {
	send   string // 发给设备的内容：翻页的空格、提问的回答
	secret bool   // send 是密码
	wait   bool   // 像提示符或提问，再等 quiet 确认设备没有继续输出
	done   bool   // 等到了
	err    error
}

// read 读设备输出直到 judge 判定结束或出错。完整的行交给 t.lineFn；每收到数据，
// 用末行调用 judge(partial, false)；judge 要求等待时，若 quiet 时间内没有新数据，再调用 judge(partial, true)。
// idle 时间内没有任何数据返回 *idleError。
func (t *term) read(ctx context.Context, idle, quiet time.Duration, judge func(partial string, quiet bool) verdict) error {
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	var quietT *time.Timer
	var quietC <-chan time.Time
	stopQuiet := func() {
		if quietT != nil {
			quietT.Stop()
			quietT, quietC = nil, nil
		}
	}
	defer stopQuiet()
	decide := func(quietNow bool) (bool, error) {
		v := judge(t.cl.partial(), quietNow)
		switch {
		case v.err != nil:
			return false, v.err
		case v.send != "":
			stopQuiet()
			if v.secret {
				return false, t.sendSecret(v.send)
			}
			return false, t.send(v.send)
		case v.done:
			return true, nil
		case v.wait && !quietNow:
			quietT = time.NewTimer(quiet)
			quietC = quietT.C
		}
		return false, nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data, ok := <-t.ch:
			if !ok {
				return t.closed()
			}
			t.recv += int64(len(data))
			if err := t.cl.write(data, t.emit); err != nil {
				return err
			}
			if !idleT.Stop() {
				select {
				case <-idleT.C:
				default:
				}
			}
			idleT.Reset(idle)
			stopQuiet()
			if done, err := decide(false); done || err != nil {
				return err
			}
		case <-quietC:
			quietT, quietC = nil, nil
			if done, err := decide(true); done || err != nil {
				return err
			}
		case <-idleT.C:
			return &idleError{wait: idle, partial: t.cl.partial()}
		}
	}
}

func (t *term) emit(line []byte) error {
	t.lines++
	if t.lineFn != nil {
		return t.lineFn(line)
	}
	return nil
}

// closed 是设备断开连接时的错误。
func (t *term) closed() error {
	err := t.rerr
	if err == nil || errors.Is(err, io.EOF) {
		return fmt.Errorf("设备断开了连接（最后显示的是“%s”）：%w", clip(t.cl.partial(), 120), io.ErrUnexpectedEOF)
	}
	return fmt.Errorf("读取设备输出：%w", err)
}

// more 判断末行是不是分页提示，是的话回答一个空格。同一个提示只回答一次：设备收到空格、
// 擦掉提示并输出下一页之前，可能还会送来不改变末行的数据，再发空格就会跳过一页。
func (t *term) more(p string) (verdict, bool) {
	if !morePrompt.MatchString(p) {
		return verdict{}, false
	}
	if t.lines == t.moreAt {
		return verdict{}, true
	}
	t.moreAt = t.lines
	return verdict{send: " "}, true
}

// isPrompt 判断末行是否像命令行提示符。
func isPrompt(s string) bool {
	return ciscoPrompt.MatchString(s) || vrpPrompt.MatchString(s)
}

// login 等登录后的第一个提示符，再按一次回车确认，记下提示符。
//
// 沿途回答“是否现在修改密码”这类提问（一律不改）。少数设备 SSH 层不认证、进入 shell 后才问账号密码，
// 这时账号和密码各回答一次；再问一次就是认证失败，立即停止，免得账号被锁定。
// 设备强制要求修改密码时报错，不做任何修改。
func (t *term) login(ctx context.Context, user, password string) error {
	var sentUser, sentPass, nudged bool
	judge := func(p string, quiet bool) verdict {
		if v, ok := t.more(p); ok {
			return v // 登录提示信息太长也会分页
		}
		switch {
		case forcedChange.MatchString(p):
			return verdict{err: &authError{"设备要求先修改这个账号的密码（“" + clip(p, 80) + "”）。工具不会修改密码，请手动登录设备改好密码，再更新这里保存的密码后重试"}}
		case userPrompt.MatchString(p):
			if !quiet {
				return verdict{wait: true}
			}
			if sentUser {
				return verdict{err: &authError{"设备再次要求输入账号，账号或密码错误。为避免账号被锁定，工具不会再试"}}
			}
			sentUser = true
			return verdict{send: user + "\n"}
		case passwordPrompt.MatchString(p):
			if !quiet {
				return verdict{wait: true}
			}
			if sentPass {
				return verdict{err: &authError{"设备再次要求输入密码，账号或密码错误。为避免账号被锁定，工具不会再试"}}
			}
			sentPass = true
			return verdict{send: password + "\n", secret: true}
		case isPrompt(p):
			return verdict{wait: true, done: quiet}
		}
		for _, a := range loginAnswers {
			if a.re.MatchString(p) {
				if !quiet {
					return verdict{wait: true}
				}
				return verdict{send: a.reply}
			}
		}
		if question.MatchString(p) && quiet {
			return verdict{err: fmt.Errorf("登录后设备提出了无法自动回答的问题：“%s”。请导出诊断记录发给开发者：%w", clip(p, 120), redfish.ErrUnsupported)}
		}
		return verdict{wait: question.MatchString(p)}
	}
	for {
		err := t.read(ctx, t.tm.Login, t.tm.Quiet, judge)
		var idle *idleError
		if errors.As(err, &idle) && !nudged {
			// 有的设备要按一下回车才显示提示符。
			nudged = true
			if err := t.send("\n"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if errors.As(err, &idle) {
				return fmt.Errorf("登录后没有等到命令行提示符（%w）", err)
			}
			return err
		}
		break
	}
	// 再按一次回车：登录提示信息里偶尔有像提示符的行，以设备重新显示的为准。
	if err := t.send("\n"); err != nil {
		return err
	}
	err := t.read(ctx, t.tm.Login, t.tm.Quiet, func(p string, quiet bool) verdict {
		if isPrompt(p) {
			return verdict{wait: true, done: quiet}
		}
		return verdict{}
	})
	if err != nil {
		return fmt.Errorf("确认命令行提示符：%w", err)
	}
	t.prompt = t.cl.partial()
	return nil
}

// output 是一条命令的执行情况。
type output struct {
	echo  string   // 第一行：提示符加命令（设备的回显）
	head  []string // 回显之后的前几行，用来判断设备是否拒绝了命令
	lines int      // 回显之后的行数
	bytes int64    // 收到的原始字节数
	asked []string // 回答过的提问
}

// runOpts 是执行一条命令的选项。
type runOpts struct {
	idle    time.Duration // 多久没有输出算卡住，0 表示 Timing.Command
	quiet   time.Duration // 提示符出现后的静默时间，0 表示 Timing.Quiet
	answers []answer      // 这条命令可能出现的提问
	// sink 接收完整的输出行（第一行是“提示符+命令”）。返回错误时停止读取。
	sink func(line []byte) error
	// progress 每隔 Timing.Progress 收到一次已接收的字节数，可为 nil。
	progress func(bytes int64)
}

// run 执行一条命令，等提示符再次出现。分页提示自动翻页，opts.answers 里的提问自动回答，
// 其他提问报错（设备等着回答时不会自己结束）。
func (t *term) run(ctx context.Context, cmd string, o runOpts) (output, error) {
	var out output
	first := true
	t.lineFn = func(line []byte) error {
		if first {
			first = false
			out.echo = string(line)
		} else {
			out.lines++
			if len(out.head) < 6 {
				out.head = append(out.head, string(line))
			}
		}
		if o.sink != nil {
			return o.sink(line)
		}
		return nil
	}
	defer func() { t.lineFn = nil }()
	t.recv = 0
	if o.idle <= 0 {
		o.idle = t.tm.Command
	}
	if o.quiet <= 0 {
		o.quiet = t.tm.Quiet
	}
	// 进度在 judge 里按时间间隔报告：judge 和读数据在同一个协程，读 t.recv 不用加锁。
	lastReport := time.Now()
	if err := t.send(cmd + "\n"); err != nil {
		return out, err
	}
	err := t.read(ctx, o.idle, o.quiet, func(p string, quiet bool) verdict {
		if o.progress != nil && time.Since(lastReport) >= t.tm.Progress {
			lastReport = time.Now()
			o.progress(t.recv)
		}
		if v, ok := t.more(p); ok {
			return v
		}
		if p == t.prompt {
			return verdict{wait: true, done: quiet}
		}
		for _, a := range o.answers {
			if a.re.MatchString(p) {
				if !quiet {
					return verdict{wait: true}
				}
				out.asked = append(out.asked, p)
				return verdict{send: a.reply}
			}
		}
		if question.MatchString(p) {
			if !quiet {
				return verdict{wait: true}
			}
			return verdict{err: fmt.Errorf("执行 %s 时设备提出了无法自动回答的问题：“%s”。请导出诊断记录发给开发者：%w", cmd, clip(p, 120), redfish.ErrUnsupported)}
		}
		return verdict{}
	})
	out.bytes = t.recv
	if o.progress != nil {
		o.progress(t.recv)
	}
	if err != nil {
		var idle *idleError
		if errors.As(err, &idle) {
			return out, fmt.Errorf("执行 %s：%w", cmd, err)
		}
		return out, err
	}
	return out, nil
}

// rejectRe 匹配设备拒绝命令时的提示：命令不存在、参数不对、权限不够。
var rejectRe = regexp.MustCompile(`(?i)^\s*(%|error:|\^?\s*error\b)|unrecognized command|invalid (input|command)|incomplete command|ambiguous command|permission denied|not authorized|authorization failed|do not have permission`)

// rejected 判断命令是否被设备拒绝：回显后的头几行里有报错，而且输出很短。
// 正常的长输出（例如日志里以 % 开头的行）不算。
func (o output) rejected() string {
	if o.lines > 8 {
		return ""
	}
	for _, l := range o.head {
		if strings.TrimSpace(strings.Trim(l, "^ ")) == "" {
			continue // 指向出错位置的 ^ 那一行
		}
		if rejectRe.MatchString(l) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// privileged 判断 Cisco 风格的提示符是否在特权模式。
func (t *term) privileged() bool {
	return strings.HasSuffix(t.prompt, "#")
}

// enable 进入 Cisco 的特权模式。密码只发一次：被拒绝时立即停止，不再尝试。
// password 为空时，设备要密码就报错，不拿登录密码去试。
func (t *term) enable(ctx context.Context, password string) error {
	var said []string
	t.lineFn = func(line []byte) error {
		if s := strings.TrimSpace(string(line)); s != "" && len(said) < 10 {
			said = append(said, s)
		}
		return nil
	}
	defer func() { t.lineFn = nil }()
	sent := false
	if err := t.send("enable\n"); err != nil {
		return err
	}
	err := t.read(ctx, t.tm.Command, t.tm.Quiet, func(p string, quiet bool) verdict {
		switch {
		case passwordPrompt.MatchString(p):
			if !quiet {
				return verdict{wait: true}
			}
			if sent {
				return verdict{err: errEnable}
			}
			if password == "" {
				return verdict{err: fmt.Errorf("进入特权模式需要 enable 密码，请在设备里填写：%w", redfish.ErrPermission)}
			}
			sent = true
			return verdict{send: password + "\n", secret: true}
		case isPrompt(p):
			return verdict{wait: true, done: quiet}
		}
		return verdict{}
	})
	if err != nil {
		return err
	}
	t.prompt = t.cl.partial()
	if t.privileged() {
		return nil
	}
	if sent {
		return errEnable
	}
	msg := "设备没有进入特权模式"
	if len(said) > 1 {
		msg += "（" + clip(strings.Join(said[1:], " "), 120) + "）"
	}
	return fmt.Errorf("%s。请给账号配置 privilege 15，或在设备上配置 enable 密码后填写到这里：%w", msg, redfish.ErrPermission)
}

// errEnable 是 enable 密码被拒绝。
var errEnable = &authError{"enable 密码不对，设备拒绝进入特权模式。为避免账号被锁定，工具不会再试"}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.String()
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Round(time.Second)/time.Second))
	case d%time.Minute == 0 && d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	case d < time.Hour:
		return fmt.Sprintf("%d 分 %d 秒", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d/time.Hour), int(d%time.Hour/time.Minute))
}
