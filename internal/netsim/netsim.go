// SPDX-License-Identifier: GPL-3.0-or-later

// Package netsim 模拟 Cisco、华为、新华三网络设备的 SSH 命令行，供测试和演示用：登录提示、分页、
// enable、关闭分页的命令、诊断信息命令的提问和输出。
//
// 输出是按各厂商命令参考和配置手册里的格式编写的合成内容，不是真机输出，不能当作真机验证结果。
package netsim

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Server 是一台模拟设备的 SSH 服务。
type Server struct {
	Addr    string // 监听地址，例如 127.0.0.1:2201
	HostKey string // 主机密钥的 SHA256 指纹

	d    *Device
	ln   net.Listener
	conf *ssh.ServerConfig

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	commands []string
	failures int
	logins   int
	wg       sync.WaitGroup
}

// Listen 在 addr 上启动模拟设备。addr 用 127.0.0.1:0 时随机选端口。
func Listen(addr string, d *Device) (*Server, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Addr: ln.Addr().String(), HostKey: ssh.FingerprintSHA256(signer.PublicKey()), d: d, ln: ln, conns: map[net.Conn]struct{}{}}
	s.conf = &ssh.ServerConfig{ServerVersion: d.ServerVersion, NoClientAuth: d.ShellLogin}
	s.conf.PasswordCallback = func(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if c.User() == d.User && string(password) == d.Password {
			return nil, nil
		}
		s.fail()
		return nil, errors.New("密码错误")
	}
	s.conf.AddHostKey(signer)
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Close 停止服务并断开所有连接。
func (s *Server) Close() error {
	err := s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

// Commands 返回到目前为止收到的命令行（含 enable 等，不含密码）。
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// Failures 返回密码错误的次数（SSH 登录、命令行登录和 enable 合计）。
func (s *Server) Failures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures
}

// Logins 返回打开过的命令行会话数。
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

func (s *Server) fail() {
	s.mu.Lock()
	s.failures++
	s.mu.Unlock()
}

func (s *Server) command(line string) {
	s.mu.Lock()
	s.commands = append(s.commands, line)
	s.mu.Unlock()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[nc] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, nc)
				s.mu.Unlock()
				nc.Close()
			}()
			s.serve(nc)
		}()
	}
}

func (s *Server) serve(nc net.Conn) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.conf)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
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

// session 处理一个会话：申请终端后打开命令行；不支持 exec。
func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	done := make(chan struct{})
	started := false
	for {
		select {
		case req, ok := <-reqs:
			if !ok {
				if started {
					<-done
				}
				return
			}
			switch req.Type {
			case "pty-req", "env", "window-change":
				req.Reply(true, nil)
			case "shell":
				if started {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				started = true
				s.mu.Lock()
				s.logins++
				s.mu.Unlock()
				go func() {
					defer close(done)
					newCLI(s, ch).run()
				}()
			default:
				req.Reply(false, nil)
			}
		case <-done:
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		}
	}
}

// String 返回服务的说明，供命令行显示。
func (s *Server) String() string {
	return fmt.Sprintf("%s（%s，账号 %s）监听 %s", s.d.Kind, s.d.Hostname, s.d.User, s.Addr)
}
