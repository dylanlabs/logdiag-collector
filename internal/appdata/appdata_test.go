// SPDX-License-Identifier: GPL-3.0-or-later

package appdata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettings(t *testing.T) {
	dir := t.TempDir()
	s, warn, err := OpenSettings(dir)
	if err != nil || warn != "" {
		t.Fatalf("OpenSettings: %v %q", err, warn)
	}
	if got := s.Get(); got != Defaults() {
		t.Errorf("没有设置文件时应为默认值：%+v", got)
	}

	v := s.Get()
	v.Concurrency = 4
	v.OutputDir = filepath.Join(dir, "out")
	if _, err := s.Set(v); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(*Settings){
		func(x *Settings) { x.Concurrency = 0 },
		func(x *Settings) { x.TimeoutMinutes = 999 },
		func(x *Settings) { x.OutputDir = "relative" },
	} {
		w := v
		bad(&w)
		if _, err := s.Set(w); err == nil {
			t.Errorf("无效设置应拒绝：%+v", w)
		}
	}
	if s.Get().Concurrency != 4 {
		t.Error("拒绝的设置不应生效")
	}

	again, _, err := OpenSettings(dir)
	if err != nil || again.Get().Concurrency != 4 || again.Get().OutputDir != v.OutputDir {
		t.Errorf("重新打开应读到保存的设置：%+v %v", again.Get(), err)
	}
}

func TestCorruptSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{坏了"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, warn, err := OpenSettings(dir)
	if err != nil || !strings.Contains(warn, "已损坏") || s.Get() != Defaults() {
		t.Fatalf("损坏的设置文件应备份并改用默认值：%v %q", err, warn)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "settings.json.bad-*"))
	if len(backups) != 1 {
		t.Errorf("应留下一份备份：%v", backups)
	}
}
