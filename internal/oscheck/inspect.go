// SPDX-License-Identifier: GPL-3.0-or-later

// Package oscheck 通过 SSH 在服务器操作系统上运行 LogDiag 的巡检脚本，取回结果压缩包。
//
// 流程：连接登录 → 识别系统、决定提权方式 → 上传脚本并核对 → root/sudo 执行 →
// 按脚本输出找到结果压缩包并下载 → 删除上传的脚本和设备上的临时文件（取消时也删）。
//
// 脚本只做读取类收集，不改系统配置（见 scripts/ 里内置的脚本）。对设备账号遵循与 BMC
// 一致的防锁定规则：密码最多发一次，认证失败立即停止，不重试。
package oscheck

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

// DefaultCommandTimeout 是单条短命令（探测、上传、清理）的超时。脚本执行本身不受它约束，
// 由调用方的整体超时控制。
const DefaultCommandTimeout = 2 * time.Minute

// Request 是一次 OS 巡检的参数。
type Request struct {
	Address  string // 系统地址，可带端口（10.0.0.5、10.0.0.5:2222）
	User     string
	Password string // OS 账号密码，也用作 sudo 密码；用私钥登录时可为空
	Key      []byte // 私钥（可选），优先于密码登录
	HostKey  string // 期望的主机密钥指纹；为空表示首次连接，连接后由 Result.HostKey 返回

	// Elevate 为 true 时非 root 账号用 sudo 提权；false 或无法提权时按普通账号运行（数据不完整）。
	Elevate bool

	Device   collect.Device // 写进 manifest 的设备信息（type 应为 "os"）
	Store    *collect.Store
	Progress func(collect.Event)
	Recorder *sshx.Recorder

	// Timeout 是短命令的超时，0 用 DefaultCommandTimeout。
	Timeout time.Duration
	// Dial 供测试替换真实连接，nil 时用 sshx.Dial。
	Dial func(context.Context, sshx.Config) (*sshx.Client, error)
}

// Result 是巡检结果。
type Result struct {
	*collect.Result
	HostKey  string   // 本次连接的主机密钥指纹（type 和 SHA256），首次连接时保存备查
	Warnings []string // 非 root、脚本告警、临时文件清理失败等不影响结果的提醒
}

// Inspect 用 script 对一台服务器做一次 OS 巡检。
func Inspect(ctx context.Context, req Request, script Script) (*Result, error) {
	run := &run{req: req, script: script}
	return run.do(ctx)
}

type run struct {
	req      Request
	script   Script
	warnings []string
}

func (r *run) warn(format string, a ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, a...))
}

func (r *run) report(stage collect.Stage, msg string) {
	if r.req.Progress != nil {
		r.req.Progress(collect.Event{Stage: stage, Percent: -1, Message: msg})
	}
}

func (r *run) cmdTimeout() time.Duration {
	if r.req.Timeout > 0 {
		return r.req.Timeout
	}
	return DefaultCommandTimeout
}

func (r *run) do(ctx context.Context) (res *Result, err error) {
	r.report(collect.StageProbe, "连接并登录系统")
	dial := r.req.Dial
	if dial == nil {
		dial = sshx.Dial
	}
	client, err := dial(ctx, sshx.Config{
		Address:  r.req.Address,
		User:     r.req.User,
		Password: r.req.Password,
		Key:      r.req.Key,
		HostKey:  r.req.HostKey,
		Recorder: r.req.Recorder,
	})
	if err != nil {
		return nil, err
	}
	defer client.Close()
	// 长命令（巡检脚本约 1～3 分钟，含 30 秒性能采样）期间发保活，防止防火墙掐断空闲连接。
	client.KeepAlive(ctx, 0)

	sys, err := r.probe(ctx, client)
	if err != nil {
		return nil, err
	}
	mode, err := r.privilege(ctx, client, sys)
	if err != nil {
		return nil, err
	}

	// 上传脚本到独立的临时目录，用后即删。
	stage, err := r.upload(ctx, client)
	if err != nil {
		return nil, err
	}
	// 无论成功、失败还是取消，都清理设备上留下的临时文件。
	defer func() {
		if cerr := r.cleanup(ctx, client, mode, stage); cerr != nil {
			r.warn("设备上的临时文件可能没有清理干净：%v（可手动删除 %s 及其下残留）", cerr, stage.dir)
		}
		if res != nil {
			res.Warnings = r.warnings
		}
	}()

	archive, err := r.execute(ctx, client, mode, stage)
	if err != nil {
		return nil, err
	}

	cres, err := r.download(ctx, client, mode, sys, archive)
	if err != nil {
		return nil, err
	}
	return &Result{Result: cres, HostKey: client.HostKeyType() + " " + client.HostKey(), Warnings: r.warnings}, nil
}

// sysInfo 是探测到的系统信息。
type sysInfo struct {
	uname    string
	root     bool
	hasSudo  bool
	hostname string
	os       string
}

func (r *run) probe(ctx context.Context, c *sshx.Client) (sysInfo, error) {
	const probe = `echo "UNAME=$(uname -s 2>/dev/null)"; echo "UID=$(id -u 2>/dev/null)"; ` +
		`echo "HOST=$(hostname 2>/dev/null)"; . /etc/os-release 2>/dev/null; echo "OS=$PRETTY_NAME"; ` +
		`command -v sudo >/dev/null 2>&1 && echo "SUDO=yes" || echo "SUDO=no"`
	out, _, err := r.capture(ctx, c, "sh -c "+shQuote(probe), nil)
	if err != nil {
		return sysInfo{}, fmt.Errorf("识别系统失败：%w", err)
	}
	var s sysInfo
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "UNAME":
			s.uname = v
		case "UID":
			s.root = v == "0"
		case "HOST":
			s.hostname = v
		case "OS":
			s.os = v
		case "SUDO":
			s.hasSudo = v == "yes"
		}
	}
	if !strings.EqualFold(s.uname, r.script.OS) {
		return s, fmt.Errorf("这台设备的系统是 %q，不是 %s，请选对巡检脚本或设备", cmpOr(s.uname, "未知"), r.script.Title)
	}
	r.report(collect.StageProbe, "系统 "+cmpOr(s.os, s.uname))
	return s, nil
}

// mode 是提权方式。
type mode struct {
	prefix   string // 命令前缀："" / "sudo -n " / "sudo -S -p '' "
	password bool   // 是否需要通过标准输入送入密码（sudo -S）
}

func (m mode) wrap(cmd string) string { return m.prefix + cmd }

func (m mode) stdin(password string) io.Reader {
	if m.password {
		return strings.NewReader(password + "\n")
	}
	return nil
}

func (r *run) privilege(ctx context.Context, c *sshx.Client, sys sysInfo) (mode, error) {
	if sys.root {
		return mode{}, nil
	}
	if !r.req.Elevate {
		r.warn("当前账号不是 root，也没有选择 sudo 提权，部分硬件和日志信息会缺失。")
		return mode{}, nil
	}
	if !sys.hasSudo {
		r.warn("当前账号不是 root，系统上也没有 sudo，部分硬件和日志信息会缺失。")
		return mode{}, nil
	}
	// 先试免密 sudo。
	if _, code, err := r.capture(ctx, c, "sudo -n true", nil); err == nil && code == 0 {
		return mode{prefix: "sudo -n "}, nil
	}
	if r.req.Password == "" {
		r.warn("当前账号不是 root，sudo 需要密码但没有提供密码，部分硬件和日志信息会缺失。")
		return mode{}, nil
	}
	return mode{prefix: "sudo -S -p '' ", password: true}, nil
}

// stage 是设备上存放上传脚本的临时目录。
type stage struct {
	dir  string
	file string
}

func (r *run) upload(ctx context.Context, c *sshx.Client) (stage, error) {
	r.report(collect.StageTrigger, "上传巡检脚本")
	out, _, err := r.capture(ctx, c, "mktemp -d /tmp/logdiag-inspect.XXXXXX", nil)
	if err != nil {
		return stage{}, fmt.Errorf("创建临时目录失败：%w", err)
	}
	dir := strings.TrimSpace(out)
	if dir == "" || !path.IsAbs(dir) {
		return stage{}, fmt.Errorf("临时目录路径异常：%q", dir)
	}
	st := stage{dir: dir, file: path.Join(dir, r.script.Name)}

	// 用 tee 写入，避免 shell 重定向的引号问题；脚本内容经标准输入送入。
	if _, _, err := r.capture(ctx, c, "tee "+shQuote(st.file), bytes.NewReader(r.script.Content)); err != nil {
		return st, fmt.Errorf("上传脚本失败：%w", err)
	}
	// 在设备上用 cksum 核对，确认脚本没有在传输中损坏（接下来会以较高权限执行它）。
	out, _, err = r.capture(ctx, c, "cksum "+shQuote(st.file), nil)
	if err != nil {
		return st, fmt.Errorf("核对脚本失败：%w", err)
	}
	fields := strings.Fields(out)
	want := cksum(r.script.Content)
	if len(fields) < 2 || fields[0] != strconv.FormatUint(uint64(want), 10) || fields[1] != strconv.Itoa(len(r.script.Content)) {
		return st, fmt.Errorf("上传的脚本校验和不符（本地 %d/%d，设备 %q），已中止", want, len(r.script.Content), out)
	}
	return st, nil
}

// savedPrefix 是脚本最后打印的结果路径行的前缀。
const savedPrefix = "Inspection data saved to "

func (r *run) execute(ctx context.Context, c *sshx.Client, m mode, st stage) (string, error) {
	r.report(collect.StageGenerate, "开始巡检")
	// 边执行边把脚本的 "Collecting ..." 进度转发给界面，同时记下结果路径和告警行。
	var saved, lastProgress string
	sc := &lineWriter{fn: func(line string) {
		switch {
		case strings.HasPrefix(line, savedPrefix):
			saved = strings.TrimSpace(line[len(savedPrefix):])
		case strings.HasPrefix(line, "WARNING:"):
			r.warn("脚本提示：%s", strings.TrimSpace(line))
		case strings.HasSuffix(strings.TrimSpace(line), "..."):
			lastProgress = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "..."))
			r.report(collect.StageGenerate, lastProgress)
		}
	}}
	var stderr bytes.Buffer
	cmd := m.wrap("bash " + shQuote(st.file))
	code, err := c.Run(ctx, cmd, m.stdin(r.req.Password), sc, &stderr)
	sc.flush()
	if err != nil {
		return "", fmt.Errorf("执行巡检脚本失败：%w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("巡检脚本以退出码 %d 结束%s", code, tail(stderr.String()))
	}
	if saved == "" {
		return "", fmt.Errorf("没有在脚本输出里找到结果文件路径%s", tail(stderr.String()))
	}
	if !path.IsAbs(saved) || path.Dir(saved) != r.script.OutDir {
		return "", fmt.Errorf("脚本给出的结果路径异常：%q", saved)
	}
	return saved, nil
}

func (r *run) download(ctx context.Context, c *sshx.Client, m mode, sys sysInfo, archive string) (*collect.Result, error) {
	r.report(collect.StageDownload, "取回结果")
	dev := r.req.Device
	if dev.Type == "" {
		dev.Type = "os"
	}
	if dev.OS == "" {
		dev.OS = sys.os
	}
	if dev.Hostname == "" {
		dev.Hostname = sys.hostname
	}
	if dev.HostIP == "" {
		dev.HostIP = r.req.Address
	}
	job := &collect.Job{Device: dev, Store: r.req.Store, Progress: r.req.Progress}
	log := collect.Log{Type: r.script.LogType, Method: "ssh"}
	// 用执行脚本的同等权限读取结果文件：sudo 执行时文件属于 root，普通账号读不了。
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := m.wrap("cat " + shQuote(archive))
	res, err := job.Save(log, path.Base(archive), func(w io.Writer) error {
		code, err := c.Run(cctx, cmd, m.stdin(r.req.Password), w, io.Discard)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("读取结果文件失败，退出码 %d", code)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// cleanup 删除上传的脚本目录，以及脚本在 OutDir 留下的结果文件和（执行中断时的）工作目录。
// 用 context.WithoutCancel：主流程被取消时也要清理。
func (r *run) cleanup(ctx context.Context, c *sshx.Client, m mode, st stage) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	if st.dir == "" {
		return nil
	}
	// 先以上传目录为时间基准，找出本次在 OutDir 下产生、比它更新的结果文件和残留工作目录
	// （脚本因中断没来得及删除的那种）。必须在删除上传目录之前做，否则 -newer 的基准就没了。
	find := fmt.Sprintf("find %s -maxdepth 1 -newer %s -name %s -print",
		shQuote(r.script.OutDir), shQuote(st.dir), shQuote(r.script.OutPattern))
	out, _, err := r.capture(ctx, c, "sh -c "+shQuote(find), nil)
	if err != nil {
		errs = append(errs, err)
	}
	// 上传目录属于登录账号，直接删。
	if _, _, err := r.capture(ctx, c, "rm -rf "+shQuote(st.dir), nil); err != nil {
		errs = append(errs, err)
	}
	// 结果文件和残留工作目录可能属于 root（经 sudo 执行时），用同等权限删。
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !r.safeToRemove(p) {
			r.warn("跳过清理可疑路径：%q", p)
			continue
		}
		if _, _, err := r.capture(ctx, c, m.wrap("rm -rf "+shQuote(p)), m.stdin(r.req.Password)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// safeToRemove 确认路径确实在脚本的输出目录下、且像脚本产出的文件名，避免 find 结果异常时误删。
func (r *run) safeToRemove(p string) bool {
	if !path.IsAbs(p) || path.Dir(p) != r.script.OutDir {
		return false
	}
	base := path.Base(p)
	return base != "" && base != "." && base != ".." && strings.Contains(base, ".pm-")
}

// capture 执行一条短命令，收集标准输出，返回输出、退出码和错误。
func (r *run) capture(ctx context.Context, c *sshx.Client, cmd string, stdin io.Reader) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cmdTimeout())
	defer cancel()
	var out, errb bytes.Buffer
	code, err := c.Run(ctx, cmd, stdin, &out, &errb)
	if err != nil {
		return out.String(), code, err
	}
	if code != 0 {
		return out.String(), code, fmt.Errorf("命令退出码 %d%s", code, tail(errb.String()))
	}
	return out.String(), code, nil
}

// lineWriter 把写入的内容按行拆开，逐行回调；适合转发脚本的进度输出。
type lineWriter struct {
	fn  func(line string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(stripANSI(string(w.buf[:i])))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.fn(stripANSI(string(w.buf)))
		w.buf = nil
	}
}

// stripANSI 去掉终端颜色等转义序列，让进度行干净。
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return strings.TrimRight(s, "\r")
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			// 跳过 CSI 序列 ESC [ ... 字母。
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !(s[j] >= '@' && s[j] <= '~') {
					j++
				}
				i = j
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return strings.TrimRight(b.String(), "\r")
}

// tail 把命令的错误输出压成一行附在错误后面，便于定位。
func tail(stderr string) string {
	stderr = strings.TrimSpace(stripANSI(stderr))
	if stderr == "" {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(stderr))
	var lines []string
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return "：" + strings.Join(lines, "；")
}

// shQuote 把字符串包成单引号形式，可安全地拼进 /bin/sh 命令。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
