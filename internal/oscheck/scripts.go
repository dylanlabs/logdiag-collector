// SPDX-License-Identifier: GPL-3.0-or-later

package oscheck

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"slices"
)

// ScriptSource 是巡检脚本的出处：与 LogDiag 网站提供下载的是同一份。
const ScriptSource = "https://logdiag.com/tools/os-inspection/"

// Script 是一套巡检脚本和它的执行方式。
type Script struct {
	OS      string // 系统，对应 uname -s 识别的结果：linux
	Name    string // 文件名，例如 linux-pm.sh
	Title   string // 给用户看的系统名
	Shell   string // 解释器，例如 bash
	LogType string // manifest 的 log.type，例如 logdiag.linux.pm
	// OutDir 是脚本放结果文件的目录，OutPattern 是结果文件名的通配符（find -name 的写法）。
	OutDir     string
	OutPattern string
	Content    []byte
}

// SHA256 返回脚本内容的 SHA-256，界面和命令行显示它，便于和网站上下载的脚本核对。
func (s Script) SHA256() string {
	sum := sha256.Sum256(s.Content)
	return hex.EncodeToString(sum[:])
}

//go:embed scripts
var scriptFiles embed.FS

// catalog 是计划支持的系统。Linux 已内置；其余等有测试环境后再加（1.0），Windows 走 WinRM（1.1）。
var catalog = []Script{
	{OS: "linux", Name: "linux-pm.sh", Title: "Linux", Shell: "bash", LogType: "logdiag.linux.pm",
		OutDir: "/tmp", OutPattern: "*.pm-*.tar.gz"},
}

// Scripts 返回内置的巡检脚本。
func Scripts() []Script {
	var out []Script
	for _, s := range catalog {
		if data, err := scriptFiles.ReadFile("scripts/" + s.Name); err == nil {
			s.Content = data
			out = append(out, s)
		}
	}
	return out
}

// ScriptFor 返回某个系统的内置脚本。
func ScriptFor(os string) (Script, bool) {
	all := Scripts()
	i := slices.IndexFunc(all, func(s Script) bool { return s.OS == os })
	if i < 0 {
		return Script{}, false
	}
	return all[i], true
}
