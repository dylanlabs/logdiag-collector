// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/httprec"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// deviceConn 是 probe、collect 连接设备用的 HTTP 客户端：不走系统代理，可选录制诊断记录。
type deviceConn struct {
	hc          *http.Client
	rec         *httprec.Recorder
	fingerprint atomic.Value // 第一次握手时的证书指纹
}

func newDeviceConn(legacy, record bool) *deviceConn {
	d := &deviceConn{}
	d.hc = redfish.NewHTTPClient(redfish.TransportOptions{
		Legacy:        legacy,
		OnCertificate: func(fp string) { d.fingerprint.CompareAndSwap(nil, fp) },
	})
	if record {
		d.rec = httprec.NewRecorder(d.hc.Transport)
		d.hc.Transport = d.rec
	}
	return d
}

// finish 打印证书指纹，需要时保存诊断记录。
func (d *deviceConn) finish(path, note string, stdout, stderr io.Writer) {
	if fp, _ := d.fingerprint.Load().(string); fp != "" {
		fmt.Fprintf(stdout, "\n证书指纹（SHA-256）：%s\n", fp)
	}
	if d.rec == nil || path == "" {
		return
	}
	cas := d.rec.Cassette()
	cas.Note = note
	cas.Tool = buildinfo.Get().String()
	cas.RecordedAt = time.Now().Format(time.RFC3339)
	if err := cas.Save(path); err != nil {
		fmt.Fprintf(stderr, "保存诊断记录失败：%v\n", err)
		return
	}
	fmt.Fprintf(stdout, "\n诊断记录已保存到 %s（%d 次请求）。\n"+
		"记录里已去掉密码、会话令牌和设备 IP，但仍有型号、序列号、主机名等设备信息，发送前请先看一遍。\n",
		path, len(cas.Interactions))
}

// askPassword 优先读环境变量 LOGDIAG_PASSWORD，否则提示输入（终端里不回显）。
func askPassword(user string, stdin io.Reader, stderr io.Writer) (string, error) {
	return askSecret(user+" 的密码：", "LOGDIAG_PASSWORD", stdin, stderr)
}

// askSecret 优先读环境变量 env，否则显示 prompt 提示输入（终端里不回显）。
func askSecret(prompt, env string, stdin io.Reader, stderr io.Writer) (string, error) {
	if p := os.Getenv(env); p != "" {
		return p, nil
	}
	fmt.Fprint(stderr, prompt)
	defer fmt.Fprintln(stderr)
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		return string(b), err
	}
	return readLine(stdin)
}

// readLine 从不是终端的输入（管道、文件）读一行。逐字节读、不多读，后面还能接着读下一个密码。
func readLine(r io.Reader) (string, error) {
	var line []byte
	var c [1]byte
	for {
		n, err := r.Read(c[:])
		if n == 1 && c[0] == '\n' {
			break
		}
		line = append(line, c[:n]...)
		if err != nil {
			if len(line) == 0 {
				return "", err
			}
			break
		}
	}
	return strings.TrimRight(string(line), "\r"), nil
}
