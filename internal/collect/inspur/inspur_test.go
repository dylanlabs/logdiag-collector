// SPDX-License-Identifier: GPL-3.0-or-later

package inspur

import "testing"

func TestFirmwareAtLeast(t *testing.T) {
	for v, want := range map[string]bool{
		"4.35.0":           true,
		"4.34.00":          true,
		"V4.34":            true,
		"4.33.9":           false,
		"3.99":             false,
		"5.01":             true,
		"":                 false,
		"unknown":          false,
		"4.40.0 (2022-01)": true,
	} {
		if got := firmwareAtLeast(v, 4, 34); got != want {
			t.Errorf("firmwareAtLeast(%q) = %v", v, got)
		}
	}
}

func TestOnlyPath(t *testing.T) {
	for in, want := range map[string]string{
		"/tmp/onekeylog.tar":                 "/tmp/onekeylog.tar",
		"tmp/onekeylog.tar":                  "/tmp/onekeylog.tar",
		"https://evil.example/tmp/x.tar?a=1": "/tmp/x.tar?a=1",
		"/api/logs/onekeylog/logfile":        "/api/logs/onekeylog/logfile",
	} {
		if got := onlyPath(in); got != want {
			t.Errorf("onlyPath(%q) = %q，应为 %q", in, got, want)
		}
	}
}
