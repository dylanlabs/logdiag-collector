// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const keyPrefix = "aesgcm"

// keyBox 用数据文件夹里的随机密钥加密，只用于在非 Windows 平台上开发调试。
type keyBox struct{ aead cipher.AEAD }

func newBox(dir string) (Box, error) {
	path := filepath.Join(dir, "secret.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, 32)
		rand.Read(key)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		err = os.WriteFile(path, key, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("读取密钥文件：%w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("密钥文件 %s 已损坏", path)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return keyBox{aead}, nil
}

func (keyBox) Method() string { return "本机密钥文件（仅开发调试用）" }

func (b keyBox) Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	rand.Read(nonce)
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return keyPrefix + ":" + base64.StdEncoding.EncodeToString(out), nil
}

func (b keyBox) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	method, data, ok := split(sealed)
	if !ok || method != keyPrefix {
		return "", ErrOtherMachine
	}
	blob, err := base64.StdEncoding.DecodeString(data)
	n := b.aead.NonceSize()
	if err != nil || len(blob) < n {
		return "", ErrOtherMachine
	}
	plain, err := b.aead.Open(nil, blob[:n], blob[n:], nil)
	if err != nil {
		return "", ErrOtherMachine
	}
	return string(plain), nil
}
