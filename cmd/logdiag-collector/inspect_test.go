// SPDX-License-Identifier: GPL-3.0-or-later

package main

import "strings"
import "testing"

func TestInspectShowScript(t *testing.T) {
	code, out, _ := runCmd(t, "inspect", "--show-script")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"#!/bin/bash", "SHA-256：0425be63", "logdiag.com", "linux-pm.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("--show-script 输出里没有 %q", want)
		}
	}
}

func TestInspectRequiresHostAndUser(t *testing.T) {
	if code, _, errOut := runCmd(t, "inspect", "--host", "10.0.0.10"); code != 2 || !strings.Contains(errOut, "--user") {
		t.Errorf("缺 user：code=%d err=%q", code, errOut)
	}
	if code, _, errOut := runCmd(t, "inspect", "--host", "10.0.0.10", "--user", "root", "--os", "plan9"); code != 2 || !strings.Contains(errOut, "不支持的系统") {
		t.Errorf("错误的系统：code=%d err=%q", code, errOut)
	}
}
