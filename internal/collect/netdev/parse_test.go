// SPDX-License-Identifier: GPL-3.0-or-later

package netdev

import (
	"strings"
	"testing"
)

func TestCleaner(t *testing.T) {
	cases := []struct {
		name    string
		chunks  []string
		lines   []string
		partial string
	}{
		{"换行", []string{"abc\r\ndef\n\rghi\r\n"}, []string{"abc", "def", "ghi"}, ""},
		{"提示符", []string{"x\r\nR1#"}, []string{"x"}, "R1#"},
		{"Cisco 分页提示用退格擦掉", []string{"line1\r\n --More-- ", strings.Repeat("\b", 10) + strings.Repeat(" ", 10) + strings.Repeat("\b", 10) + "next\r\n"}, []string{"line1", "next"}, ""},
		{"华为分页提示用光标左移擦掉，控制序列跨块", []string{"  ---- More ----\x1b[1", "6D                \x1b[16", "Dline2\r\n"}, []string{"line2"}, ""},
		{"ASA 分页提示用回车擦掉", []string{"<--- More --->", "\r              \rline3\r\n"}, []string{"line3"}, ""},
		{"清除到行尾", []string{"abcdef\r\x1b[Kxyz\n"}, []string{"xyz"}, ""},
		{"退格覆盖", []string{"ab\bc\n"}, []string{"ac"}, ""},
		{"标题和响铃", []string{"\x1b]0;title\x07te\x07xt\n"}, []string{"text"}, ""},
		{"行尾空白", []string{"a  \t\r\n"}, []string{"a"}, ""},
		{"光标右移补空格", []string{"a\x1b[3Cb\n"}, []string{"a   b"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cl cleaner
			var got []string
			for _, ch := range c.chunks {
				cl.write([]byte(ch), func(l []byte) error { got = append(got, string(l)); return nil })
			}
			if strings.Join(got, "|") != strings.Join(c.lines, "|") || cl.partial() != c.partial {
				t.Errorf("行 = %q，末行 = %q；应为 %q，%q", got, cl.partial(), c.lines, c.partial)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	cases := []struct{ in, want string }{
		{"enable secret 9 $9$abc$def", "enable secret 9 ******"},
		{"username admin privilege 15 secret 9 $9$abc$def", "username admin privilege 15 secret 9 ******"},
		{"username backup password 0 my secret phrase", "username backup password 0 ******"},
		{" password 7 0822455D0A16", " password 7 ******"},
		{"enable password level 15 7 0822455D0A16", "enable password level 15 7 ******"},
		{"tacacs-server host 10.0.0.1 key 7 0822455D0A16", "tacacs-server host 10.0.0.1 key 7 ******"},
		{"tacacs-server key 7 \"abc def\"", "tacacs-server key 7 ******"},
		{" ip ospf message-digest-key 1 md5 7 0822455D0A16", " ip ospf message-digest-key 1 md5 7 ******"},
		{"ntp authentication-key 1 md5 ntpkey 7", "ntp authentication-key 1 md5 ****** 7"},
		{"crypto isakmp key psk123 address 198.51.100.7", "crypto isakmp key ****** address 198.51.100.7"},
		{"snmp-server community public RO", "snmp-server community ****** RO"},
		{"snmp-server host 10.0.0.50 version 2c trapcomm", "snmp-server host 10.0.0.50 version 2c ******"},
		{"snmp-server host nms.example.com trapcomm", "snmp-server host nms.example.com ******"},
		{"snmp-server host inside 10.0.0.50 community asacomm version 2c", "snmp-server host inside 10.0.0.50 community ****** version 2c"},
		{"snmp-server user admin network-admin auth md5 0x1234 priv 0x5678 localizedkey", "snmp-server user admin network-admin auth md5 ****** priv ****** localizedkey"},
		{" local-user admin password irreversible-cipher $1c$abc$def$", " local-user admin password irreversible-cipher ******"},
		{"snmp-agent community read cipher %^%#abc def%^%#", "snmp-agent community read cipher ******"},
		{"snmp-agent community read simple public", "snmp-agent community read simple ******"},
		{"snmp-agent target-host trap address udp-domain 10.0.0.50 params securityname cipher %^%#x%^%# v2c", "snmp-agent target-host trap address udp-domain 10.0.0.50 params securityname cipher ****** v2c"},
		{" authentication-mode md5 1 cipher %^%#x%^%#", " authentication-mode md5 ****** cipher ******"},
		{" key authentication cipher $c$3$abc==", " key authentication cipher ******"},
		{" password hash $h$6$abc$def==", " password hash ******"},
		{"super password role network-admin hash $h$6$abc", "super password role network-admin hash ******"},
		{"snmp-agent usm-user v3 u g simple authentication-mode sha authpass privacy-mode aes128 privpass", "snmp-agent usm-user v3 u g simple authentication-mode sha ****** privacy-mode aes128 ******"},
		{"some text %^%#abc%^%# more", "some text ****** more"},
		// 不是口令的行保持原样。
		{"service password-encryption", "service password-encryption"},
		{"The password-recovery mechanism is enabled.", "The password-recovery mechanism is enabled."},
		{" authentication-mode aaa", " authentication-mode aaa"},
		{"snmp-server enable traps", "snmp-server enable traps"},
		{"interface GigabitEthernet1/0/1", "interface GigabitEthernet1/0/1"},
		{"Password:", "Password:"},
	}
	var r redactor
	for _, c := range cases {
		if got := string(r.line([]byte(c.in))); got != c.want {
			t.Errorf("%q\n脱敏后 %q\n应为   %q", c.in, got, c.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		name string
		fam  family
		text string
		prof *profile
		ver  string
		mod  string
	}{
		{"IOS 12.2", famCisco, "Cisco Internetwork Operating System Software\nIOS (tm) C2950 Software (C2950-I6Q4L2-M), Version 12.1(22)EA14, RELEASE SOFTWARE (fc1)\nSW uptime is 1 week\ncisco WS-C2950T-24 (RC32300) processor (revision R0) with 19912K bytes of memory.\n", profIOS, "12.1(22)EA14", "WS-C2950T-24"},
		{"IOS XE 3.x", famCisco, "Cisco IOS Software, IOS-XE Software, Catalyst 4500 L3 Switch Software (cat4500e-UNIVERSALK9-M), Version 03.06.07.E RELEASE SOFTWARE (fc3)\n", profIOSXE, "03.06.07.E", ""},
		{"华为 VRP5", famVRP, "Huawei Versatile Routing Platform Software\nVRP (R) software, Version 5.170 (S5720 V200R011C10SPC500)\nCopyright (C) 2000-2018 HUAWEI TECH CO., LTD\nHUAWEI S5720-28X-SI-AC Routing Switch uptime is 0 week, 1 day\n", profVRP, "V200R011C10SPC500", "S5720-28X-SI-AC"},
		{"新华三 Comware 5", famVRP, "H3C Comware Platform Software\nComware Software, Version 5.20.99, Release 1106\nCopyright (c) 2004-2014 Hangzhou H3C Tech. Co., Ltd. All rights reserved.\nH3C S5120-28P-SI uptime is 1 week, 0 days, 2 hours, 30 minutes\n", profComware, "5.20.99 Release 1106", "S5120-28P-SI"},
		{"认不出", famCisco, "Linux version 5.15\n", nil, "", ""},
	}
	for _, c := range cases {
		x := parseVersion(c.fam, c.text)
		if x.prof != c.prof || x.version != c.ver || x.model != c.mod {
			t.Errorf("%s：系统 %v，版本 %q，型号 %q", c.name, x.prof, x.version, x.model)
		}
	}
	hpe := "HPE Comware Software, Version 7.1.045, Release 2432P01\nHPE 5130-24G-4SFP+ EI Switch uptime is 1 week\n"
	if v := vendorOf(profComware, hpe); v != "HPE" {
		t.Errorf("HPE Comware 的厂商 = %q", v)
	}
}

func TestHostnameAndFamily(t *testing.T) {
	cases := []struct {
		prompt, host string
		fam          family
	}{
		{"R1#", "R1", famCisco},
		{"R1>", "R1", famCisco},
		{"asa/pri/act#", "asa", famCisco},
		{"<HUAWEI>", "HUAWEI", famVRP},
		{"[~HUAWEI]", "HUAWEI", famVRP},
		{"root@host:~#", "", famUnknown},
		{"[root@host ~]#", "", famUnknown},
	}
	for _, c := range cases {
		if f := familyOf(c.prompt); f != c.fam {
			t.Errorf("familyOf(%q) = %v", c.prompt, f)
		}
		if c.fam != famUnknown {
			if h := hostnameOf(c.prompt); h != c.host {
				t.Errorf("hostnameOf(%q) = %q", c.prompt, h)
			}
		}
	}
}
