// SPDX-License-Identifier: GPL-3.0-or-later

// Package sshx 是 SSH 连接的公共部分：连接、密码认证、主机密钥核对、执行命令、保活和诊断记录。
// OS 巡检和网络设备收集共用。
//
// 防止账号被锁定：一次连接里密码最多发送一次（password 和 keyboard-interactive 两种方式合计），
// 认证失败返回 ErrAuth，调用方不要重试。设备流量直连，不走系统代理。
package sshx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultPort 是 SSH 的默认端口。
const DefaultPort = "22"

// DefaultTimeout 是 TCP 连接加握手、认证的默认超时。老设备握手慢，给 30 秒。
const DefaultTimeout = 30 * time.Second

// DefaultKeepAlive 是 KeepAlive 的默认间隔。
const DefaultKeepAlive = time.Minute

var (
	// ErrAuth 表示账号或密码错误（或服务器拒绝了这个账号）。调用方不要自动重试，免得账号被锁定。
	ErrAuth = errors.New("账号或密码错误")
	// ErrNoPassword 表示服务器不接受密码登录（例如只允许密钥，或要求动态口令），密码没有发出。
	ErrNoPassword = errors.New("服务器不接受密码登录")
	// ErrAlgorithms 表示和服务器没有共同支持的加密算法。
	ErrAlgorithms = errors.New("和服务器没有共同支持的 SSH 加密算法")
)

// errPasswordSent 是第二种认证方式又来要密码时，回调返回的错误：第一次已经被拒绝了。
var errPasswordSent = errors.New("密码已经发送过一次")

// errExtraFactor 是 keyboard-interactive 一轮里有多个提问（例如密码加动态口令）时回调返回的错误。
var errExtraFactor = errors.New("服务器要求密码以外的验证")

// HostKeyError 表示服务器的主机密钥和期望的不一致：可能重装过系统，也可能有人冒充这台服务器。
type HostKeyError struct {
	Want string // 期望的指纹
	Got  string // 这次收到的指纹
	Type string // 这次收到的密钥类型
}

func (e *HostKeyError) Error() string {
	return fmt.Sprintf("服务器的主机密钥和上次记录的不一致（记录的是 %s，这次是 %s %s）", e.Want, e.Type, e.Got)
}

// Config 是 Dial 的参数。
type Config struct {
	// Address 是主机名或 IP，可以带端口（10.0.0.5:2222、[fe80::1]:22），不带时用 22。
	Address  string
	User     string
	Password string
	// HostKey 是期望的主机密钥指纹（SHA256:… 形式）。为空时接受服务器给出的密钥（首次连接），
	// 连接后用 Client.HostKey 取得指纹保存下来，下次连接时传入。
	HostKey string
	// Timeout 是 TCP 连接和握手、认证的超时，0 表示 DefaultTimeout。
	Timeout time.Duration
	// ClientVersion 是发给服务器的版本串，为空时用 "SSH-2.0-LogDiagCollector"。
	ClientVersion string
	// Recorder 记录这次连接的诊断记录，可为 nil。
	Recorder *Recorder
}

// Client 是一个已登录的 SSH 连接，可以被多个 goroutine 同时使用。
type Client struct {
	c             *ssh.Client
	rec           *Recorder
	hostKey       string
	hostKeyType   string
	serverVersion string
}

// Dial 连接并登录服务器。认证失败的错误包装 ErrAuth；主机密钥不符返回 *HostKeyError，此时密码不会发出。
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	addr, host, err := HostPort(cfg.Address)
	if err != nil {
		return nil, err
	}
	timeout := cmp.Or(cfg.Timeout, DefaultTimeout)
	c := &Client{rec: cfg.Recorder}
	c.rec.begin(host, cfg.Password)
	start := time.Now()
	c.c, err = dial(ctx, c, cfg, addr, timeout)
	c.rec.connected(c, time.Since(start), err)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func dial(ctx context.Context, c *Client, cfg Config, addr string, timeout time.Duration) (*ssh.Client, error) {
	// 密码只发一次：两种方式共用这个标记。password 方式被拒绝后，x/crypto 还会接着试
	// keyboard-interactive，这时回调直接报错，不再发送密码，免得一次连接算两次失败。
	sent := false
	password := func() (string, error) {
		if sent {
			return "", errPasswordSent
		}
		sent = true
		return cfg.Password, nil
	}
	kbd := func(_, _ string, questions []string, _ []bool) ([]string, error) {
		switch len(questions) {
		case 0:
			return []string{}, nil // 只显示提示、没有提问的轮次照常回答
		case 1:
			p, err := password()
			return []string{p}, err
		}
		return nil, errExtraFactor
	}
	// 主机密钥回调在握手协程里调用，连接中途重新协商密钥时还会再调用。
	var mu sync.Mutex
	var seenKey, seenType string
	conf := &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{ssh.PasswordCallback(password), ssh.KeyboardInteractive(kbd)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			mu.Lock()
			defer mu.Unlock()
			want := NormalizeFingerprint(cfg.HostKey)
			if seenKey != "" {
				want = seenKey // 连接中途重新协商密钥时，服务器必须还是同一把主机密钥
			}
			if want != "" && fp != want {
				return &HostKeyError{Want: want, Got: fp, Type: key.Type()}
			}
			seenKey, seenType = fp, key.Type()
			return nil
		},
		ClientVersion: cmp.Or(cfg.ClientVersion, "SSH-2.0-LogDiagCollector"),
		Timeout:       timeout,
	}
	// 安全算法在前，有安全问题的旧算法附在后面：新系统总是协商到安全算法，
	// 只有老系统、老交换机才会用到旧算法。协商过程受密钥交换保护，不能被中间人降级。
	sup, old := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
	conf.KeyExchanges = slices.Concat(sup.KeyExchanges, old.KeyExchanges)
	conf.Ciphers = slices.Concat(sup.Ciphers, old.Ciphers)
	conf.MACs = slices.Concat(sup.MACs, old.MACs)
	conf.HostKeyAlgorithms = slices.Concat(sup.HostKeys, old.HostKeys)

	d := net.Dialer{Timeout: timeout}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("连接 SSH 端口：%w", err)
	}
	// 握手和认证也受超时和 ctx 约束：到时把连接的截止时间提前，阻塞的读写立即返回。
	conn.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, conf)
	if !stop() || ctx.Err() != nil {
		if err == nil {
			sc.Close()
		}
		conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, classify(err, sent)
	}
	conn.SetDeadline(time.Time{})
	mu.Lock()
	c.hostKey, c.hostKeyType = seenKey, seenType
	mu.Unlock()
	c.serverVersion = string(sc.ServerVersion())
	return ssh.NewClient(sc, chans, reqs), nil
}

// classify 把握手和认证的错误换成调用方能判断的类型。
func classify(err error, sent bool) error {
	var hk *HostKeyError
	if errors.As(err, &hk) {
		return hk
	}
	msg := err.Error()
	switch {
	case errors.Is(err, errPasswordSent):
		return fmt.Errorf("%w（服务器拒绝了这个账号或密码）", ErrAuth)
	case errors.Is(err, errExtraFactor):
		return fmt.Errorf("%w：要求动态口令等额外验证，本工具不支持", ErrNoPassword)
	case strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "Too many authentication failures"):
		if sent {
			return fmt.Errorf("%w（服务器拒绝了这个账号或密码）", ErrAuth)
		}
		return fmt.Errorf("%w，只允许密钥等其他方式登录", ErrNoPassword)
	case strings.Contains(msg, "no common algorithm"):
		return fmt.Errorf("%w：%v", ErrAlgorithms, err)
	}
	if sent && strings.Contains(msg, "disconnect") {
		// 有的设备认证失败直接断开连接，不回失败消息。
		return fmt.Errorf("%w（发送密码后服务器断开了连接）：%v", ErrAuth, err)
	}
	return fmt.Errorf("SSH 握手失败：%w", err)
}

// HostPort 把地址整理成 host:port（不带端口时用 22），并返回不带端口的主机。
func HostPort(address string) (addr, host string, err error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", "", errors.New("没有填写地址")
	}
	if h, p, err := net.SplitHostPort(address); err == nil {
		return net.JoinHostPort(h, p), h, nil
	}
	h := strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	return net.JoinHostPort(h, DefaultPort), h, nil
}

// NormalizeFingerprint 整理用户填写的主机密钥指纹，统一成 SHA256:… 形式。
// 可以直接粘贴 ssh-keygen -l 的输出（“256 SHA256:… root@host (ED25519)”），也可以不带 SHA256: 前缀。
func NormalizeFingerprint(fp string) string {
	if i := strings.Index(strings.ToUpper(fp), "SHA256:"); i >= 0 {
		fp = fp[i+len("SHA256:"):]
	}
	f := strings.Fields(fp)
	if len(f) == 0 {
		return ""
	}
	return "SHA256:" + strings.TrimRight(f[0], "=")
}

// HostKey 返回服务器主机密钥的 SHA256 指纹，例如 SHA256:Nf1x…。
func (c *Client) HostKey() string { return c.hostKey }

// HostKeyType 返回服务器主机密钥的类型，例如 ssh-ed25519。
func (c *Client) HostKeyType() string { return c.hostKeyType }

// ServerVersion 返回服务器的版本串原文，例如 SSH-2.0-OpenSSH_8.7。
func (c *Client) ServerVersion() string { return c.serverVersion }

// Raw 返回底层连接，供交互式 shell 等本包没有封装的用法。
func (c *Client) Raw() *ssh.Client { return c.c }

// Close 断开连接。
func (c *Client) Close() error { return c.c.Close() }

// Run 在服务器上执行一条命令（exec 通道，不分配终端），等它结束，返回退出码。
// stdin、stdout、stderr 都可以为 nil。命令没给出退出码（例如连接断开）或被信号终止时返回 -1 和错误。
// ctx 取消时关闭通道并返回 ctx 的错误；服务器上的进程是否随之结束由服务器决定。
func (c *Client) Run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	step := c.rec.exec(cmd, stdin != nil)
	exit, err := c.run(ctx, cmd, stdin, step.stdout(stdout), step.stderr(stderr))
	step.finish(exit, err)
	return exit, err
}

func (c *Client) run(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	s, err := c.c.NewSession()
	if err != nil {
		return -1, fmt.Errorf("打开 SSH 会话：%w", err)
	}
	defer s.Close()
	s.Stdin, s.Stdout, s.Stderr = stdin, stdout, stderr
	if err := s.Start(cmd); err != nil {
		return -1, fmt.Errorf("执行命令：%w", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		s.Signal(ssh.SIGTERM) // OpenSSH 8.1 起支持；不支持时关闭通道也会让多数命令结束
		s.Close()
		<-done
		return -1, ctx.Err()
	}
	return exitStatus(err)
}

func exitStatus(err error) (int, error) {
	var ee *ssh.ExitError
	var em *ssh.ExitMissingError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee) && ee.Signal() != "":
		return -1, fmt.Errorf("命令被信号 %s 终止", ee.Signal())
	case errors.As(err, &ee):
		return ee.ExitStatus(), nil
	case errors.As(err, &em):
		return -1, errors.New("命令没有返回退出码，连接可能已经断开")
	}
	return -1, err
}

// KeepAlive 每隔 interval（0 表示 DefaultKeepAlive）发一次 keepalive@openssh.com，直到 ctx 结束或连接关闭。
// 长时间没有输出的命令期间用它防止防火墙断开空闲连接。发送失败只记进诊断记录，不断开连接。
func (c *Client) KeepAlive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(cmp.Or(interval, DefaultKeepAlive))
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if _, _, err := c.c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					return
				}
				c.rec.Note("保活请求失败：" + err.Error())
			}
		}
	}()
}
