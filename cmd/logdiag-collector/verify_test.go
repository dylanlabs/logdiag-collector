// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerify(t *testing.T) {
	const dir = "../../docs/schema/examples/packages/"
	if code, out, _ := runCmd(t, "verify", dir+"ok.zip"); code != 0 || !strings.Contains(out, "没有问题：3 台设备，客户“示例客户”") {
		t.Errorf("ok.zip: code=%d out=%q", code, out)
	}
	code, out, _ := runCmd(t, "verify", dir+"bad-files.zip", "--json")
	var v struct {
		Result   string
		Problems []struct{ Code, Path, Message string }
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 1 || v.Result != "problems" || len(v.Problems) != 3 || v.Problems[0].Code != "checksum_mismatch" {
		t.Errorf("bad-files.zip: code=%d out=%s err=%v", code, out, err)
	}
	if code, out, _ := runCmd(t, "verify", "-json", dir+"not-a-package.zip"); code != 1 || !strings.Contains(out, `"result": "not_package"`) {
		t.Errorf("not-a-package.zip: code=%d out=%q", code, out)
	}
	if code, out, _ := runCmd(t, "verify", dir+"not-a-zip.zip"); code != 1 || !strings.Contains(out, "不是完整的 zip") {
		t.Errorf("not-a-zip.zip: code=%d out=%q", code, out)
	}
	for _, args := range [][]string{{"verify"}, {"verify", "a.zip", "b.zip"}} {
		if code, _, errOut := runCmd(t, args...); code != 2 || !strings.Contains(errOut, "请指定一个离线上传包") {
			t.Errorf("%v: code=%d err=%q", args, code, errOut)
		}
	}
	if got := consoleNeedOf([]string{"verify", "a.zip"}); got != consoleOutput {
		t.Errorf("verify 的控制台需要：%d", got)
	}
}
