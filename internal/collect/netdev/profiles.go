// SPDX-License-Identifier: GPL-3.0-or-later

package netdev

import (
	"regexp"
	"strings"
)

// family 是命令行风格：Cisco 的 show、terminal length，华为和新华三的 display、screen-length。
type family int

const (
	famUnknown family = iota
	famCisco
	famVRP // 华为 VRP 和新华三 Comware：尖括号提示符
)

// familyOf 按提示符判断命令行风格。
func familyOf(prompt string) family {
	switch {
	case vrpPrompt.MatchString(prompt):
		return famVRP
	case ciscoPrompt.MatchString(prompt):
		return famCisco
	}
	return famUnknown
}

// versionCommand 是识别系统和型号的命令。
func (f family) versionCommand() string {
	if f == famVRP {
		return "display version"
	}
	return "show version"
}

// pagingCommands 是关闭分页的命令，依次尝试，设备接受一个就停。都不接受时靠自动翻页。
// 华为是 screen-length 0 temporary，新华三是 screen-length disable，先试 SSH 版本串指向的那家。
func (f family) pagingCommands(serverVersion string) []string {
	if f == famCisco {
		return []string{"terminal length 0", "terminal pager 0"} // 后者是 ASA
	}
	if v := strings.ToLower(serverVersion); strings.Contains(v, "comware") || strings.Contains(v, "h3c") {
		return []string{"screen-length disable", "screen-length 0 temporary"}
	}
	return []string{"screen-length 0 temporary", "screen-length disable"}
}

// profile 是一种网络操作系统的收集方法。
type profile struct {
	vendor  string // manifest 的厂商名
	os      string // manifest 的 controller.type
	logType string // manifest 的 log.type
	// diag 是诊断信息命令，answers 是它可能提出的问题。
	diag    string
	answers []answer
	// extras 是附带保存的命令（日志缓冲区），设备不支持时跳过。
	extras []string
	// inventory 是读序列号的命令。
	inventory string
	// enable 表示诊断命令要在特权模式下执行（提示符以 # 结尾）。
	enable bool
}

var (
	// Cisco IOS、IOS XE：show tech-support 需要特权模式，口令默认显示为 <removed>。
	profIOS   = &profile{vendor: "Cisco", os: "IOS", logType: "cisco.ios.showtech", diag: "show tech-support", extras: []string{"show logging"}, inventory: "show inventory", enable: true}
	profIOSXE = &profile{vendor: "Cisco", os: "IOS-XE", logType: "cisco.ios.showtech", diag: "show tech-support", extras: []string{"show logging"}, inventory: "show inventory", enable: true}
	// Nexus 用普通的 show tech-support（LogDiag 按它解析）；MDS 的支持清单要求 details。
	profNXOS = &profile{vendor: "Cisco", os: "NX-OS", logType: "cisco.nxos.showtech", diag: "show tech-support", extras: []string{"show logging logfile"}, inventory: "show inventory"}
	profMDS  = &profile{vendor: "Cisco", os: "NX-OS", logType: "cisco.mds.showtech", diag: "show tech-support details", extras: []string{"show logging logfile"}, inventory: "show inventory"}
	profASA  = &profile{vendor: "Cisco", os: "ASA", logType: "cisco.asa.showtech", diag: "show tech-support", extras: []string{"show logging"}, inventory: "show inventory", enable: true}

	// 华为：部分版本执行前会确认“Continue? [Y/N]”。
	profVRP = &profile{vendor: "Huawei", os: "VRP", logType: "huawei.vrp.diag", diag: "display diagnostic-information",
		answers: []answer{
			{regexp.MustCompile(`(?i)(continue|are you sure)\b.*\[y/n\]\s*:?$`), "y\n"},
		},
		extras: []string{"display logbuffer", "display trapbuffer"}, inventory: "display esn"}
	// 新华三：“Save or display diagnostic information (Y=save, N=display)? [Y/N]:”回答 N，直接显示。
	profComware = &profile{vendor: "H3C", os: "Comware", logType: "h3c.comware.diag", diag: "display diagnostic-information",
		answers: []answer{
			{regexp.MustCompile(`(?i)save or display\b.*\[y/n\]\s*:?$`), "n\n"},
			{regexp.MustCompile(`(?i)(continue|are you sure)\b.*\[y/n\]\s*:?$`), "y\n"},
		},
		extras: []string{"display logbuffer"}, inventory: "display device manuinfo"}
)

// LogTypes 是网络设备的日志类型编码和说明，与网站共用（docs/schema/manifest-1.schema.json）。
var LogTypes = map[string]string{
	"cisco.ios.showtech":  "Cisco IOS / IOS XE show tech-support",
	"cisco.nxos.showtech": "Cisco Nexus show tech-support",
	"cisco.mds.showtech":  "Cisco MDS show tech-support details",
	"cisco.asa.showtech":  "Cisco ASA show tech-support",
	"huawei.vrp.diag":     "华为 display diagnostic-information",
	"h3c.comware.diag":    "新华三 display diagnostic-information",
}

// facts 是从 show version、show inventory 这类命令里读到的信息，读不到的留空。
type facts struct {
	prof     *profile
	version  string
	model    string
	serial   string
	hostname string
}

var (
	reIOSXE      = regexp.MustCompile(`(?i)Cisco IOS[ -]XE Software,? Version\s+([^\s,]+)`)
	reIOSXEOld   = regexp.MustCompile(`(?i)IOS-XE Software.*?Version\s+([^\s,]+)`)
	reIOS        = regexp.MustCompile(`(?i)(?:Cisco IOS Software|IOS \(tm\)).*?,\s*Version\s+([^\s,]+)`)
	reIOSHost    = regexp.MustCompile(`(?m)^(\S+) uptime is `)
	reIOSModel   = regexp.MustCompile(`(?mi)^\s*Model [Nn]umber\s*:\s*(\S+)`)
	reIOSModel2  = regexp.MustCompile(`(?m)^[Cc]isco (\S+) \(.*\) processor`)
	reIOSSerial  = regexp.MustCompile(`(?mi)^\s*System [Ss]erial [Nn]umber\s*:\s*(\S+)`)
	reBoardID    = regexp.MustCompile(`(?mi)^\s*Processor [Bb]oard ID\s+(\S+)`)
	reNXOS       = regexp.MustCompile(`(?i)Cisco Nexus Operating System|NX-OS`)
	reNXVersion  = regexp.MustCompile(`(?mi)^\s*(?:NXOS|system):\s+version\s+(\S+)`)
	reNXChassis  = regexp.MustCompile(`(?mi)^\s*cisco (.+?)\s+[Cc]hassis`)
	reNXHost     = regexp.MustCompile(`(?mi)^\s*Device name:\s*(\S+)`)
	reMDS        = regexp.MustCompile(`(?i)\bcisco MDS\b`)
	reASA        = regexp.MustCompile(`(?i)Adaptive Security Appliance Software Version\s+(\S+)`)
	reASAModel   = regexp.MustCompile(`(?mi)^\s*Hardware:\s*([^,\s]+)`)
	reASASerial  = regexp.MustCompile(`(?mi)^\s*Serial Number:\s*(\S+)`)
	reASAHost    = regexp.MustCompile(`(?m)^(\S+) up \d`)
	reInventory  = regexp.MustCompile(`(?i)PID:\s*([^\s,]*)\s*,\s*VID:[^,]*,\s*SN:\s*([^\s,]*)`)
	reHuawei     = regexp.MustCompile(`(?i)Huawei Versatile Routing Platform|VRP \(R\) software`)
	reVRPVersion = regexp.MustCompile(`(?i)VRP \(R\) software,\s*Version\s+(\S+)(?:\s*\((?:\S+\s+)?(V\d+R\d+\S*)\))?`)
	reVRPModel   = regexp.MustCompile(`(?mi)^\s*(?:HUAWEI|Quidway)\s+(\S+).*?\buptime is`)
	reESN        = regexp.MustCompile(`(?mi)^\s*ESN of (?:slot|master|chassis|device)[^:]*:\s*(\S+)`)
	reESNTable   = regexp.MustCompile(`(?m)^\s*\d+\s+([0-9A-Z]{16,24})\s*$`)
	reComware    = regexp.MustCompile(`(?i)Comware (?:Software|Platform)`)
	reCWVersion  = regexp.MustCompile(`(?i)Comware Software,\s*Version\s+([^\s,]+)(?:,\s*Release\s+(\S+))?`)
	reCWModel    = regexp.MustCompile(`(?mi)^\s*(?:H3C|HPE|HP|UNIS)\s+(\S+).*?\buptime is`)
	reCWVendor   = regexp.MustCompile(`(?mi)^\s*(HPE|HP)\s+Comware`)
	reManuSerial = regexp.MustCompile(`(?mi)^\s*DEVICE_SERIAL_NUMBER\s*:\s*(\S+)`)
)

// parseVersion 从 show version、display version 的输出判断系统，读出版本、型号、序列号和主机名。
// 认不出时 prof 为 nil。
func parseVersion(f family, text string) facts {
	var x facts
	if f == famVRP {
		switch {
		case reComware.MatchString(text):
			x.prof = profComware
			if m := reCWVersion.FindStringSubmatch(text); m != nil {
				x.version = m[1]
				if m[2] != "" {
					x.version += " Release " + m[2]
				}
			}
			x.model = sub(reCWModel, text)
		case reHuawei.MatchString(text):
			x.prof = profVRP
			if m := reVRPVersion.FindStringSubmatch(text); m != nil {
				x.version = m[1]
				if m[2] != "" {
					x.version = m[2] // 产品版本号（V200R011C10SPC500）比平台版本更常用
				}
			}
			x.model = sub(reVRPModel, text)
		}
		return x
	}
	switch {
	case reASA.MatchString(text):
		x.prof = profASA
		x.version = sub(reASA, text)
		x.model = sub(reASAModel, text)
		x.serial = sub(reASASerial, text)
		x.hostname = sub(reASAHost, text)
	case reNXOS.MatchString(text):
		x.prof = profNXOS
		if reMDS.MatchString(text) {
			x.prof = profMDS
		}
		x.version = sub(reNXVersion, text)
		x.model = sub(reNXChassis, text)
		x.serial = sub(reBoardID, text)
		x.hostname = sub(reNXHost, text)
	case reIOSXE.MatchString(text), reIOSXEOld.MatchString(text):
		x.prof = profIOSXE
		x.version = firstNonEmpty(sub(reIOSXE, text), sub(reIOSXEOld, text))
	case reIOS.MatchString(text):
		x.prof = profIOS
		x.version = sub(reIOS, text)
	}
	if x.prof == profIOS || x.prof == profIOSXE {
		x.model = firstNonEmpty(sub(reIOSModel, text), sub(reIOSModel2, text))
		x.serial = firstNonEmpty(sub(reIOSSerial, text), sub(reBoardID, text))
		x.hostname = sub(reIOSHost, text)
	}
	return x
}

// parseInventory 从 show inventory、display esn、display device manuinfo 读出整机的型号和序列号：
// 取第一条（整机或堆叠的 1 号成员）。读不到时为空。
func parseInventory(p *profile, text string) (model, serial string) {
	switch p {
	case profVRP:
		return "", firstNonEmpty(sub(reESN, text), sub(reESNTable, text))
	case profComware:
		return "", sub(reManuSerial, text)
	}
	if m := reInventory.FindStringSubmatch(text); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

// vendorOf 返回 manifest 的厂商名。新华三的 Comware 也用在 HPE 的交换机上。
func vendorOf(p *profile, versionText string) string {
	if p == profComware {
		if m := reCWVendor.FindStringSubmatch(versionText); m != nil {
			return "HPE"
		}
	}
	return p.vendor
}

// hostnameOf 从提示符取主机名：R1#、R1>、asa/pri/act#、<HUAWEI>、[H3C]。
func hostnameOf(prompt string) string {
	p := strings.TrimRight(prompt, ">#]")
	p = strings.TrimLeft(p, "<[~*")
	if i := strings.IndexAny(p, "/("); i > 0 {
		p = p[:i]
	}
	return p
}

func sub(re *regexp.Regexp, text string) string {
	if m := re.FindStringSubmatch(text); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
