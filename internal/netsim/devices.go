// SPDX-License-Identifier: GPL-3.0-or-later

package netsim

import (
	"fmt"
	"strings"
	"time"
)

// Device 是一台模拟设备。用 New 按型号取默认值，再按需要修改。
type Device struct {
	// Kind 是模拟的系统：ios、iosxe、nxos、mds、asa、huawei、h3c。
	Kind          string
	Hostname      string
	User          string
	Password      string
	Enable        string // 非空时账号登录后在普通模式（>），要用它进入特权模式
	ServerVersion string

	ShellLogin  bool // SSH 层不认证，进入命令行后再问账号密码（部分老设备）
	AskChange   bool // 登录后问“是否现在修改密码”（华为密码快过期）
	ForceChange bool // 登录后强制修改密码（新华三首次登录）
	DiagConfirm bool // 华为执行诊断信息命令前确认“Continue? [Y/N]”
	KeepPaging  bool // 不接受关闭分页的命令（模拟账号没有这条命令的权限），只能按页翻

	PageLines int           // 每页行数，0 表示 24
	Ports     int           // 诊断信息里接口部分的端口数，0 表示 48；调大可以模拟几十 MB 的输出
	Pause     time.Duration // 诊断信息中途的停顿，0 表示 300 毫秒
}

// Kinds 是可以模拟的系统。
var Kinds = []string{"ios", "iosxe", "nxos", "mds", "asa", "huawei", "h3c"}

// New 返回一种系统的模拟设备，账号 admin，密码 Admin@123（Cisco IOS、ASA 的 enable 密码是 Enable@123）。
func New(kind string) (*Device, error) {
	d := &Device{Kind: kind, User: "admin", Password: "Admin@123"}
	switch kind {
	case "ios":
		d.Hostname, d.Enable, d.ServerVersion = "SW-ACCESS-01", "Enable@123", "SSH-2.0-Cisco-1.25"
	case "iosxe":
		d.Hostname, d.ServerVersion = "SW-CORE-01", "SSH-2.0-Cisco-1.25"
	case "nxos":
		d.Hostname, d.ServerVersion = "N9K-LEAF-01", "SSH-2.0-OpenSSH_8.3"
	case "mds":
		d.Hostname, d.ServerVersion = "MDS-FAB-A", "SSH-2.0-OpenSSH_8.3"
	case "asa":
		d.Hostname, d.Enable, d.ServerVersion = "ASA-EDGE-01", "Enable@123", "SSH-2.0-Cisco-1.25"
	case "huawei":
		d.Hostname, d.ServerVersion, d.AskChange, d.DiagConfirm = "HUAWEI-CE-01", "SSH-2.0-HUAWEI-1.5", true, true
	case "h3c":
		d.Hostname, d.ServerVersion = "H3C-S6850-01", "SSH-2.0-Comware-7.1.064"
	default:
		return nil, fmt.Errorf("不能模拟 %q，可选：%s", kind, strings.Join(Kinds, "、"))
	}
	return d, nil
}

func (d *Device) vrp() bool { return d.Kind == "huawei" || d.Kind == "h3c" }

func (d *Device) ports() int {
	if d.Ports > 0 {
		return d.Ports
	}
	return 48
}

func (d *Device) pause() time.Duration {
	if d.Pause > 0 {
		return d.Pause
	}
	return 300 * time.Millisecond
}

const (
	pagingKeep = iota
	pagingOff
	pagingOn
)

// output 是一条命令的输出。
type output struct {
	text    string
	paging  int    // 命令对分页的影响
	ask     string // 输出前的提问
	want    string // 期望的回答，回答不同时不输出
	pauseAt int    // 在第几行前停顿，0 表示不停
}

func (o output) lines() []string {
	if o.text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(o.text, "\n"), "\n")
}

// more 返回分页提示和按键后擦掉提示的控制序列，和各家设备一样。
func (d *Device) more() (prompt, erase string) {
	switch {
	case d.vrp():
		return "  ---- More ----", "\x1b[16D                \x1b[16D"
	case d.Kind == "asa":
		return "<--- More --->", "\r              \r"
	}
	return " --More-- ", "\b\b\b\b\b\b\b\b\b\b          \b\b\b\b\b\b\b\b\b\b"
}

// invalid 是不认识的命令的报错：先用 ^ 指出位置，再给出各家的提示。
func (d *Device) invalid(line string) string {
	caret := strings.Repeat(" ", len(d.Hostname)+2) + "^"
	switch d.Kind {
	case "huawei":
		return caret + "\nError: Unrecognized command found at '^' position."
	case "h3c":
		return caret + "\n% Unrecognized command found at '^' position."
	case "nxos", "mds":
		return caret + "\n% Invalid command at '^' marker."
	case "asa":
		return caret + "\nERROR: % Invalid input detected at '^' marker."
	}
	return caret + "\n% Invalid input detected at '^' marker."
}

func (d *Device) banner() string {
	switch d.Kind {
	case "huawei":
		return "\nInfo: The max number of VTY users is 10, the number of current VTY users online is 1, and total number of terminal users online is 1.\n      The current login time is 2026-10-26 14:03:11+08:00.\n      The last login time is 2026-10-20 09:12:45+08:00 from 10.20.0.99 through SSH."
	case "h3c":
		return "\n******************************************************************************\n* Copyright (c) 2004-2021 New H3C Technologies Co., Ltd. All rights reserved.*\n* Without the owner's prior written consent,                                 *\n* no decompiling or reverse-engineering shall be allowed.                    *\n******************************************************************************\n"
	case "asa":
		return "Type help or '?' for a list of available commands."
	}
	return "\n##############################################################\n#  Authorized access only. Activity is logged and monitored.  #\n##############################################################\n"
}

// command 返回一条命令的输出；不认识时 ok 为 false。
func (d *Device) command(line string, priv bool) (output, bool) {
	switch d.Kind {
	case "huawei", "h3c":
		return d.vrpCommand(line)
	}
	return d.ciscoCommand(line, priv)
}

func (d *Device) ciscoCommand(line string, priv bool) (output, bool) {
	switch {
	case d.KeepPaging && strings.HasPrefix(line, "terminal "):
		return output{}, false
	case line == "terminal length 0" && d.Kind != "asa":
		return output{paging: pagingOff}, true
	case line == "terminal pager 0" && d.Kind == "asa":
		return output{paging: pagingOff}, true
	case line == "show version":
		return output{text: d.ciscoVersion()}, true
	case line == "show inventory":
		return output{text: d.ciscoInventory()}, true
	case line == "show logging" && (d.Kind == "ios" || d.Kind == "iosxe" || d.Kind == "asa"):
		return output{text: d.ciscoLogging()}, true
	case line == "show logging logfile" && (d.Kind == "nxos" || d.Kind == "mds"):
		return output{text: d.nxosLogfile()}, true
	case !priv:
		return output{}, false // 普通模式下 show tech-support 不可用，和 IOS 一样报“Invalid input”
	case line == "show tech-support" && d.Kind != "mds", line == "show tech-support details" && d.Kind == "mds":
		text := d.ciscoTech()
		return output{text: text, pauseAt: strings.Count(text, "\n") / 2}, true
	}
	return output{}, false
}

func (d *Device) vrpCommand(line string) (output, bool) {
	switch {
	case d.KeepPaging && strings.HasPrefix(line, "screen-length "):
		return output{}, false
	case line == "screen-length 0 temporary" && d.Kind == "huawei":
		return output{text: "Info: The configuration takes effect on the current user terminal interface only.", paging: pagingOff}, true
	case line == "screen-length disable" && d.Kind == "h3c":
		return output{paging: pagingOff}, true
	case line == "display version":
		return output{text: d.vrpVersion()}, true
	case line == "display esn" && d.Kind == "huawei":
		return output{text: "ESN of slot 1: " + d.serial()}, true
	case line == "display device manuinfo" && d.Kind == "h3c":
		return output{text: d.h3cManuinfo()}, true
	case line == "display logbuffer":
		return output{text: d.vrpLogbuffer()}, true
	case line == "display trapbuffer" && d.Kind == "huawei":
		return output{text: "Trapping buffer configuration and contents : enabled\nAllowed max buffer size : 1024\nActual buffer size : 256\nChannel number : 3 , Channel name : trapbuffer\nDropped messages : 0\nOverwritten messages : 0\nCurrent messages : 1\n\nOct 26 2026 13:58:02 " + d.Hostname + " IFNET/2/linkDown_active(t):CID=0x807a0405-alarmID=0x08520003;The interface status changes. (ifName=10GE1/0/48, AdminStatus=UP, OperStatus=DOWN, Reason=The interface is shut down)"}, true
	case line == "display diagnostic-information":
		text := d.vrpDiag()
		o := output{text: text, pauseAt: strings.Count(text, "\n") / 2}
		if d.Kind == "h3c" {
			o.ask, o.want = "Save or display diagnostic information (Y=save, N=display)? [Y/N] :", "n"
		} else if d.DiagConfirm {
			o.ask, o.want = "Info: It will take a long time if the content you search is too much or the string you input is too long, you can press CTRL_C to break.\r\nContinue? [Y/N]:", "y"
		}
		return o, true
	}
	return output{}, false
}

func (d *Device) serial() string {
	return map[string]string{
		"ios":    "FOCSYNTH005",
		"iosxe":  "FOCSYNTH001",
		"nxos":   "FDOSYNTH002",
		"mds":    "JAFSYNTH003",
		"asa":    "JADSYNTH004",
		"huawei": "2102350DLS10SYNTH001",
		"h3c":    "210235A2FSHSYNTH0001",
	}[d.Kind]
}

func (d *Device) ciscoVersion() string {
	h, sn := d.Hostname, d.serial()
	switch d.Kind {
	case "ios":
		return `Cisco IOS Software, C2960X Software (C2960X-UNIVERSALK9-M), Version 15.2(7)E4, RELEASE SOFTWARE (fc2)
Technical Support: http://www.cisco.com/techsupport
Copyright (c) 1986-2021 by Cisco Systems, Inc.
Compiled Tue 16-Mar-21 06:53 by prod_rel_team

ROM: Bootstrap program is C2960X boot loader
BOOTLDR: C2960X Boot Loader (C2960X-HBOOT-M) Version 15.2(7r)E1, RELEASE SOFTWARE (fc1)

` + h + ` uptime is 12 weeks, 3 days, 4 hours, 5 minutes
System returned to ROM by power-on
System image file is "flash:/c2960x-universalk9-mz.152-7.E4/c2960x-universalk9-mz.152-7.E4.bin"

cisco WS-C2960X-48TS-L (APM86XXX) processor (revision B0) with 524288K bytes of memory.
Processor board ID ` + sn + `
Last reset from power-on
1 Virtual Ethernet interface
52 Gigabit Ethernet interfaces
The password-recovery mechanism is enabled.

512K bytes of flash-simulated non-volatile configuration memory.
Base ethernet MAC Address       : 00:11:22:33:44:00
Model revision number           : B0
Model number                    : WS-C2960X-48TS-L
System serial number            : ` + sn + `

Switch Ports Model                     SW Version            SW Image
------ ----- -----                     ----------            ----------
*    1 52    WS-C2960X-48TS-L          15.2(7)E4             C2960X-UNIVERSALK9-M

Configuration register is 0xF`
	case "iosxe":
		return `Cisco IOS XE Software, Version 17.03.04
Cisco IOS Software [Amsterdam], Catalyst L3 Switch Software (CAT9K_IOSXE), Version 17.3.4, RELEASE SOFTWARE (fc3)
Technical Support: http://www.cisco.com/techsupport
Copyright (c) 1986-2021 by Cisco Systems, Inc.
Compiled Sat 30-Jul-21 19:54 by mcpre

ROM: IOS-XE ROMMON
BOOTLDR: System Bootstrap, Version 17.6.1r[FC2], RELEASE SOFTWARE (P)

` + h + ` uptime is 2 weeks, 3 days, 4 hours, 5 minutes
Uptime for this control processor is 2 weeks, 3 days, 4 hours, 7 minutes
System returned to ROM by Reload Command
System image file is "flash:packages.conf"
Last reload reason: Reload Command

cisco C9300-48P (X86) processor with 1392780K/6147K bytes of memory.
Processor board ID ` + sn + `
2048K bytes of non-volatile configuration memory.
8388608K bytes of physical memory.

Base Ethernet MAC Address          : 00:11:22:33:44:01
Motherboard Assembly Number        : 73-17952-06
Model Revision Number              : A0
Model Number                       : C9300-48P
System Serial Number               : ` + sn + `

Switch Ports Model              SW Version        SW Image              Mode
------ ----- -----              ----------        ----------            ----
*    1 64    C9300-48P          17.03.04          CAT9K_IOSXE           INSTALL

Configuration register is 0x102`
	case "nxos":
		return `Cisco Nexus Operating System (NX-OS) Software
TAC support: http://www.cisco.com/tac
Copyright (C) 2002-2021, Cisco and/or its affiliates.
All rights reserved.

Software
  BIOS: version 07.69
 NXOS: version 9.3(8)
  BIOS compile time:  04/08/2021
  NXOS image file is: bootflash:///nxos.9.3.8.bin
  NXOS compile time:  8/18/2021 16:00:00 [08/18/2021 08:31:12]

Hardware
  cisco Nexus9000 C93180YC-EX chassis
  Intel(R) Xeon(R) CPU  @ 1.80GHz with 24569356 kB of memory.
  Processor Board ID FDOSYNTH0SUP

  Device name: ` + h + `
  bootflash:   53298520 kB
Kernel uptime is 45 day(s), 6 hour(s), 7 minute(s), 8 second(s)

Last reset at 123456 usecs after Mon Sep 10 08:00:00 2026
  Reason: Reset Requested by CLI command reload
  System version: 9.3(8)
  Service:`
	case "mds":
		return `Cisco Nexus Operating System (NX-OS) Software
TAC support: http://www.cisco.com/tac
Copyright (c) 2002-2021, Cisco Systems, Inc. All rights reserved.

Software
  BIOS:      version 1.0.19
  loader:    version N/A
  kickstart: version 8.4(2c)
  system:    version 8.4(2c)
  BIOS compile time:       01/08/14
  kickstart image file is: bootflash:///m9100-s6ek9-kickstart-mz.8.4.2c.bin
  system image file is:    bootflash:///m9100-s6ek9-mz.8.4.2c.bin

Hardware
  cisco MDS 9148S 16G 48 FC (1 Slot) Chassis ("2/4/8/16 Gbps FC/Supervisor")
  Motorola, e500v2, core 0 with 4088636 kB of memory.
  Processor Board ID JAFSYNTH0SUP

  Device name: ` + h + `
  bootflash:    4001760 kB
Kernel uptime is 120 day(s), 1 hour(s), 2 minute(s), 3 second(s)`
	case "asa":
		return `
Cisco Adaptive Security Appliance Software Version 9.12(4)37
SSP Operating System Version 2.6(1.224)
Device Manager Version 7.12(2)

Compiled on Wed 21-Oct-20 18:06 GMT by builders
System image file is "disk0:/asa9-12-4-37-lfbff-k8.SPA"
Config file at boot was "startup-config"

` + h + ` up 25 days 3 hours

Hardware:   ASA5516, 8192 MB RAM, CPU Atom C2000 series 2416 MHz, 1 CPU (8 cores)
Internal ATA Compact Flash, 8192MB
BIOS Flash M25P64 @ 0xfed01000, 16384KB

 0: Ext: GigabitEthernet1/1  : address is 0011.2233.4410, irq 255
 1: Ext: GigabitEthernet1/2  : address is 0011.2233.4411, irq 255

Licensed features for this platform:
Maximum Physical Interfaces       : Unlimited      perpetual
Maximum VLANs                     : 150            perpetual

Serial Number: ` + sn + `
Running Permanent Activation Key: 0x00000000 0x00000000 0x00000000 0x00000000 0x00000000
Configuration register is 0x1
Configuration last modified by enable_15 at 10:00:00.000 CST Mon Oct 19 2026`
	}
	return ""
}

func (d *Device) ciscoInventory() string {
	sn := d.serial()
	switch d.Kind {
	case "ios":
		return `NAME: "1", DESCR: "WS-C2960X-48TS-L"
PID: WS-C2960X-48TS-L  , VID: V06  , SN: ` + sn + `

NAME: "Switch 1 - Power Supply 0", DESCR: "FRU Power Supply"
PID: PWR-C2-250WAC     , VID: V02  , SN: LITSYNTH0PS`
	case "iosxe":
		return `NAME: "c93xx Stack", DESCR: "c93xx Stack"
PID: C9300-48P         , VID: V02  , SN: ` + sn + `

NAME: "Switch 1", DESCR: "C9300-48P"
PID: C9300-48P         , VID: V02  , SN: ` + sn + `

NAME: "Switch 1 - Power Supply A", DESCR: "Switch 1 - Power Supply A"
PID: PWR-C1-715WAC     , VID: V02  , SN: DCBSYNTH0PS`
	case "nxos":
		return `NAME: "Chassis",  DESCR: "Nexus9000 C93180YC-EX chassis"
PID: N9K-C93180YC-EX     ,  VID: V03 ,  SN: ` + sn + `

NAME: "Slot 1",  DESCR: "48x10/25G + 6x40/100G Ethernet Module"
PID: N9K-C93180YC-EX     ,  VID: V03 ,  SN: FDOSYNTH0SUP`
	case "mds":
		return `NAME: "Chassis",  DESCR: "MDS 9148S 16G 48 FC (1 Slot) Chassis"
PID: DS-C9148S-K9        ,  VID: V01 ,  SN: ` + sn + `

NAME: "Slot 1",  DESCR: "2/4/8/16 Gbps FC/Supervisor"
PID: DS-C9148S-K9-SUP    ,  VID: V01 ,  SN: JAFSYNTH0SUP`
	case "asa":
		return `Name: "Chassis", DESCR: "ASA 5516-X with FirePOWER services, 8GE, AC, DES"
PID: ASA5516           , VID: V05     , SN: ` + sn + `

Name: "Storage Device 1", DESCR: "ASA 5516-X SSD"
PID: N/A               , VID: N/A     , SN: MSASYNTH0SSD`
	}
	return ""
}

func (d *Device) ciscoLogging() string {
	h := d.Hostname
	if d.Kind == "asa" {
		return `Syslog logging: enabled
    Facility: 20
    Timestamp logging: enabled
    Buffer logging: level informational, 3 messages logged
Oct 26 2026 13:55:01: %ASA-6-302013: Built inbound TCP connection 1001 for outside:203.0.113.10/51515 (203.0.113.10/51515) to inside:10.20.0.5/443 (10.20.0.5/443)
Oct 26 2026 13:57:12: %ASA-4-411002: Line protocol on Interface GigabitEthernet1/3, changed state to down
Oct 26 2026 13:58:40: %ASA-5-111008: User 'admin' executed the 'enable' command.`
	}
	return `Syslog logging: enabled (0 messages dropped, 2 messages rate-limited, 0 flushes, 0 overruns, xml disabled, filtering disabled)

    Console logging: level debugging, 120 messages logged, xml disabled,
                     filtering disabled
    Buffer logging:  level debugging, 120 messages logged, xml disabled,
                    filtering disabled
    Trap logging: level informational, 124 message lines logged
        Logging to 10.20.0.50  (udp port 514, audit disabled,
              link up),
              124 message lines logged,
              0 message lines rate-limited,
              0 message lines dropped-by-MD,
              xml disabled, sequence number disabled
              filtering disabled

Log Buffer (16384 bytes):

Oct 26 13:52:10.123: %LINK-3-UPDOWN: Interface GigabitEthernet1/0/24, changed state to down
Oct 26 13:52:11.130: %LINEPROTO-5-UPDOWN: Line protocol on Interface GigabitEthernet1/0/24, changed state to down
Oct 26 13:57:02.881: %PLATFORM_ENV-1-FAN: Faulty fan detected
Oct 26 13:58:40.002: %SYS-5-CONFIG_I: Configured from console by admin on vty0 (10.20.0.99)
` + h + ` log end`
}

func (d *Device) nxosLogfile() string {
	return `2026 Oct 26 13:50:01 ` + d.Hostname + ` %ETHPORT-5-IF_DOWN_LINK_FAILURE: Interface Ethernet1/12 is down (Link failure)
2026 Oct 26 13:51:44 ` + d.Hostname + ` %PLATFORM-2-FAN_REMOVED: Fan module 3 (Serial number ) Fan3(sys_fan3) removed
2026 Oct 26 13:58:40 ` + d.Hostname + ` %AUTHPRIV-6-SYSTEM_MSG: pam_unix(dcos_sshd:session): session opened for user admin - dcos_sshd[12345]`
}

// ciscoTech 是 show tech-support 的输出：各段命令输出，含运行配置（其中的口令用来测试脱敏）和接口信息。
func (d *Device) ciscoTech() string {
	var b strings.Builder
	sec := func(cmd string) {
		if d.Kind == "nxos" || d.Kind == "mds" {
			fmt.Fprintf(&b, "\n`%s`\n", cmd)
		} else {
			fmt.Fprintf(&b, "\n------------------ %s ------------------\n\n", cmd)
		}
	}
	sec("show clock")
	b.WriteString("*14:03:11.000 CST Mon Oct 26 2026\n")
	sec("show version")
	b.WriteString(d.ciscoVersion() + "\n")
	sec("show running-config")
	b.WriteString(d.ciscoConfig())
	sec("show inventory")
	b.WriteString(d.ciscoInventory() + "\n")
	sec("show environment all")
	b.WriteString("FAN PS-1 is OK\nFAN 1 is OK\nFAN 2 is OK\nFAN 3 is FAULTY\nSYSTEM TEMPERATURE is OK\nSystem Temperature Value: 38 Degree Celsius\nPower Supply A: OK\n")
	sec("show interfaces")
	for i := 1; i <= d.ports(); i++ {
		name := fmt.Sprintf("GigabitEthernet1/0/%d", i)
		if d.Kind == "nxos" || d.Kind == "mds" {
			name = fmt.Sprintf("Ethernet1/%d", i)
		}
		state := "up"
		if i%12 == 0 {
			state = "down"
		}
		fmt.Fprintf(&b, "%s is %s, line protocol is %s (connected)\n", name, state, state)
		fmt.Fprintf(&b, "  Hardware is Gigabit Ethernet, address is 0011.2233.%04x (bia 0011.2233.%04x)\n", i, i)
		b.WriteString("  MTU 1500 bytes, BW 1000000 Kbit/sec, DLY 10 usec,\n     reliability 255/255, txload 1/255, rxload 1/255\n")
		b.WriteString("  Full-duplex, 1000Mb/s, media type is 10/100/1000BaseTX\n")
		fmt.Fprintf(&b, "     %d packets input, %d bytes, 0 no buffer\n", 100000*i, 64000000*i)
		fmt.Fprintf(&b, "     %d input errors, %d CRC, 0 frame, 0 overrun, 0 ignored\n", i%7, i%5)
		fmt.Fprintf(&b, "     %d packets output, %d bytes, 0 underruns\n", 90000*i, 58000000*i)
	}
	sec("show logging")
	b.WriteString(d.ciscoLogging() + "\n")
	// 输出里偶尔有和提示符一样的行（例如 show history），不能当成命令结束。
	sec("show history all")
	b.WriteString(d.Hostname + "#\n  show version\n  show tech-support\n")
	return b.String()
}

// ciscoConfig 是运行配置。口令、密钥、团体名都带 S3cr3t，测试据此检查脱敏有没有漏。
func (d *Device) ciscoConfig() string {
	h := d.Hostname
	switch d.Kind {
	case "nxos", "mds":
		return `
!Command: show running-config
!Running configuration last done at: Mon Oct 19 10:00:00 2026
version 9.3(8) Bios:version 07.69
hostname ` + h + `
feature tacacs+
feature lacp
username admin password 5 $5$S3cr3tSalt$S3cr3tHashValue0123456789abcdef  role network-admin
username monitor password 5 $5$S3cr3tSalt2$S3cr3tHashValue9876543210  role network-operator
tacacs-server key 7 "S3cr3tTacKey"
tacacs-server host 10.20.0.60 key 7 "S3cr3tHostKey"
snmp-server user admin network-admin auth md5 0xS3cr3tAuth0123 priv 0xS3cr3tPriv0123 localizedkey
snmp-server host 10.20.0.50 traps version 2c S3cr3tTrapComm
snmp-server community S3cr3tRoComm group network-operator
ntp authentication-key 1 md5 S3cr3tNtpKey 7
interface Ethernet1/1
  description uplink-to-core
  no shutdown
`
	case "asa":
		return `
: Saved
:
ASA Version 9.12(4)37
!
hostname ` + h + `
enable password $sha512$5000$S3cr3tEnableHash$S3cr3tValue== pbkdf2
passwd S3cr3tTelnetPwd encrypted
username admin password $sha512$5000$S3cr3tUserHash$S3cr3tValue== pbkdf2 privilege 15
names
!
interface GigabitEthernet1/1
 nameif outside
 security-level 0
 ip address 203.0.113.2 255.255.255.0
!
crypto ikev1 policy 10
tunnel-group 198.51.100.7 ipsec-attributes
 ikev1 pre-shared-key S3cr3tPsk
aaa-server TACACS protocol tacacs+
aaa-server TACACS (inside) host 10.20.0.60
 key S3cr3tAaaKey
snmp-server host inside 10.20.0.50 community S3cr3tAsaComm version 2c
snmp-server community S3cr3tAsaRo
`
	}
	return `
Building configuration...

Current configuration : 4012 bytes
!
version 17.3
service timestamps debug datetime msec
service password-encryption
!
hostname ` + h + `
!
enable secret 9 $9$S3cr3tSalt$S3cr3tScryptHash0123456789
!
username admin privilege 15 secret 9 $9$S3cr3tSalt$S3cr3tAdminHash
username backup privilege 1 password 0 S3cr3t Plain Pass
username netops password 7 S3cr3t0822455D0A16
!
key chain OSPF-KEYS
 key 1
  key-string 7 S3cr3tKeyString
!
interface Vlan10
 ip address 10.20.10.1 255.255.255.0
 ip ospf message-digest-key 1 md5 7 S3cr3tOspfKey
!
router bgp 65001
 neighbor 10.20.0.2 password 7 S3cr3tBgpPass
!
tacacs-server host 10.20.0.60 key 7 S3cr3tTacacsKey
radius-server key 7 S3cr3tRadiusKey
crypto isakmp key S3cr3tIsakmp address 198.51.100.7
snmp-server community S3cr3tRoComm RO
snmp-server community S3cr3tRwComm RW 10
snmp-server host 10.20.0.50 version 2c S3cr3tTrapComm
ntp authentication-key 1 md5 S3cr3tNtpKey 7
!
line vty 0 4
 password 7 S3cr3tVtyPass
 transport input ssh
!
end
`
}

func (d *Device) vrpVersion() string {
	if d.Kind == "h3c" {
		return `H3C Comware Software, Version 7.1.070, Release 6728P22
Copyright (c) 2004-2021 New H3C Technologies Co., Ltd. All rights reserved.
H3C S6850-56HF uptime is 0 weeks, 3 days, 4 hours, 5 minutes
Last reboot reason : User reboot

Boot image: flash:/S6850-CMW710-BOOT-R6728P22.bin
Boot image version: 7.1.070, Release 6728P22
  Compiled Apr 18 2021 15:00:00
System image: flash:/S6850-CMW710-SYSTEM-R6728P22.bin
System image version: 7.1.070, Release 6728P22
  Compiled Apr 18 2021 15:00:00

Slot 1:
Uptime is 0 weeks,3 days,4 hours,5 minutes
S6850-56HF with 4 Processors
BOARD TYPE:         S6850-56HF
DRAM:               8192M bytes
FLASH:              4096M bytes
PCB 1 Version:      VER.B
Bootrom Version:    143
CPLD 1 Version:     002
Release Version:    H3C S6850-56HF-6728P22
Patch Version  :    None
Reboot Cause  :     UserReboot
[SubSlot 0] 48SFP Plus+8QSFP28`
	}
	return `Huawei Versatile Routing Platform Software
VRP (R) software, Version 8.180 (CE6850 V200R005C10SPC800)
Copyright (C) 2012-2018 Huawei Technologies Co., Ltd.
HUAWEI CE6850-48S6Q-HI uptime is 45 days, 6 hours, 7 minutes
Patch Version: V200R005SPH012

CE6850-48S6Q-HI(Master) 1 : uptime is  45 days, 6 hours, 6 minutes
        StartupTime 2026/09/10   08:00:00
Memory    Size    : 2048 M bytes
Flash     Size    : 1024 M bytes
CE6850-48S6Q-HI version information
1. PCB    Version : CEM48S6QP04    VER B
2. MAB    Version : 1
3. Board  Type    : CE6850-48S6Q-HI
4. CPLD1  Version : 102
5. BIOS   Version : 386`
}

func (d *Device) h3cManuinfo() string {
	return `Slot 1 CPU 0:
DEVICE_NAME          : S6850-56HF
DEVICE_SERIAL_NUMBER : ` + d.serial() + `
MAC_ADDRESS          : 0011-2233-4466
MANUFACTURING_DATE   : 2019-06-12
VENDOR_NAME          : H3C

Fan 1:
DEVICE_NAME          : FAN-40B-1-A
DEVICE_SERIAL_NUMBER : 210231A1SYNTHFAN01
MANUFACTURING_DATE   : 2019-05-20
VENDOR_NAME          : H3C`
}

func (d *Device) vrpLogbuffer() string {
	h := d.Hostname
	if d.Kind == "h3c" {
		return `Log buffer: Enabled
Max buffer size: 1024
Actual buffer size: 512
Dropped messages: 0
Overwritten messages: 0
Current messages: 3
%Oct 26 13:52:10:123 2026 ` + h + ` IFNET/3/PHY_UPDOWN: Physical state on the interface Ten-GigabitEthernet1/0/24 changed to down.
%Oct 26 13:57:02:881 2026 ` + h + ` DEV/2/FAN_ABSENT: -Slot=1; Fan 2 is absent.
%Oct 26 13:58:40:002 2026 ` + h + ` SHELL/5/SHELL_LOGIN: admin logged in from 10.20.0.99.`
	}
	return `Logging buffer configuration and contents : enabled
Allowed max buffer size : 1024
Actual buffer size : 512
Channel number : 4 , Channel name : logbuffer
Dropped messages : 0
Overwritten messages : 0
Current messages : 3

Oct 26 2026 13:52:10 ` + h + ` %%01IFNET/4/IF_STATE(l)[0]:Interface 10GE1/0/24 has turned into DOWN state.
Oct 26 2026 13:57:02 ` + h + ` %%01DEVM/1/hwFanRemove_active(l)[1]:The fan module was removed. (EntityPhysicalIndex=16842752, EntPhysicalIndex=16842752, EntPhysicalName=FAN2, EntityTrapFaultID=137216)
Oct 26 2026 13:58:40 ` + h + ` %%01SSH/5/SSH_USER_LOGIN(l)[2]:The SSH user succeeded in logging in. (ServiceType=stelnet, UserName=admin, UserAddress=10.20.0.99, LocalAddress=10.20.0.1, VPNInstanceName=_public_)`
}

// vrpDiag 是 display diagnostic-information 的输出：各段 display 命令的结果，含当前配置（其中的密文用来测试脱敏）。
func (d *Device) vrpDiag() string {
	var b strings.Builder
	sec := func(cmd string) {
		fmt.Fprintf(&b, "\n===============================================\n  ===============display %s===============\n===============================================\n\n", cmd)
	}
	sec("clock")
	b.WriteString("2026-10-26 14:03:11+08:00\nMonday\nTime Zone(DefaultZoneName) : UTC+08:00\n")
	sec("version")
	b.WriteString(d.vrpVersion() + "\n")
	sec("current-configuration")
	b.WriteString(d.vrpConfig())
	if d.Kind == "h3c" {
		sec("device manuinfo")
		b.WriteString(d.h3cManuinfo() + "\n")
	} else {
		sec("esn")
		b.WriteString("ESN of slot 1: " + d.serial() + "\n")
	}
	sec("device")
	b.WriteString("Slot  Card   Type               Online   Power Register     Alarm     Primary\n")
	b.WriteString("1     -      CE6850-48S6Q-HI    Present  On    Registered   Normal    Master\n")
	b.WriteString("FAN1  -      FAN-40SB-B         Present  On    Registered   Normal    NA\n")
	b.WriteString("FAN2  -      FAN-40SB-B         Absent   -     -            -         NA\n")
	sec("interface brief")
	for i := 1; i <= d.ports(); i++ {
		name := fmt.Sprintf("10GE1/0/%d", i)
		if d.Kind == "h3c" {
			name = fmt.Sprintf("XGE1/0/%d", i)
		}
		state := "up"
		if i%12 == 0 {
			state = "down"
		}
		fmt.Fprintf(&b, "%-20s %-6s %-6s %6.2f%% %6.2f%% %10d %10d\n", name, state, state, float64(i%10)/10, float64(i%7)/10, i%5, i%3)
		fmt.Fprintf(&b, "  Input:  %d packets, %d bytes; CRC: %d, Giants: 0, Runts: 0\n", 100000*i, 64000000*i, i%5)
		fmt.Fprintf(&b, "  Output: %d packets, %d bytes; Discard: 0\n", 90000*i, 58000000*i)
	}
	sec("logbuffer")
	b.WriteString(d.vrpLogbuffer() + "\n")
	// 输出里出现和提示符一样的行，不能当成命令结束。
	sec("history-command")
	b.WriteString("<" + d.Hostname + ">\n  display version\n  display diagnostic-information\n")
	return b.String()
}

func (d *Device) vrpConfig() string {
	h := d.Hostname
	if d.Kind == "h3c" {
		return `#
 version 7.1.070, Release 6728P22
#
 sysname ` + h + `
#
 password-recovery enable
#
local-user admin class manage
 password hash $h$6$S3cr3tSalt$S3cr3tHashValue0123456789==
 service-type ssh
 authorization-attribute user-role network-admin
#
super password role network-admin hash $h$6$S3cr3tSuper$S3cr3tHash==
#
radius scheme rad1
 primary authentication 10.20.0.61
 key authentication cipher $c$3$S3cr3tRadiusCipher==
#
hwtacacs scheme tac1
 primary authentication 10.20.0.60
 key authentication simple S3cr3tTacPlain
#
 snmp-agent
 snmp-agent community read simple S3cr3tReadComm
 snmp-agent community write cipher $c$3$S3cr3tWriteComm==
 snmp-agent sys-info version v2c v3
 snmp-agent target-host trap address udp-domain 10.20.0.50 params securityname S3cr3tTrapName v2c
 snmp-agent usm-user v3 snmpv3 group1 simple authentication-mode sha S3cr3tAuthPlain privacy-mode aes128 S3cr3tPrivPlain
#
return
`
	}
	return `!Software Version V200R005C10SPC800
!Last configuration was updated at 2026-10-19 10:00:00+08:00 by admin
#
sysname ` + h + `
#
aaa
 local-user admin password irreversible-cipher $1c$S3cr3tSalt$S3cr3tIrreversible0123$
 local-user admin service-type ssh
 local-user admin level 3
#
hwtacacs-server template tac1
 hwtacacs-server authentication 10.20.0.60
 hwtacacs-server shared-key cipher %^%#S3cr3tTacacsCipher%^%#
#
ospf 1
 area 0.0.0.0
  authentication-mode md5 1 cipher %^%#S3cr3tOspfCipher%^%#
#
snmp-agent
snmp-agent community read cipher %^%#S3cr3tReadComm%^%#
snmp-agent community write cipher %@%@S3cr3tWriteComm%@%@
snmp-agent sys-info version v2c v3
snmp-agent target-host trap address udp-domain 10.20.0.50 params securityname cipher %^%#S3cr3tTrapName%^%# v2c
#
user-interface vty 0 4
 authentication-mode aaa
 set authentication password cipher %^%#S3cr3tVtyCipher%^%#
#
return
`
}
