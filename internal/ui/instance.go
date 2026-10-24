// SPDX-License-Identifier: GPL-3.0-or-later

package ui

// 同一个数据文件夹只运行一个界面程序。再次运行程序时（例如关掉窗口后程序还在等收集任务完成），
// 新启动的程序请已经在运行的那个准备一个新窗口，自己退出，免得两个程序同时改设备库。
//
// 运行中的程序把地址和随机密钥写进数据文件夹的 instance.json，退出时删除。
// 开新窗口的请求要带这个密钥，网页拿不到它，也就没法借这个接口登录界面。

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	instanceFile   = "instance.json"
	instancePath   = "/instance/open"
	instanceHeader = "X-LogDiag-Instance"
)

type instanceInfo struct {
	PID int    `json:"pid"`
	URL string `json:"url"`
	Key string `json:"key"`
}

// Register 把本程序登记为 dataDir 里正在运行的界面，返回退出时调用的注销函数。
func (s *Server) Register(dataDir string) (unregister func(), err error) {
	key := randomHex(32)
	data, _ := json.Marshal(instanceInfo{PID: os.Getpid(), URL: s.URL(), Key: key})
	path := filepath.Join(dataDir, instanceFile)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return func() {}, err
	}
	s.mu.Lock()
	s.instanceKey = key
	s.mu.Unlock()
	return func() {
		// 只删自己写的那份：另一个程序可能已经登记了它自己。
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
			os.Remove(path)
		}
	}, nil
}

// FindRunning 请 dataDir 里登记的界面程序准备一个新窗口，返回带一次性令牌的地址。
// 没有程序在运行、登记信息已过期或程序正在退出时返回空字符串。
func FindRunning(dataDir string) string {
	data, err := os.ReadFile(filepath.Join(dataDir, instanceFile))
	if err != nil {
		return ""
	}
	var info instanceInfo
	if json.Unmarshal(data, &info) != nil || info.Key == "" {
		return ""
	}
	if u, err := url.Parse(info.URL); err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/" {
		return ""
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(info.URL, "/")+instancePath, nil)
	if err != nil {
		return ""
	}
	req.Header.Set(instanceHeader, info.Key)
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{}} // 本机地址，不走代理
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		URL string `json:"url"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out) != nil ||
		!strings.HasPrefix(out.URL, info.URL+"?token=") {
		return ""
	}
	return out.URL
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key := s.instanceKey
	s.mu.Unlock()
	given := r.Header.Get(instanceHeader)
	if r.Method != http.MethodPost || key == "" || subtle.ConstantTimeCompare([]byte(given), []byte(key)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	launch, ok := s.reopen()
	if !ok {
		http.Error(w, "exiting", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{"url": launch})
}
