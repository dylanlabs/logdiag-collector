// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"encoding/base64"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const dpapiPrefix = "dpapi"

// entropy 是附加熵：其他程序即使在同一账号下调用 DPAPI，也要知道它才能解密。
var entropy = []byte("LogDiag Collector/device-credentials/1")

type dpapi struct{}

func newBox(string) (Box, error) { return dpapi{}, nil }

func (dpapi) Method() string { return "Windows DPAPI（绑定当前 Windows 账号）" }

func (dpapi) Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	out, err := crypt([]byte(plain), true)
	if err != nil {
		return "", fmt.Errorf("加密密码失败：%w", err)
	}
	return dpapiPrefix + ":" + base64.StdEncoding.EncodeToString(out), nil
}

func (dpapi) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	method, data, ok := split(sealed)
	if !ok || method != dpapiPrefix {
		return "", ErrOtherMachine
	}
	blob, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(blob) == 0 {
		return "", ErrOtherMachine
	}
	plain, err := crypt(blob, false)
	if err != nil {
		return "", ErrOtherMachine
	}
	return string(plain), nil
}

func crypt(data []byte, protect bool) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	ent := windows.DataBlob{Size: uint32(len(entropy)), Data: &entropy[0]}
	var out windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
