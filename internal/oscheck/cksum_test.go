// SPDX-License-Identifier: GPL-3.0-or-later

package oscheck

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// 用系统自带的 cksum 命令核对实现是否一致（CI 的 Linux runner 上有 cksum）。
func TestCksumMatchesCommand(t *testing.T) {
	if _, err := exec.LookPath("cksum"); err != nil {
		t.Skip("没有 cksum 命令")
	}
	for _, data := range [][]byte{
		nil,
		[]byte("a"),
		[]byte("hello, 世界\n"),
		[]byte(strings.Repeat("LogDiag", 1000)),
	} {
		cmd := exec.Command("cksum")
		cmd.Stdin = strings.NewReader(string(data))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		want, err := strconv.ParseUint(strings.Fields(string(out))[0], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		if got := cksum(data); uint64(got) != want {
			t.Errorf("cksum(%d 字节) = %d，cksum 命令给出 %d", len(data), got, want)
		}
	}
}
