// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// TransportOptions 决定怎样连接设备。
type TransportOptions struct {
	// PinnedSHA256 非空时只接受证书 SHA-256 指纹与之相同的设备（首次信任后锁定）。
	// 为空时不校验证书：BMC 基本都是自签名证书。
	PinnedSHA256 string
	// Legacy 为老固件放开 TLS 1.0/1.1 和旧加密套件，只对标记为“老设备”的目标使用。
	Legacy bool
	// OnCertificate 在每次握手后收到设备证书的 SHA-256 指纹，用于首次信任时保存。
	OnCertificate func(sha256Hex string)
}

// NewHTTPClient 返回连接 BMC 用的 http.Client。
//
// 它不使用系统或环境变量里的代理（管理网流量必须直连），也不跟随重定向，
// 以免把 X-Auth-Token 带到别的主机。
func NewHTTPClient(o TransportOptions) *http.Client {
	pin := strings.ToLower(strings.ReplaceAll(o.PinnedSHA256, ":", ""))
	cfg := &tls.Config{
		InsecureSkipVerify: true, // 由 VerifyConnection 按指纹校验
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("设备没有提供证书")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			fp := hex.EncodeToString(sum[:])
			if o.OnCertificate != nil {
				o.OnCertificate(fp)
			}
			if pin != "" && fp != pin {
				return fmt.Errorf("设备证书指纹 %s 与已保存的 %s 不一致，设备可能被更换或存在中间人", fp, pin)
			}
			return nil
		},
	}
	if o.Legacy {
		cfg.MinVersion = tls.VersionTLS10
		cfg.CipherSuites = legacyCipherSuites()
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       cfg,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// legacyCipherSuites 返回所有可用于 TLS 1.2 及以下的套件，包括 Go 默认关闭的旧套件。
func legacyCipherSuites() []uint16 {
	var ids []uint16
	for _, s := range append(tls.CipherSuites(), tls.InsecureCipherSuites()...) {
		for _, v := range s.SupportedVersions {
			if v <= tls.VersionTLS12 {
				ids = append(ids, s.ID)
				break
			}
		}
	}
	return ids
}
