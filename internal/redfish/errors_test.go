// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"fmt"
	"net"
	"testing"

	"github.com/dylanlabs/logdiag-collector/internal/sshx"
)

func TestKindOfSSH(t *testing.T) {
	for _, c := range []struct {
		err  error
		want Kind
	}{
		{fmt.Errorf("登录：%w", fmt.Errorf("%w（服务器拒绝了这个账号或密码）", sshx.ErrAuth)), KindAuth},
		{fmt.Errorf("连接：%w", &sshx.HostKeyError{Want: "SHA256:a", Got: "SHA256:b"}), KindHostKey},
		{fmt.Errorf("%w，只允许密钥等其他方式登录", sshx.ErrNoPassword), KindUnsupported},
		{fmt.Errorf("%w：no common algorithm", sshx.ErrAlgorithms), KindUnsupported},
		{fmt.Errorf("连接 SSH 端口：%w", &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")}), KindNetwork},
	} {
		if got := KindOf(c.err); got != c.want {
			t.Errorf("KindOf(%v) = %s，应为 %s", c.err, got, c.want)
		}
	}
}
