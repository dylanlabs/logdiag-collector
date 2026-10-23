// SPDX-License-Identifier: GPL-3.0-or-later

// Package buildinfo 提供产品名、版本号和构建来源。
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// ProductName 是界面、版本资源和 User-Agent 里使用的产品名。
const ProductName = "LogDiag Collector"

// Version 由构建脚本通过 -ldflags "-X .../buildinfo.Version=0.1.0" 写入。
var Version = "0.0.0-dev"

// Info 描述当前可执行文件。
type Info struct {
	Product    string `json:"product"`
	Version    string `json:"version"`
	Commit     string `json:"commit,omitempty"`
	CommitTime string `json:"commitTime,omitempty"`
	Modified   bool   `json:"modified,omitempty"`
	GoVersion  string `json:"go"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

// Get 读取版本号和 Go 工具链写入的 VCS 信息。
func Get() Info {
	info := Info{
		Product:   ProductName,
		Version:   Version,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
			case "vcs.time":
				info.CommitTime = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	return info
}

// String 返回一行版本说明，例如 "LogDiag Collector 0.1.0 (1a2b3c4, go1.27.3 windows/amd64)"。
func (i Info) String() string {
	commit := "unknown"
	if i.Commit != "" {
		commit = i.Commit
		if len(commit) > 7 {
			commit = commit[:7]
		}
		if i.Modified {
			commit += "-dirty"
		}
	}
	return fmt.Sprintf("%s %s (%s, %s %s/%s)", i.Product, i.Version, commit, i.GoVersion, i.OS, i.Arch)
}

// UserAgent 是访问设备和 LogDiag 网站时使用的 User-Agent。
func UserAgent() string {
	return "LogDiagCollector/" + Version
}
