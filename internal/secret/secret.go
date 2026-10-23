// SPDX-License-Identifier: GPL-3.0-or-later

// Package secret 加密保存设备密码。
//
// Windows 上用 DPAPI 绑定当前 Windows 用户：不用记主密码；设备库拷到别的电脑、
// 或在同一台电脑的其他 Windows 账号下都无法解密。其他系统只用于开发调试，
// 用数据文件夹里的随机密钥加密。
package secret

import (
	"errors"
	"strings"
)

// Box 加密和解密密码。空字符串表示没有密码，加密后仍是空字符串。
type Box interface {
	Seal(plain string) (string, error)
	Open(sealed string) (string, error)
	// Method 是给用户看的加密方式说明。
	Method() string
}

// ErrOtherMachine 表示密文不是用本机、本账号的密钥加密的。
var ErrOtherMachine = errors.New("无法解密已保存的密码：设备库可能是从别的电脑或别的 Windows 账号复制来的，请重新填写密码")

// New 返回本平台的加密方式；dir 是数据文件夹，只有非 Windows 平台用它存放密钥。
func New(dir string) (Box, error) {
	return newBox(dir)
}

// split 拆出密文的方式前缀，例如 "dpapi:AAAA"。
func split(sealed string) (method, data string, ok bool) {
	return strings.Cut(sealed, ":")
}
