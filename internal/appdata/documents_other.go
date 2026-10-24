// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package appdata

import (
	"os"
	"path/filepath"
)

// documentsDir 返回 ~/Documents（非 Windows 平台只用于开发调试）。
func documentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Documents")
}
