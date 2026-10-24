// SPDX-License-Identifier: GPL-3.0-or-later

package update

import (
	"regexp"
	"strconv"
	"strings"
)

// Version 是语义化版本号：主.次.修订，可带预发布部分（-beta.1）；构建信息（+…）忽略。
type Version struct {
	Major, Minor, Patch int
	Pre                 []string
}

var versionRE = regexp.MustCompile(`^v?(\d{1,9})\.(\d{1,9})\.(\d{1,9})(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z.-]+)?$`)

// ParseVersion 解析版本号，可带前缀 v。
func ParseVersion(s string) (Version, bool) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.Pre = strings.Split(m[4], ".")
	}
	return v, true
}

// String 返回不带前缀 v 的版本号。
func (v Version) String() string {
	s := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Dev 报告是不是开发版本（build.sh 不带版本号编译出的 0.0.0-dev.g<提交>）。
func (v Version) Dev() bool {
	return len(v.Pre) > 0 && v.Pre[0] == "dev"
}

// Compare 按语义化版本的规则比较：v 比 w 旧返回 -1，相同返回 0，更新返回 1。
// 预发布版本比对应的正式版旧，例如 0.2.0-beta.1 < 0.2.0。
func (v Version) Compare(w Version) int {
	for _, d := range [...]int{v.Major - w.Major, v.Minor - w.Minor, v.Patch - w.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case len(v.Pre) == 0 && len(w.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(w.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(w.Pre); i++ {
		a, b := v.Pre[i], w.Pre[i]
		an, aNum := number(a)
		bn, bNum := number(b)
		switch {
		case aNum && bNum:
			if an != bn {
				return sign(an - bn)
			}
		case aNum:
			return -1 // 数字标识比字母标识旧
		case bNum:
			return 1
		case a != b:
			return sign(strings.Compare(a, b))
		}
	}
	return sign(len(v.Pre) - len(w.Pre))
}

func number(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" || len(s) > 9 {
		return 0, false
	}
	n, _ := strconv.Atoi(s)
	return n, true
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
