// SPDX-License-Identifier: GPL-3.0-or-later

package netdev

import "bytes"

// maxLine 是一行的长度上限。设备输出了没有换行的大段内容（例如二进制乱码）时强制换行，
// 免得末行无限变长、拖慢提示符判断。
const maxLine = 64 << 10

// cleaner 把设备终端输出的原始字节整理成纯文本行，模拟终端的光标：
//
//   - 去掉 ANSI 控制序列；光标左移（ESC[nD）、退格、回车按光标移动处理，后面的字符覆盖原有内容。
//     分页提示（--More--、---- More ----）被设备用这些序列擦掉后，不会留在输出里。
//   - \r\n、\n\r 都算一次换行；行尾空白去掉。
//   - 其他控制字符（响铃等）丢弃。
//
// 数据可以按任意边界分块送进来，跨块的控制序列会拼起来处理。
type cleaner struct {
	line []byte // 还没换行的当前行
	cur  int    // 光标在 line 里的位置
	esc  []byte // 还没结束的控制序列
}

// write 处理一块输出，每凑齐一行（不含换行符）调用一次 emit。emit 返回错误时停止处理并返回该错误。
func (c *cleaner) write(p []byte, emit func(line []byte) error) error {
	for _, b := range p {
		if len(c.esc) > 0 {
			c.escape(b)
			continue
		}
		switch {
		case b == 0x1b:
			c.esc = append(c.esc, b)
		case b == '\n':
			if err := c.flush(emit); err != nil {
				return err
			}
		case b == '\r':
			c.cur = 0
		case b == '\b':
			if c.cur > 0 {
				c.cur--
			}
		case b == '\t' || b >= 0x20 && b != 0x7f:
			c.put(b)
			if len(c.line) >= maxLine {
				if err := c.flush(emit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *cleaner) put(b byte) {
	if c.cur < len(c.line) {
		c.line[c.cur] = b
	} else {
		c.line = append(c.line, b)
	}
	c.cur++
}

func (c *cleaner) flush(emit func([]byte) error) error {
	line := bytes.TrimRight(c.line, " \t")
	err := emit(line)
	c.line, c.cur = c.line[:0], 0
	return err
}

// escape 处理控制序列的一个字节。只模拟会影响文本的几种：光标左移、右移、清除到行尾、清除整行。
func (c *cleaner) escape(b byte) {
	c.esc = append(c.esc, b)
	seq := c.esc
	switch {
	case len(seq) == 2 && b != '[' && b != ']' && b != '(' && b != ')':
		c.esc = c.esc[:0] // ESC 7、ESC = 这类两字节序列
	case len(seq) == 3 && (seq[1] == '(' || seq[1] == ')'):
		c.esc = c.esc[:0] // 字符集选择
	case seq[1] == ']':
		// OSC：以 BEL 或 ESC \ 结束
		if b == 0x07 || len(seq) >= 2 && seq[len(seq)-2] == 0x1b && b == '\\' {
			c.esc = c.esc[:0]
		}
	case seq[1] == '[' && len(seq) > 2 && b >= 0x40 && b <= 0x7e:
		c.csi(seq[2:len(seq)-1], b)
		c.esc = c.esc[:0]
	}
	if len(c.esc) > 32 {
		c.esc = c.esc[:0] // 不认识的长序列直接丢掉
	}
}

func (c *cleaner) csi(param []byte, final byte) {
	n := 0
	for _, d := range param {
		if d < '0' || d > '9' {
			n = -1
			break
		}
		n = min(n*10+int(d-'0'), maxLine)
	}
	switch final {
	case 'D': // 光标左移
		c.cur = max(0, c.cur-max(n, 1))
	case 'C': // 光标右移，超出行尾时补空格
		for i := 0; i < max(n, 1); i++ {
			if c.cur >= len(c.line) {
				c.line = append(c.line, ' ')
			}
			c.cur++
		}
	case 'K':
		switch n {
		case 0, -1: // 清除到行尾
			c.line = c.line[:c.cur]
		case 2: // 清除整行，光标不动
			for i := range c.line {
				c.line[i] = ' '
			}
		}
	}
}

// partial 返回还没换行的当前行，去掉行尾空白。提示符、分页提示和设备的提问都在这里。
func (c *cleaner) partial() string {
	return string(bytes.TrimRight(c.line, " \t"))
}
