// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// MaxBatch 是一次批量添加的地址数上限。
const MaxBatch = 1024

// netipOf 解析不带端口的 IP 地址，用于排序。
func netipOf(s string) (netip.Addr, error) {
	return netip.ParseAddr(s)
}

// ExpandAddresses 把批量添加框里的文字展开成地址列表，去掉重复。支持的写法：
//
//	10.0.0.10                 一个地址（也可以是主机名或带端口）
//	10.0.0.10-50              同一网段的范围
//	10.0.0.10-10.0.0.50       完整范围
//	10.0.0.0/28               网段（不含网络地址和广播地址）
//
// 地址之间用换行、空格、逗号、分号或顿号分隔。
func ExpandAddresses(text string) ([]string, error) {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ' ' || r == '\t' || r == ',' || r == ';' || r == '，' || r == '；' || r == '、'
	})
	var out []string
	seen := map[string]bool{}
	add := func(s string) error {
		if !seen[s] {
			if len(out) >= MaxBatch {
				return fmt.Errorf("一次最多添加 %d 个地址", MaxBatch)
			}
			seen[s] = true
			out = append(out, s)
		}
		return nil
	}
	for _, f := range fields {
		addrs, err := expand(f)
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			if err := add(a); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("没有填写地址")
	}
	return out, nil
}

func expand(f string) ([]string, error) {
	if strings.Contains(f, "/") && !strings.Contains(f, "://") {
		p, err := netip.ParsePrefix(f)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("%q 不是有效的 IPv4 网段", f)
		}
		p = p.Masked()
		if p.Bits() < 32-10 {
			return nil, fmt.Errorf("网段 %s 太大，一次最多添加 %d 个地址", f, MaxBatch)
		}
		var out []string
		for a := p.Addr(); p.Contains(a); a = a.Next() {
			out = append(out, a.String())
		}
		if len(out) > 2 { // 去掉网络地址和广播地址
			out = out[1 : len(out)-1]
		}
		return out, nil
	}
	// 带连字符的主机名（bmc-01）不是范围：只有开头是 IPv4 地址时才按范围处理。
	if from, to, ok := strings.Cut(f, "-"); ok && isIPv4(from) {
		start := netip.MustParseAddr(from)
		end, err := netip.ParseAddr(to)
		if err != nil {
			// 只写了最后一段：10.0.0.10-50
			b := start.As4()
			end, err = netip.ParseAddr(fmt.Sprintf("%d.%d.%d.%s", b[0], b[1], b[2], to))
		}
		if err != nil || !end.Is4() || end.Less(start) {
			return nil, fmt.Errorf("%q 不是有效的地址范围，应写成 10.0.0.10-50 或 10.0.0.10-10.0.0.50", f)
		}
		var out []string
		for a := start; ; a = a.Next() {
			out = append(out, a.String())
			if a == end {
				break
			}
			if len(out) > MaxBatch {
				return nil, fmt.Errorf("范围 %s 太大，一次最多添加 %d 个地址", f, MaxBatch)
			}
		}
		return out, nil
	}
	a, err := NormalizeAddress(f)
	if err != nil {
		return nil, err
	}
	return []string{a}, nil
}

func isIPv4(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4()
}
