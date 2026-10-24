// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"errors"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	box, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"s3cret!", "密码 with spaces", strings.Repeat("x", 300)} {
		sealed, err := box.Seal(plain)
		if err != nil {
			t.Fatal(err)
		}
		if sealed == "" || strings.Contains(sealed, plain) {
			t.Fatalf("Seal(%q) = %q，应是密文", plain, sealed)
		}
		got, err := box.Open(sealed)
		if err != nil || got != plain {
			t.Fatalf("Open = %q, %v，期望 %q", got, err, plain)
		}
	}
	if s, err := box.Seal(""); s != "" || err != nil {
		t.Errorf("空密码应保持为空，得到 %q, %v", s, err)
	}
	if p, err := box.Open(""); p != "" || err != nil {
		t.Errorf("空密文应解出空密码，得到 %q, %v", p, err)
	}
}

func TestOpenForeign(t *testing.T) {
	box, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := other.Seal("s3cret!")
	for _, s := range []string{"garbage", "dpapi:!!!", "aesgcm:AAAA", "unknown:AAAA"} {
		if _, err := box.Open(s); !errors.Is(err, ErrOtherMachine) {
			t.Errorf("Open(%q) 应返回 ErrOtherMachine，得到 %v", s, err)
		}
	}
	// Windows 上两个 Box 是同一个用户的 DPAPI，能互相解密；其他平台各用各的密钥。
	if _, err := box.Open(sealed); err != nil && !errors.Is(err, ErrOtherMachine) {
		t.Errorf("意外的错误：%v", err)
	}
}
