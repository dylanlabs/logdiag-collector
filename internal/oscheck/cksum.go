// SPDX-License-Identifier: GPL-3.0-or-later

package oscheck

// POSIX cksum 的 CRC：多项式 0x04C11DB7，不反转，处理完数据后再按字节处理文件长度，
// 最后取反。上传脚本后在设备上执行 cksum 比对这个值，确认脚本没有在传输中损坏。
// 用 cksum 而不是 sha256sum：cksum 是 POSIX 必备命令，AIX、Solaris、HP-UX 上都有。

var cksumTable [256]uint32

func init() {
	const poly = 0x04C11DB7
	for i := range cksumTable {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ poly
			} else {
				c <<= 1
			}
		}
		cksumTable[i] = c
	}
}

// cksum 返回和 POSIX cksum 命令一致的校验和。
func cksum(data []byte) uint32 {
	var c uint32
	for _, b := range data {
		c = c<<8 ^ cksumTable[byte(c>>24)^b]
	}
	// 把文件长度按字节（低位在前）继续喂进 CRC。
	for n := len(data); n != 0; n >>= 8 {
		c = c<<8 ^ cksumTable[byte(c>>24)^byte(n)]
	}
	return ^c
}
