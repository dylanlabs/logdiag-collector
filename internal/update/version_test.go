// SPDX-License-Identifier: GPL-3.0-or-later

package update

import (
	"slices"
	"testing"
)

func TestVersionOrder(t *testing.T) {
	// 从旧到新，例子来自 semver.org 第 11 条。
	order := []string{
		"0.0.0-dev.g1a2b3c4", "0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta", "0.1.0-beta.2",
		"0.1.0-beta.11", "0.1.0-rc.1", "v0.1.0", "0.1.1", "0.2.0", "0.10.0", "1.0.0",
	}
	var vs []Version
	for _, s := range order {
		v, ok := ParseVersion(s)
		if !ok {
			t.Fatalf("%q 应能解析", s)
		}
		vs = append(vs, v)
	}
	for i := range vs {
		for j := range vs {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := vs[i].Compare(vs[j]); got != want {
				t.Errorf("Compare(%s, %s) = %d，应为 %d", order[i], order[j], got, want)
			}
		}
	}
	if v, _ := ParseVersion("v1.2.3-rc.1+build.5"); v.String() != "1.2.3-rc.1" || v.Dev() {
		t.Errorf("构建信息应忽略：%s", v)
	}
	if v, _ := ParseVersion("0.0.0-dev"); !v.Dev() || !slices.Equal(v.Pre, []string{"dev"}) {
		t.Errorf("开发版本：%+v", v)
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "v", "1.2.3-", "1.2.3-a..b", "latest", "1.2.x"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("%q 不应能解析", bad)
		}
	}
}
