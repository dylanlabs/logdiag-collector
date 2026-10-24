// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import "testing"

func TestWindowSize(t *testing.T) {
	for _, c := range []struct{ areaW, areaH, w, h int }{
		{1920, 1032, 1440, 900}, // 1080p，100% 缩放
		{1536, 816, 1382, 734},  // 1080p，125% 缩放（常见的笔记本）
		{1280, 672, 1152, 604},  // 1080p，150% 缩放
		{1366, 728, 1229, 655},  // 1366×768
		{2560, 1400, 1440, 900}, // 大屏幕不超过最大尺寸
	} {
		w, h := windowSize(c.areaW, c.areaH)
		if w != c.w || h != c.h {
			t.Errorf("windowSize(%d, %d) = %d×%d，应为 %d×%d", c.areaW, c.areaH, w, h, c.w, c.h)
		}
		if h > w*5/8 || w > c.areaW || h > c.areaH {
			t.Errorf("windowSize(%d, %d) = %d×%d 比例或大小不对", c.areaW, c.areaH, w, h)
		}
	}
}
