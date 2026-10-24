// SPDX-License-Identifier: GPL-3.0-or-later

// Package netdev 通过 SSH 命令行收集网络设备的诊断信息：Cisco（IOS、IOS XE、NX-OS、MDS、ASA）、
// 华为（VRP）、新华三（Comware）。
//
// 流程：SSH 登录 → 等提示符（登录后的提问一律不改密码）→ Cisco 账号不在特权模式时用 enable 密码进入 →
// 关闭当前会话的分页 → show/display version 识别系统和型号，show inventory 等读序列号 →
// 执行诊断信息命令（show tech-support、display diagnostic-information），边收边写文件 →
// 附带保存日志缓冲区 → 关闭会话（即退出登录）。
//
// 输出直接从终端收下来：不在设备上生成文件、不用 SFTP、不改配置。保存前按 rules 遮盖口令、密钥和
// 团体名，manifest 的 redaction 写明规则。诊断信息命令会让设备 CPU 短时升高，业务高峰期慎用。
package netdev

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

// DeviceType 是 manifest 和设备库里网络设备的类型。
const DeviceType = "network"

// 终端大小：宽一些，免得设备按 80 列折行；行数大一些，少分页。
const (
	termWidth  = 511
	termHeight = 1000
)

// maxParse 是识别型号时读进内存的输出上限。
const maxParse = 1 << 20

// Request 是一次收集或识别的参数。
type Request struct {
	Address  string // 管理口地址，可以带端口（10.0.0.1:2222）
	User     string
	Password string
	Enable   string // Cisco 的 enable 密码，可空：账号登录后已在特权模式时用不到
	// HostKey 是期望的 SSH 主机密钥指纹。为空时接受设备给出的密钥（首次连接），
	// 结果里返回指纹，调用方保存下来，下次传入核对。
	HostKey string

	Store    *collect.Store // Identify 不用
	Progress func(collect.Event)
	Recorder *Recorder // 诊断记录，可为 nil

	// 以下供测试替换。
	Dial   Dialer // nil 时用 DialSSH
	Timing Timing
}

// Conn 是登录 SSH 后打开的交互式命令行。
type Conn struct {
	Shell         io.ReadWriteCloser
	ServerVersion string // 例如 SSH-2.0-Cisco-1.25，用来初步判断厂商
	HostKey       string // SHA256 指纹
	HostKeyType   string
}

// Dialer 连接设备、登录 SSH 并打开交互式命令行。
type Dialer func(ctx context.Context, cfg sshx.Config) (*Conn, error)

// DialSSH 是默认的 Dialer：用 sshx 登录（密码只发一次），申请终端、打开 shell，并定时保活。
func DialSSH(ctx context.Context, cfg sshx.Config) (*Conn, error) {
	c, err := sshx.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	sh, err := openShell(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &Conn{Shell: sh, ServerVersion: c.ServerVersion(), HostKey: c.HostKey(), HostKeyType: c.HostKeyType()}, nil
}

func openShell(c *sshx.Client) (*sshShell, error) {
	s, err := c.Raw().NewSession()
	if err != nil {
		return nil, fmt.Errorf("打开 SSH 会话：%w", err)
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := s.RequestPty("vt100", termHeight, termWidth, modes); err != nil {
		s.Close()
		return nil, fmt.Errorf("设备不接受终端请求：%w", err)
	}
	in, err := s.StdinPipe()
	if err != nil {
		s.Close()
		return nil, err
	}
	out, err := s.StdoutPipe()
	if err != nil {
		s.Close()
		return nil, err
	}
	if err := s.Shell(); err != nil {
		s.Close()
		return nil, fmt.Errorf("打开命令行：%w", err)
	}
	// 诊断信息命令期间设备可能几分钟没有输出，定时发保活请求，免得防火墙断开空闲连接。
	ctx, stop := context.WithCancel(context.Background())
	c.KeepAlive(ctx, 0)
	return &sshShell{s: s, in: in, out: out, c: c, stop: stop}, nil
}

type sshShell struct {
	s    *ssh.Session
	in   io.WriteCloser
	out  io.Reader
	c    *sshx.Client
	stop context.CancelFunc
	once sync.Once
}

func (s *sshShell) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *sshShell) Write(p []byte) (int, error) { return s.in.Write(p) }

// Close 关闭会话和连接，设备随之结束这次登录。
func (s *sshShell) Close() error {
	s.once.Do(func() {
		s.stop()
		s.s.Close()
		s.c.Close()
	})
	return nil
}

// Identity 是登录后识别出的设备信息。
type Identity struct {
	Vendor   string // Cisco、Huawei、H3C（Comware 的 HPE 交换机为 HPE）
	OS       string // IOS、IOS-XE、NX-OS、ASA、VRP、Comware
	Version  string
	Model    string
	Serial   string
	Hostname string
	LogType  string // 收集时的日志类型
	HostKey  string // SSH 主机密钥指纹
}

// Firmware 返回设备表“固件”一栏的写法，例如“NX-OS 9.3(8)”。
func (id *Identity) Firmware() string {
	return strings.TrimSpace(id.OS + " " + id.Version)
}

// Identify 登录设备，识别厂商、系统、型号、序列号和主机名，然后断开。只执行查看类命令。
func Identify(ctx context.Context, req Request) (*Identity, error) {
	s, err := open(ctx, req)
	if err != nil {
		return nil, withHint(err)
	}
	defer s.close()
	return s.identity(), nil
}

// Collect 登录设备、识别型号，执行诊断信息命令并保存到 req.Store。连接在结束、失败或取消时都会关闭。
// 取消时设备上正在输出的命令随会话结束，不影响设备运行。
func Collect(ctx context.Context, req Request) (*collect.Result, error) {
	res, err := collectLog(ctx, req)
	return res, withHint(err)
}

func collectLog(ctx context.Context, req Request) (*collect.Result, error) {
	s, err := open(ctx, req)
	if err != nil {
		return nil, err
	}
	defer s.close()
	id := s.identity()
	p := s.facts.prof
	s.report(collect.StageProbe, fmt.Sprintf("识别为 %s %s（序列号 %s，%s）", id.Vendor, id.Model, id.Serial, id.Firmware()))
	if p.enable && !s.t.privileged() {
		err := fmt.Errorf("%s 要在特权模式下执行，但账号登录后在普通模式（提示符“%s”）。请在设备里填写 enable 密码，或给账号配置 privilege 15：%w",
			p.diag, s.t.prompt, redfish.ErrPermission)
		req.Recorder.fail(err)
		return nil, err
	}
	// 先收日志缓冲区（很快），再执行耗时的诊断信息命令。设备不支持的跳过。
	var extras []collect.Attachment
	for _, cmd := range p.extras {
		data, err := s.extra(ctx, cmd)
		if err != nil {
			req.Recorder.fail(err)
			return nil, err
		}
		if len(data) > 0 {
			extras = append(extras, collect.Attachment{Name: fileName(cmd), Data: data})
		}
	}
	job := &collect.Job{
		Device: collect.Device{
			Type: DeviceType, Vendor: id.Vendor, Model: id.Model, Serial: id.Serial, Hostname: id.Hostname,
			Address:    s.host,
			Controller: collect.Controller{Type: id.OS, Firmware: id.Version},
		},
		Store:     req.Store,
		Progress:  req.Progress,
		Redaction: collect.Redaction{Applied: true, Rules: RedactionRules},
	}
	res, err := job.Save(collect.Log{Type: p.logType, Method: "ssh"}, fileName(p.diag), func(w io.Writer) error { return s.diag(ctx, w) }, extras...)
	if err != nil {
		req.Recorder.fail(err)
		return nil, err
	}
	res.SSHHostKey = s.conn.HostKey
	return res, nil
}

// 网络设备的处理建议。分类的通用建议是按服务器 BMC 写的（HTTPS 端口、BMC 固件），这里换成 SSH 的说法。
const (
	hintNetwork     = "检查管理口 IP、网线和防火墙，确认本机能连上设备的 SSH 端口（默认 22；端口不同时地址写成 10.0.0.1:2222）。"
	hintUnsupported = "目前支持 Cisco IOS、IOS XE、NX-OS（含 MDS）、ASA 和华为 VRP、新华三 Comware 的命令行。可导出诊断记录发给开发者。"
	hintHostKey     = "设备的 SSH 主机密钥和上次记录的不一样：可能换过设备或重新生成过密钥，也可能有人冒充这台设备。确认无误后在设备里清除记录的主机密钥再试。"
	hintTimeout     = "设备长时间没有输出或整理诊断信息较慢（大型 Nexus 可能要几十分钟），可在设置里调大单台超时后重试；反复超时请导出诊断记录发给开发者。"
)

// withHint 给错误换上网络设备的处理建议，已有专门建议的不变。
func withHint(err error) error {
	if err == nil || redfish.HintOf(err) != "" {
		return err
	}
	switch redfish.KindOf(err) {
	case redfish.KindNetwork:
		return redfish.WithHint(err, redfish.KindUnknown, hintNetwork)
	case redfish.KindUnsupported:
		return redfish.WithHint(err, redfish.KindUnknown, hintUnsupported)
	case redfish.KindHostKey:
		return redfish.WithHint(err, redfish.KindUnknown, hintHostKey)
	case redfish.KindTimeout:
		return redfish.WithHint(err, redfish.KindUnknown, hintTimeout)
	}
	return err
}

// session 是登录并识别后的一次会话。
type session struct {
	req    Request
	conn   *Conn
	sh     io.ReadWriteCloser
	t      *term
	host   string // 不带端口的地址，写进 manifest
	fam    family
	facts  facts
	vendor string
}

// open 连接、登录、关闭分页并识别设备。失败时连接已关闭。
func open(ctx context.Context, req Request) (_ *session, err error) {
	addr, host, err := sshx.HostPort(req.Address)
	if err != nil {
		return nil, err
	}
	s := &session{req: req, host: host}
	req.Recorder.begin(host, req.Password, req.Enable)
	defer func() {
		if err != nil {
			req.Recorder.fail(err)
			s.close()
		}
	}()

	s.report(collect.StageProbe, "SSH 连接 "+addr)
	dial := req.Dial
	if dial == nil {
		dial = DialSSH
	}
	s.conn, err = dial(ctx, sshx.Config{Address: req.Address, User: req.User, Password: req.Password, HostKey: req.HostKey})
	if err != nil {
		err = fmt.Errorf("SSH 登录：%w", err)
		if k := redfish.KindOf(err); k == redfish.KindNetwork || k == redfish.KindTimeout {
			err = redfish.WithHint(err, redfish.KindNetwork, hintNetwork)
		}
		return nil, err
	}
	req.Recorder.connected(s.conn)
	s.sh = req.Recorder.wrap(s.conn.Shell)
	s.t = newTerm(s.sh, req.Timing)

	s.report(collect.StageProbe, "等待命令行提示符")
	if err := s.t.login(ctx, req.User, req.Password); err != nil {
		return nil, fmt.Errorf("登录命令行：%w", err)
	}
	s.fam = familyOf(s.t.prompt)
	if s.fam == famUnknown {
		return nil, fmt.Errorf("提示符“%s”不像 Cisco、华为或新华三的网络设备：%w", clip(s.t.prompt, 60), redfish.ErrUnsupported)
	}
	if s.fam == famCisco && !s.t.privileged() && req.Enable != "" {
		s.report(collect.StageProbe, "进入特权模式")
		if err := s.t.enable(ctx, req.Enable); err != nil {
			return nil, err
		}
	}
	if err := s.paging(ctx); err != nil {
		return nil, err
	}

	s.report(collect.StageProbe, "识别型号")
	cmd := s.fam.versionCommand()
	text, err := s.capture(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("执行 %s：%w", cmd, err)
	}
	s.facts = parseVersion(s.fam, text)
	if s.facts.prof == nil {
		return nil, fmt.Errorf("%s 的输出里没认出设备的系统（开头是“%s”）。目前支持 Cisco IOS、IOS XE、NX-OS、ASA，华为 VRP，新华三 Comware：%w",
			cmd, clip(firstLine(text), 80), redfish.ErrUnsupported)
	}
	s.vendor = vendorOf(s.facts.prof, text)
	if inv := s.facts.prof.inventory; inv != "" {
		text, err := s.capture(ctx, inv)
		var rej *rejectedError
		switch {
		case errors.As(err, &rej):
			s.note("读不到序列号：" + err.Error())
		case err != nil:
			return nil, fmt.Errorf("执行 %s：%w", inv, err)
		default:
			model, serial := parseInventory(s.facts.prof, text)
			s.facts.model = firstNonEmpty(model, s.facts.model)
			s.facts.serial = firstNonEmpty(serial, s.facts.serial)
		}
	}
	if s.facts.hostname == "" {
		s.facts.hostname = hostnameOf(s.t.prompt)
	}
	return s, nil
}

func (s *session) close() {
	if s.t != nil {
		s.t.close()
	}
	if s.sh != nil {
		s.sh.Close()
	}
}

func (s *session) report(stage collect.Stage, msg string) {
	if s.req.Progress != nil {
		s.req.Progress(collect.Event{Stage: stage, Percent: -1, Message: msg})
	}
}

func (s *session) note(text string) {
	s.req.Recorder.Note(text)
}

func (s *session) identity() *Identity {
	return &Identity{
		Vendor: s.vendor, OS: s.facts.prof.os, Version: s.facts.version, Model: s.facts.model, Serial: s.facts.serial,
		Hostname: s.facts.hostname, LogType: s.facts.prof.logType, HostKey: s.conn.HostKey,
	}
}

// paging 关闭当前会话的分页（不改配置）。设备都不接受时靠自动翻页，只是慢一些。
func (s *session) paging(ctx context.Context) error {
	for _, cmd := range s.fam.pagingCommands(s.conn.ServerVersion) {
		out, err := s.t.run(ctx, cmd, runOpts{})
		if err != nil {
			return fmt.Errorf("执行 %s：%w", cmd, err)
		}
		if out.rejected() == "" {
			return nil
		}
	}
	s.note("设备不接受关闭分页的命令，改为自动翻页")
	return nil
}

// rejectedError 表示设备拒绝了命令：命令不存在、参数不对或权限不够。
type rejectedError struct {
	cmd, msg string
}

func (e *rejectedError) Error() string {
	return fmt.Sprintf("设备拒绝执行 %s（%s）", e.cmd, e.msg)
}

// capture 执行一条查看命令，返回回显之后的输出（最多 maxParse 字节），用来识别型号。
func (s *session) capture(ctx context.Context, cmd string) (string, error) {
	var b strings.Builder
	out, err := s.t.run(ctx, cmd, runOpts{sink: func(line []byte) error {
		if b.Len() < maxParse {
			b.Write(line)
			b.WriteByte('\n')
		}
		return nil
	}})
	if err != nil {
		return "", err
	}
	if r := out.rejected(); r != "" {
		return "", &rejectedError{cmd, r}
	}
	text := b.String()
	_, text, _ = strings.Cut(text, "\n") // 去掉回显
	return text, nil
}

// save 执行一条命令，把整理后的输出逐行脱敏写进 w：第一行是“提示符+命令”，最后一行是命令结束后的提示符，
// 和在终端里看到的一样。
func (s *session) save(ctx context.Context, w io.Writer, cmd string, o runOpts) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	var rd redactor
	o.sink = func(line []byte) error {
		if _, err := bw.Write(rd.line(line)); err != nil {
			return err
		}
		return bw.WriteByte('\n')
	}
	out, err := s.t.run(ctx, cmd, o)
	if err != nil {
		return err
	}
	if r := out.rejected(); r != "" {
		return &rejectedError{cmd, r}
	}
	if _, err := bw.WriteString(s.t.prompt + "\n"); err != nil {
		return err
	}
	return bw.Flush()
}

// permissionRe 匹配权限不够时设备的提示。
var permissionRe = regexp.MustCompile(`(?i)permission|authoriz|privilege|not allowed`)

// diag 执行诊断信息命令，是日志包的主文件。
func (s *session) diag(ctx context.Context, w io.Writer) error {
	p := s.facts.prof
	s.report(collect.StageTrigger, "执行 "+p.diag+"：设备整理诊断信息要几分钟到几十分钟，期间 CPU 会升高")
	start := time.Now()
	err := s.save(ctx, w, p.diag, runOpts{
		idle:    s.t.tm.Big,
		quiet:   s.t.tm.BigQuiet,
		answers: p.answers,
		progress: func(n int64) {
			s.report(collect.StageDownload, fmt.Sprintf("正在接收 %s 的输出：已收到 %s，用时 %s", p.diag, fmtBytes(n), fmtDuration(time.Since(start).Truncate(time.Second))))
		},
	})
	var rej *rejectedError
	if errors.As(err, &rej) {
		if permissionRe.MatchString(rej.msg) || p.enable && !s.t.privileged() {
			return fmt.Errorf("%w。请换用管理员权限的账号：%w", err, redfish.ErrPermission)
		}
		return fmt.Errorf("%w：%w", err, redfish.ErrUnsupported)
	}
	return err
}

// extra 执行附带的命令（日志缓冲区），返回脱敏后的输出。设备不支持这条命令时返回空，其他错误照常返回。
func (s *session) extra(ctx context.Context, cmd string) ([]byte, error) {
	s.report(collect.StageTrigger, "执行 "+cmd)
	var b bytes.Buffer
	err := s.save(ctx, &b, cmd, runOpts{})
	var rej *rejectedError
	switch {
	case errors.As(err, &rej):
		s.note("跳过 " + cmd + "：" + err.Error())
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("执行 %s：%w", cmd, err)
	}
	return b.Bytes(), nil
}

// fileName 是命令输出的文件名，例如 show-tech-support.txt。
func fileName(cmd string) string {
	return strings.Join(strings.Fields(cmd), "-") + ".txt"
}

func firstLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d 字节", n)
}
