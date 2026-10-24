// SPDX-License-Identifier: GPL-3.0-or-later

package sshx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/sshx"
	"github.com/dylanlabs/logdiag-collector/internal/sshx/sshtest"
)

// echo 是测试用的命令处理：回显输入，按命令决定输出和退出码。
func echo(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch {
	case cmd == "cat":
		io.Copy(stdout, stdin)
		return 0
	case strings.HasPrefix(cmd, "exit "):
		var code int
		fmt.Sscan(cmd[5:], &code)
		fmt.Fprintf(stderr, "退出 %d\n", code)
		return code
	case cmd == "sleep":
		<-ctx.Done()
		return -1
	case strings.HasPrefix(cmd, "print "):
		fmt.Fprint(stdout, cmd[6:])
		return 0
	case cmd == "binary":
		stdout.Write(bytes.Repeat([]byte{0x1f, 0x8b, 0, 1}, 1000))
		return 0
	}
	fmt.Fprintln(stdout, "ok:", cmd)
	return 0
}

func dial(t *testing.T, s *sshtest.Server, password string, mod ...func(*sshx.Config)) (*sshx.Client, error) {
	t.Helper()
	cfg := sshx.Config{Address: s.Addr, User: "root", Password: password, Timeout: 5 * time.Second}
	for _, m := range mod {
		m(&cfg)
	}
	c, err := sshx.Dial(context.Background(), cfg)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func TestRun(t *testing.T) {
	s := sshtest.New(t, sshtest.Options{User: "root", Password: "Secret-1", Version: "SSH-2.0-OpenSSH_8.7"}, echo)
	c, err := dial(t, s, "Secret-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerVersion() != "SSH-2.0-OpenSSH_8.7" {
		t.Errorf("ServerVersion = %q", c.ServerVersion())
	}
	if c.HostKey() != s.HostKey || c.HostKeyType() != "ssh-ed25519" {
		t.Errorf("HostKey = %s %s，应为 ssh-ed25519 %s", c.HostKeyType(), c.HostKey(), s.HostKey)
	}

	var out, errOut bytes.Buffer
	code, err := c.Run(context.Background(), "cat", strings.NewReader("你好"), &out, nil)
	if err != nil || code != 0 || out.String() != "你好" {
		t.Errorf("cat：code=%d err=%v out=%q", code, err, out.String())
	}
	code, err = c.Run(context.Background(), "exit 3", nil, nil, &errOut)
	if err != nil || code != 3 || errOut.String() != "退出 3\n" {
		t.Errorf("exit 3：code=%d err=%v stderr=%q", code, err, errOut.String())
	}
	if got := s.Commands(); len(got) != 2 || got[0] != "cat" || got[1] != "exit 3" {
		t.Errorf("服务器收到的命令 = %q", got)
	}
}

// 密码错误时，一次连接只能算一次失败：x/crypto 默认会接着用 keyboard-interactive 再发一次密码。
func TestWrongPasswordSentOnce(t *testing.T) {
	for _, methods := range [][]string{
		{"password", "keyboard-interactive"},
		{"keyboard-interactive", "password"},
		{"keyboard-interactive"},
		{"password"},
	} {
		t.Run(strings.Join(methods, "+"), func(t *testing.T) {
			s := sshtest.New(t, sshtest.Options{User: "root", Password: "Secret-1", Methods: methods}, echo)
			_, err := dial(t, s, "wrong")
			if !errors.Is(err, sshx.ErrAuth) {
				t.Fatalf("错误 = %v，应包装 ErrAuth", err)
			}
			if n := s.Failures(); n != 1 {
				t.Errorf("服务器记了 %d 次密码错误，应为 1 次", n)
			}
			if _, err := dial(t, s, "Secret-1"); err != nil {
				t.Errorf("正确的密码登录失败：%v", err)
			}
		})
	}
}

func TestNoPasswordMethod(t *testing.T) {
	s := sshtest.New(t, sshtest.Options{User: "root", Password: "Secret-1", Methods: []string{"publickey"}}, echo)
	_, err := dial(t, s, "Secret-1")
	if !errors.Is(err, sshx.ErrNoPassword) || errors.Is(err, sshx.ErrAuth) {
		t.Fatalf("错误 = %v，应包装 ErrNoPassword 而不是 ErrAuth", err)
	}
}

func TestHostKey(t *testing.T) {
	s := sshtest.New(t, sshtest.Options{User: "root", Password: "Secret-1"}, echo)
	// 记录的指纹相同：可以登录。粘贴 ssh-keygen -l 的整行输出也认。
	pasted := "256 " + s.HostKey + " root@host (ED25519)"
	if _, err := dial(t, s, "Secret-1", func(c *sshx.Config) { c.HostKey = pasted }); err != nil {
		t.Fatalf("指纹相同却登录失败：%v", err)
	}
	// 指纹不同：在发送密码之前就停下。
	_, err := dial(t, s, "wrong", func(c *sshx.Config) { c.HostKey = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" })
	var hk *sshx.HostKeyError
	if !errors.As(err, &hk) || hk.Got != s.HostKey || hk.Type != "ssh-ed25519" {
		t.Fatalf("错误 = %v，应为 HostKeyError", err)
	}
	if s.Failures() != 0 {
		t.Error("主机密钥不符时不应发送密码")
	}
}

func TestDialRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = sshx.Dial(context.Background(), sshx.Config{Address: addr, User: "root", Password: "x", Timeout: 2 * time.Second})
	var ne net.Error
	if !errors.As(err, &ne) {
		t.Fatalf("错误 = %v，应保留 net.Error 便于归类为网络不可达", err)
	}
}

func TestRunCancel(t *testing.T) {
	s := sshtest.New(t, sshtest.Options{User: "root", Password: "Secret-1"}, echo)
	c, err := dial(t, s, "Secret-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	code, err := c.Run(ctx, "sleep", nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || code != -1 {
		t.Fatalf("code=%d err=%v，应返回超时", code, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("取消后等了 %v 才返回", d)
	}
	// 连接还能继续用。
	if code, err := c.Run(context.Background(), "print x", nil, nil, nil); err != nil || code != 0 {
		t.Errorf("取消后再执行：code=%d err=%v", code, err)
	}
}

func TestRecorder(t *testing.T) {
	s := sshtest.New(t, sshtest.Options{User: "root", Password: "Secret-1"}, echo)
	rec := sshx.NewRecorder()
	rec.MaxOutput = 1024
	c, err := dial(t, s, "Secret-1", func(c *sshx.Config) { c.Recorder = rec })
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ := net.SplitHostPort(s.Addr)
	ctx := context.Background()
	c.Run(ctx, "print 密码是 Secret-1，地址是 "+host, nil, nil, nil)
	c.Run(ctx, "cat", strings.NewReader("Secret-1\n"), io.Discard, nil)
	var bin bytes.Buffer
	c.Run(ctx, "binary", nil, &bin, nil)
	rec.Note("上传到 " + host)

	r := rec.Record()
	if r.Format != sshx.RecordFormat || r.ServerVersion != "SSH-2.0-OpenSSH_9.6" || !strings.HasSuffix(r.HostKey, s.HostKey) {
		t.Errorf("记录头 = %+v", r)
	}
	if len(r.Steps) != 5 || r.Steps[0].Kind != "connect" || r.Steps[4].Kind != "note" {
		t.Fatalf("记录了 %d 步：%+v", len(r.Steps), r.Steps)
	}
	if got := r.Steps[1]; got.Command != "print 密码是 REDACTED，地址是 {{host}}" || got.Stdout.Text != "密码是 REDACTED，地址是 {{host}}" {
		t.Errorf("命令和输出没有脱敏：%q / %q", got.Command, got.Stdout.Text)
	}
	if got := r.Steps[2]; !got.Stdin || got.Stdout.Text != "REDACTED\n" {
		t.Errorf("cat 一步 = %+v", got)
	}
	got := r.Steps[3].Stdout
	if !got.Truncated || got.Size != int64(bin.Len()) || len(got.SHA256) != 64 || got.Base64 == "" {
		t.Errorf("超过上限的二进制输出应只存开头：%+v", got)
	}
	if b, _ := got.Bytes(); len(b) != 512 || !bytes.Equal(b, bin.Bytes()[:512]) {
		t.Errorf("开头部分不对：%d 字节", len(b))
	}
	if r.Steps[4].Text != "上传到 {{host}}" {
		t.Errorf("说明没有脱敏：%q", r.Steps[4].Text)
	}
	if *r.Steps[1].Exit != 0 || r.Steps[1].Error != "" {
		t.Errorf("退出码 = %v，错误 = %q", *r.Steps[1].Exit, r.Steps[1].Error)
	}
}

func TestRecorderAuthFailure(t *testing.T) {
	s := sshtest.New(t, sshtest.Options{User: "root", Password: "Secret-1"}, echo)
	rec := sshx.NewRecorder()
	if _, err := dial(t, s, "wrong-pass", func(c *sshx.Config) { c.Recorder = rec }); err == nil {
		t.Fatal("应认证失败")
	}
	r := rec.Record()
	if len(r.Steps) != 1 || r.Steps[0].Kind != "connect" || !strings.Contains(r.Steps[0].Error, "账号或密码错误") {
		t.Fatalf("记录 = %+v", r.Steps)
	}
}

func TestHostPort(t *testing.T) {
	for in, want := range map[string][2]string{
		"10.0.0.5":        {"10.0.0.5:22", "10.0.0.5"},
		" 10.0.0.5:2222 ": {"10.0.0.5:2222", "10.0.0.5"},
		"fe80::1":         {"[fe80::1]:22", "fe80::1"},
		"[fe80::1]":       {"[fe80::1]:22", "fe80::1"},
		"[fe80::1]:2200":  {"[fe80::1]:2200", "fe80::1"},
		"db01.example":    {"db01.example:22", "db01.example"},
	} {
		addr, host, err := sshx.HostPort(in)
		if err != nil || addr != want[0] || host != want[1] {
			t.Errorf("HostPort(%q) = %q %q %v，应为 %q", in, addr, host, err, want)
		}
	}
	if _, _, err := sshx.HostPort(" "); err == nil {
		t.Error("空地址应报错")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	const fp = "SHA256:Nf1xZtEzyIB0Pzpv3J0V3uYxjeG5jP2HDPbF8ckNzDg"
	for _, in := range []string{
		fp,
		" " + fp + "= ",
		"Nf1xZtEzyIB0Pzpv3J0V3uYxjeG5jP2HDPbF8ckNzDg",
		"sha256:Nf1xZtEzyIB0Pzpv3J0V3uYxjeG5jP2HDPbF8ckNzDg",
		"256 " + fp + " root@db01 (ED25519)",
	} {
		if got := sshx.NormalizeFingerprint(in); got != fp {
			t.Errorf("NormalizeFingerprint(%q) = %q", in, got)
		}
	}
	if got := sshx.NormalizeFingerprint("  "); got != "" {
		t.Errorf("空指纹 = %q", got)
	}
}
