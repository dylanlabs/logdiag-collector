// SPDX-License-Identifier: GPL-3.0-or-later

package ui

// windowSize 按屏幕可用区域（不含任务栏，单位是 Windows 缩放后的逻辑像素）算界面窗口大小：
// 横向的长方形，占可用区域的九成左右，最大 1440×900，高度不超过宽度的 0.625（16:10）。
func windowSize(areaW, areaH int) (w, h int) {
	w = min(1440, areaW*9/10)
	h = min(900, areaH*9/10, w*5/8)
	return w, h
}
