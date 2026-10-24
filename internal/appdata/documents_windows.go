// SPDX-License-Identifier: GPL-3.0-or-later

package appdata

import "golang.org/x/sys/windows"

// documentsDir 返回“文档”文件夹的实际位置（用户可能把它移到了 D 盘或 OneDrive）。
func documentsDir() string {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0)
	if err != nil {
		return ""
	}
	return dir
}
