// SPDX-License-Identifier: GPL-3.0-or-later

// Package sshtest 是测试用的 SSH 服务器：在本机随机端口监听，用密码或 keyboard-interactive 登录，
// 把每个 exec 请求交给 Handler 处理。sshx、OS 巡检和网络设备收集的测试共用。
package sshtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Handler 执行一条命令，返回退出码。ctx 在客户端发来信号或关闭通道时取消。
type Handler func(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) int

// Options 是测试服务器的设置。
type Options struct {
	User, Password string
	// Methods 是服务器接受的认证方式：password、keyboard-interactive、publickey（只认 AuthorizedKey）。
	// 为空时接受 password 和 keyboard-interactive。
	Methods []string
	// Version 是服务器版本串，为空时用 SSH-2.0-OpenSSH_9.6。
	Version string
	// AuthorizedKey 是允许登录的公钥，设置后服务器也接受用这把密钥登录（publickey 方式）。
	AuthorizedKey ssh.PublicKey
}

// Server 是测试用的 SSH 服务器，测试结束时自动关闭。
type Server struct {
	Addr    string // 127.0.0.1:端口
	HostKey string // 主机密钥的 SHA256 指纹

	opts    Options
	handler Handler
	ln      net.Listener

	mu       sync.Mutex
	commands []string
	failures int
	wg       sync.WaitGroup
}

// New 启动测试服务器。
func New(t testing.TB, o Options, h Handler) *Server {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: ln.Addr().String(), HostKey: ssh.FingerprintSHA256(signer.PublicKey()), opts: o, handler: h, ln: ln}
	conf := s.config()
	conf.AddHostKey(signer)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(nc, conf)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *Server) config() *ssh.ServerConfig {
	conf := &ssh.ServerConfig{ServerVersion: s.opts.Version}
	if conf.ServerVersion == "" {
		conf.ServerVersion = "SSH-2.0-OpenSSH_9.6"
	}
	methods := s.opts.Methods
	if len(methods) == 0 {
		methods = []string{"password", "keyboard-interactive"}
	}
	check := func(user, password string) (*ssh.Permissions, error) {
		if user == s.opts.User && password == s.opts.Password {
			return nil, nil
		}
		s.mu.Lock()
		s.failures++
		s.mu.Unlock()
		return nil, errors.New("密码错误")
	}
	if slices.Contains(methods, "password") {
		conf.PasswordCallback = func(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return check(c.User(), string(password))
		}
	}
	if slices.Contains(methods, "keyboard-interactive") {
		conf.KeyboardInteractiveCallback = func(c ssh.ConnMetadata, ask ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			answers, err := ask("", "", []string{"Password: "}, []bool{false})
			if err != nil {
				return nil, err
			}
			if len(answers) != 1 {
				return nil, errors.New("回答数量不对")
			}
			return check(c.User(), answers[0])
		}
	}
	if slices.Contains(methods, "publickey") || s.opts.AuthorizedKey != nil {
		conf.PublicKeyCallback = func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if k := s.opts.AuthorizedKey; k != nil && c.User() == s.opts.User && bytes.Equal(key.Marshal(), k.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("密钥不认识")
		}
	}
	return conf
}

// Commands 返回到目前为止收到的命令。
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// Failures 返回认证失败的次数（密码不对，两种方式合计）。
func (s *Server) Failures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures
}

func (s *Server) serve(nc net.Conn, conf *ssh.ServerConfig) {
	defer nc.Close()
	sc, chans, reqs, err := ssh.NewServerConn(nc, conf)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs) // keepalive@openssh.com 回答“不支持”，和 OpenSSH 一样
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "只支持 session")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.session(ch, chReqs)
		}()
	}
}

// session 处理一个会话：收到 exec 后执行命令，结束时回送退出码并关闭通道。
func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	started := false
	for {
		select {
		case req, ok := <-reqs:
			if !ok {
				// 客户端关闭了通道：取消命令，等它结束。
				cancel()
				if started {
					<-done
				}
				return
			}
			switch req.Type {
			case "exec":
				var p struct{ Command string }
				if started || ssh.Unmarshal(req.Payload, &p) != nil {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				started = true
				s.mu.Lock()
				s.commands = append(s.commands, p.Command)
				s.mu.Unlock()
				go func() { done <- s.handler(ctx, p.Command, ch, ch, ch.Stderr()) }()
			case "signal":
				cancel()
			case "env", "pty-req":
				req.Reply(true, nil)
			default:
				req.Reply(false, nil)
			}
		case code := <-done:
			if code < 0 {
				// 被信号终止：和 OpenSSH 一样回送 exit-signal。
				ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
					Signal     string
					CoreDumped bool
					Message    string
					Lang       string
				}{"TERM", false, "", ""}))
			} else {
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			}
			return
		}
	}
}

// LocalShell 用本机的 sh 执行命令，测试上传、执行、下载的完整流程，只能在类 Unix 系统上用。
// dir 是命令的工作目录，env 追加到环境变量里（例如把模拟的 sudo 放到 PATH 前面）。
// 命令被取消时返回 -1。
func LocalShell(dir string, env ...string) Handler {
	return func(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Dir = dir
		c.Env = append(os.Environ(), env...)
		c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
		c.WaitDelay = time.Second
		err := c.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0
		case ctx.Err() != nil:
			return -1
		case errors.As(err, &ee):
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, err)
		return 127
	}
}
