// SPDX-License-Identifier: GPL-3.0-or-later

// Package appdata 管理本机数据文件夹和设置。
//
// 数据文件夹默认是 %APPDATA%\LogDiag Collector，里面有设备库 devices.json 和设置 settings.json；
// 日志包默认保存到“文档\LogDiag Collector”。
package appdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FolderName 是数据文件夹和默认保存位置的文件夹名。
const FolderName = "LogDiag Collector"

// DefaultDir 返回数据文件夹：Windows 上是 %APPDATA%\LogDiag Collector。
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("找不到用户配置文件夹：%w", err)
	}
	return filepath.Join(base, FolderName), nil
}

// DefaultOutputDir 返回日志包的默认保存位置：“文档\LogDiag Collector”。
func DefaultOutputDir() string {
	if docs := documentsDir(); docs != "" {
		return filepath.Join(docs, FolderName)
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, FolderName)
	}
	return FolderName
}

// Settings 是用户可以修改的设置。
type Settings struct {
	// OutputDir 是日志包的保存位置，按 [客户]/日期/设备 建文件夹。
	OutputDir string `json:"output_dir"`
	// Concurrency 是同时进行的收集任务数。生成日志包会占用 BMC 资源，同一台设备始终只跑一个任务。
	Concurrency int `json:"concurrency"`
	// TimeoutMinutes 是单台设备的收集超时。
	TimeoutMinutes int `json:"timeout_minutes"`
	// Record 为 true 时收集过程保留诊断记录（已去掉密码和令牌），失败后可以导出发给开发者。
	Record bool `json:"record"`
	// AcceptEULA 为 true 时，Dell iDRAC 没接受 SupportAssist 许可协议的由工具代为接受。
	// 这会改动设备设置，默认关闭，由用户自己决定。
	AcceptEULA bool `json:"accept_eula"`
	// OSModule 为 false 时隐藏 OS 巡检模块。
	OSModule bool `json:"os_module"`
}

// 设置的取值范围。
const (
	MinConcurrency = 1
	MaxConcurrency = 32
	MinTimeout     = 5
	MaxTimeout     = 180
)

// Defaults 返回默认设置。
func Defaults() Settings {
	return Settings{
		OutputDir:      DefaultOutputDir(),
		Concurrency:    8,
		TimeoutMinutes: 40,
		Record:         true,
		OSModule:       true,
	}
}

// Validate 检查取值范围，并把保存位置整理成绝对路径。
func (s *Settings) Validate() error {
	if s.OutputDir == "" {
		return errors.New("保存位置不能为空")
	}
	if !filepath.IsAbs(s.OutputDir) {
		return fmt.Errorf("保存位置 %q 不是完整路径，请写成 D:\\日志 这样的形式", s.OutputDir)
	}
	s.OutputDir = filepath.Clean(s.OutputDir)
	if s.Concurrency < MinConcurrency || s.Concurrency > MaxConcurrency {
		return fmt.Errorf("同时收集的设备数应在 %d 到 %d 之间", MinConcurrency, MaxConcurrency)
	}
	if s.TimeoutMinutes < MinTimeout || s.TimeoutMinutes > MaxTimeout {
		return fmt.Errorf("单台超时应在 %d 到 %d 分钟之间", MinTimeout, MaxTimeout)
	}
	return nil
}

// SettingsStore 读写 settings.json，可以被多个 goroutine 同时使用。
type SettingsStore struct {
	path string
	mu   sync.Mutex
	cur  Settings
}

// OpenSettings 读取 dir 下的 settings.json；文件不存在时用默认值。
// 文件损坏时改名备份并改用默认值，第二个返回值说明发生了什么。
func OpenSettings(dir string) (*SettingsStore, string, error) {
	s := &SettingsStore{path: filepath.Join(dir, "settings.json"), cur: Defaults()}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("读取设置：%w", err)
	}
	v := Defaults() // 旧版本文件里没有的字段保持默认值
	if err := json.Unmarshal(data, &v); err == nil && v.Validate() == nil {
		s.cur = v
		return s, "", nil
	}
	backup := s.path + ".bad-" + time.Now().Format("20060102-150405")
	if err := os.Rename(s.path, backup); err != nil {
		return nil, "", fmt.Errorf("设置文件已损坏，也无法改名备份：%w", err)
	}
	return s, "设置文件已损坏，已改用默认设置，原文件备份为 " + backup, nil
}

// Get 返回当前设置。
func (s *SettingsStore) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Set 校验并保存设置。
func (s *SettingsStore) Set(v Settings) (Settings, error) {
	if err := v.Validate(); err != nil {
		return Settings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return Settings{}, err
	}
	if err := WriteFile(s.path, append(data, '\n')); err != nil {
		return Settings{}, fmt.Errorf("保存设置：%w", err)
	}
	s.cur = v
	return v, nil
}

// WriteFile 先写临时文件再改名，写到一半断电或崩溃时原文件不受影响。
func WriteFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
