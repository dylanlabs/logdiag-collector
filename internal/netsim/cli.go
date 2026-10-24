// SPDX-License-Identifier: GPL-3.0-or-later

package netsim

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"time"
)

// errQuit 表示会话结束（用户退出、密码错误次数用完或连接断开）。
var errQuit = errors.New("会话结束")

// cli 是一个会话的命令行：读一行、回显、执行、按页输出。
type cli struct {
	s      *Server
	d      *Device
	w      io.Writer
	r      *bufio.Reader
	priv   bool // Cisco 的特权模式
	paging bool // 分页是否开启
}

func newCLI(s *Server, rw io.ReadWriter) *cli {
	return &cli{s: s, d: s.d, w: rw, r: bufio.NewReader(rw), priv: s.d.Enable == "", paging: true}
}

func (c *cli) run() {
	if err := c.login(); err != nil {
		return
	}
	for {
		if err := c.write(c.prompt()); err != nil {
			return
		}
		line, err := c.readLine(true)
		if err != nil {
			return
		}
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		c.s.command(line)
		if err := c.exec(line); err != nil {
			return
		}
	}
}

func (c *cli) prompt() string {
	switch {
	case c.d.vrp():
		return "<" + c.d.Hostname + ">"
	case c.priv:
		return c.d.Hostname + "#"
	}
	return c.d.Hostname + ">"
}

// login 是登录后、出现提示符之前的部分：命令行里的账号密码、提示信息、修改密码的提问。
func (c *cli) login() error {
	if c.d.ShellLogin {
		ok := false
		for range 3 {
			if err := c.write("\r\nUser Name:"); err != nil {
				return err
			}
			user, err := c.readLine(true)
			if err != nil {
				return err
			}
			if err := c.write("Password:"); err != nil {
				return err
			}
			pass, err := c.readLine(false)
			if err != nil {
				return err
			}
			if user == c.d.User && pass == c.d.Password {
				ok = true
				break
			}
			c.s.fail()
			c.write("\r\n% Authentication failed\r\n")
		}
		if !ok {
			return errQuit
		}
	}
	if err := c.write(crlf(c.d.banner())); err != nil {
		return err
	}
	switch {
	case c.d.ForceChange:
		c.write("\r\nFirst login or password reset. For security reasons you will need to change your password. Please enter your password.\r\nold password:")
		c.readLine(false)
		return errQuit // 模拟器不实现改密码
	case c.d.AskChange:
		if err := c.write("\r\nWarning: The password will expire in 5 days.\r\nThe password needs to be changed. Change now? [Y/N]:"); err != nil {
			return err
		}
		ans, err := c.readLine(true)
		if err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(ans), "y") {
			c.write("\r\nPlease enter old password:")
			c.readLine(false)
			return errQuit
		}
	}
	return c.write("\r\n")
}

// readLine 读一行，echo 时回显（输入密码时不回显）。\r、\n、\r\n 都算行尾。
func (c *cli) readLine(echo bool) (string, error) {
	var b strings.Builder
	for {
		ch, err := c.r.ReadByte()
		if err != nil {
			return "", err
		}
		switch {
		case ch == '\r' || ch == '\n':
			if ch == '\r' {
				if next, err := c.r.Peek(1); err == nil && next[0] == '\n' {
					c.r.ReadByte()
				}
			}
			return b.String(), c.write("\r\n")
		case ch == 0x7f || ch == '\b':
			if s := b.String(); s != "" {
				b.Reset()
				b.WriteString(s[:len(s)-1])
				if echo {
					c.write("\b \b")
				}
			}
		case ch >= 0x20:
			b.WriteByte(ch)
			if echo {
				if err := c.write(string(ch)); err != nil {
					return "", err
				}
			}
		}
	}
}

func (c *cli) write(s string) error {
	_, err := io.WriteString(c.w, s)
	return err
}

// exec 执行一条命令。
func (c *cli) exec(line string) error {
	if line == "quit" || line == "exit" || line == "logout" {
		return errQuit
	}
	if !c.d.vrp() && line == "enable" {
		return c.enable()
	}
	out, ok := c.d.command(line, c.priv)
	switch {
	case !ok:
		return c.write(crlf(c.d.invalid(line)))
	case out.paging == pagingOff:
		c.paging = false
	case out.paging == pagingOn:
		c.paging = true
	}
	if out.ask != "" {
		if err := c.write(out.ask); err != nil {
			return err
		}
		ans, err := c.readLine(true)
		if err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(ans), out.want) {
			return c.write("\r\n")
		}
	}
	return c.page(out)
}

// enable 进入 Cisco 的特权模式。IOS 密码错误时再问，最多三次；ASA 错一次就回到普通模式。
func (c *cli) enable() error {
	if c.priv {
		return nil
	}
	for range 3 {
		if err := c.write("Password: "); err != nil {
			return err
		}
		pass, err := c.readLine(false)
		if err != nil {
			return err
		}
		if pass == c.d.Enable {
			c.priv = true
			return nil
		}
		c.s.fail()
		if c.d.Kind == "asa" {
			return c.write("Invalid password\r\n")
		}
	}
	return c.write("% Bad secrets\r\n\r\n")
}

// page 按页输出。分页开启时每页之后显示分页提示，等一个按键：空格下一页，回车下一行，q 结束。
func (c *cli) page(out output) error {
	lines := out.lines()
	pageLen := c.d.PageLines
	if pageLen <= 0 {
		pageLen = 24
	}
	n := 0
	for i, l := range lines {
		if out.pauseAt > 0 && i == out.pauseAt {
			// 模拟设备整理某一部分时停顿：先输出半行，过一会儿再补完。
			if err := c.write("Collecting data, please wait"); err != nil {
				return err
			}
			time.Sleep(c.d.pause())
			if err := c.write(" ... done\r\n"); err != nil {
				return err
			}
		}
		if err := c.write(l + "\r\n"); err != nil {
			return err
		}
		n++
		if !c.paging || n < pageLen-1 || i == len(lines)-1 {
			continue
		}
		more, erase := c.d.more()
		if err := c.write(more); err != nil {
			return err
		}
		key, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		if err := c.write(erase); err != nil {
			return err
		}
		switch key {
		case ' ':
			n = 0
		case '\r', '\n':
			n = pageLen - 2
		default:
			return nil
		}
	}
	return nil
}

func crlf(s string) string {
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\r\n") + "\r\n"
}
