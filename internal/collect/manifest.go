// SPDX-License-Identifier: GPL-3.0-or-later

package collect

// ManifestSchema 是 manifest 规范版本，与 LogDiag 网站约定，见 docs/schema/manifest-1.schema.json。
const ManifestSchema = "logdiag.manifest/1"

// Manifest 放在每个日志包旁边（manifest.json），LogDiag 据此选择解析器、按客户归档。
type Manifest struct {
	Schema      string         `json:"schema"`
	Tool        Tool           `json:"tool"`
	CollectedAt string         `json:"collected_at"` // RFC 3339，带时区
	Customer    string         `json:"customer"`
	BatchID     string         `json:"batch_id"`
	Device      ManifestDevice `json:"device"`
	Log         Log            `json:"log"`
	Files       []File         `json:"files"`
	Redaction   Redaction      `json:"redaction"`
}

// Tool 是生成文件的工具。
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ManifestDevice 是 manifest 里的设备信息。
type ManifestDevice struct {
	Type       string     `json:"type"`
	Vendor     string     `json:"vendor"`
	Model      string     `json:"model"`
	Serial     string     `json:"serial"`
	Hostname   string     `json:"hostname,omitempty"`
	MgmtIP     string     `json:"mgmt_ip,omitempty"`
	Controller Controller `json:"controller"`
}

// File 是 manifest 列出的一个文件。
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Role   string `json:"role"` // main：主日志包；extra：附带文件
}

// Redaction 说明是否对日志内容做过文本脱敏。服务器日志包按设备原样保存，不做脱敏。
type Redaction struct {
	Applied bool     `json:"applied"`
	Rules   []string `json:"rules,omitempty"`
}
