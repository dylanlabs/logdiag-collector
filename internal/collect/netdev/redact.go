// SPDX-License-Identifier: GPL-3.0-or-later

package netdev

import (
	"bytes"
	"regexp"
	"slices"
)

// Mask 替换日志里的口令、密钥和 SNMP 团体名。
const Mask = "******"

// rule 是一条脱敏规则：re 的第 1 个分组（没有分组时是整个匹配）换成 Mask。
type rule struct {
	name string
	re   *regexp.Regexp
}

// 口令、密钥前面可能有的级别、加密类型等修饰词，跳过它们再遮盖后面的值。
// 值一般是一个词；带引号的、华为 %^%#…%^%# 这类密文里可能有空格，整个遮住。
const (
	pwMods  = `(?:(?:level|role)\s+\S+\s+)?(?:(?:cipher|simple|hash|irreversible-cipher|encrypted|\d{1,2})\s+)?`
	keyMods = `(?:(?:\d{1,3}|md5|sha\S*|cipher|simple|hash|encrypted|ascii|hex|authentication|authorization|accounting)\s+)*`
	value   = `(%\^%#.*?%\^%#|%@%@.*?%@%@|%\$%\$.*?%\$%\$|"[^"]*"|\S+)`
)

// rules 按顺序作用在每一行上。设备的诊断信息里有运行配置，口令多半已是密文或被设备隐去，
// 这里再统一遮盖一遍。宁可多遮（例如日志里 password 后面的普通单词），也不漏掉。
var rules = []rule{
	// 明文口令（Cisco 类型 0、华为/新华三 simple）可能带空格，遮到行尾。
	{"password", regexp.MustCompile(`(?i)\b(?:password|secret)\s+(?:(?:level|role)\s+\S+\s+)?(?:0|simple)\s+(\S.*)$`)},
	// enable secret 5 …、username … password 7 …、local-user … password irreversible-cipher …、
	// super password level 15 cipher …、password hash …
	{"password", regexp.MustCompile(`(?i)\b(?:password|passwd|secret)\s+` + pwMods + value)},
	// tacacs-server key 7 …、key-string 7 …、message-digest-key 1 md5 7 …、shared-key cipher …、
	// authentication-mode md5 1 cipher …、新华三 key authentication cipher …
	{"auth-key", regexp.MustCompile(`(?i)\b(?:key|key-string|shared-key|authentication-key|message-digest-key|pre-shared-key|md5|wpa-psk|psk)\s+` + keyMods + value)},
	// SNMPv3 用户：NX-OS auth md5 0x… priv 0x…；华为、新华三 authentication-mode sha … privacy-mode aes128 …
	{"auth-key", regexp.MustCompile(`(?i)\b(?:auth\s+(?:md5|sha\S*)|priv(?:\s+(?:aes-128|aes-256|aes|des|3des)(?:\s+\d+)?)?)\s+` + value)},
	{"auth-key", regexp.MustCompile(`(?i)\b(?:authentication-mode|privacy-mode)\s+\S+\s+(?:(?:cipher|simple)\s+)?` + value)},
	{"snmp-community", regexp.MustCompile(`(?i)\bsnmp\S*\s.*?\bcommunity\s+(?:(?:read|write|simple|cipher|\d)\s+)*` + value)},
	// snmp-server host 10.0.0.1 [traps] [version 2c] 团体名。主机是 IP 或带点的主机名；
	// ASA 的 snmp-server host inside 10.0.0.1 community … 由上一条处理。
	{"snmp-community", regexp.MustCompile(`(?i)\bsnmp-server\s+host\s+(?:[0-9a-f:]*[.:][0-9a-f.:]*|[\w-]+(?:\.[\w-]+)+)(?:\s+(?:informs|traps|vrf\s+\S+|version\s+(?:1|2c|3\s+\S+)))*\s+` + value)},
	// 华为、新华三 snmp-agent target-host … params securityname 团体名
	{"snmp-community", regexp.MustCompile(`(?i)\bsecurityname\s+(?:(?:cipher|simple)\s+)?` + value)},
	{"ciphertext", regexp.MustCompile(`(?i)\b(?:cipher|irreversible-cipher)\s+` + value)},
	// 华为 %^%#…%^%#、%@%@…%@%@、%$%$…%$%$，新华三 $c$3$…、$h$6$…，Cisco/NX-OS $1$…、$5$…、$8$…、$9$…
	{"ciphertext", regexp.MustCompile(`%\^%#.*?%\^%#|%@%@.*?%@%@|%\$%\$.*?%\$%\$|@%@%.*?@%@%|\$(?:[ch]\$\d+|\d{1,2}[a-z]?)\$\S+`)},
}

// RedactionRules 是 manifest 的 redaction.rules：收集网络设备日志时用的全部规则名。
var RedactionRules = func() []string {
	var names []string
	for _, r := range rules {
		if !slices.Contains(names, r.name) {
			names = append(names, r.name)
		}
	}
	return names
}()

// needles 是可能需要脱敏的行里一定有的片段（小写）。绝大多数行一个都没有，不必跑正则。
var needles = [][]byte{
	[]byte("pass"), []byte("secret"), []byte("key"), []byte("md5"), []byte("psk"), []byte("auth"), []byte("priv"),
	[]byte("communit"), []byte("snmp"), []byte("securityname"), []byte("cipher"), []byte("$"), []byte("%^%"), []byte("%@%"), []byte("%$%"), []byte("@%@"),
}

// redactor 对一行文本脱敏，复用小写缓冲区。不能被多个 goroutine 同时使用。
type redactor struct {
	lower []byte
}

// line 返回脱敏后的行。没有要遮盖的内容时原样返回 l（不复制）。
func (r *redactor) line(l []byte) []byte {
	r.lower = r.lower[:0]
	for _, b := range l {
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		r.lower = append(r.lower, b)
	}
	if !slices.ContainsFunc(needles, func(n []byte) bool { return bytes.Contains(r.lower, n) }) {
		return l
	}
	for _, ru := range rules {
		l = mask(ru.re, l)
	}
	return l
}

// text 对多行文本逐行脱敏（诊断记录里的原始输出，行尾可能带 \r）。
func (r *redactor) text(s string) string {
	lines := bytes.SplitAfter([]byte(s), []byte("\n"))
	var out []byte
	for _, l := range lines {
		body := bytes.TrimRight(l, "\r\n")
		out = append(out, r.line(body)...)
		out = append(out, l[len(body):]...)
	}
	return string(out)
}

func mask(re *regexp.Regexp, l []byte) []byte {
	locs := re.FindAllSubmatchIndex(l, -1)
	if locs == nil {
		return l
	}
	out := make([]byte, 0, len(l))
	last := 0
	for _, loc := range locs {
		s, e := loc[0], loc[1]
		if len(loc) >= 4 && loc[2] >= 0 {
			s, e = loc[2], loc[3]
		}
		if s < last {
			continue
		}
		out = append(out, l[last:s]...)
		out = append(out, Mask...)
		last = e
	}
	return append(out, l[last:]...)
}
