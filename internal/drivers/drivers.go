// SPDX-License-Identifier: GPL-3.0-or-later

// Package drivers 识别设备、选择采集驱动并执行一次收集。
//
// 识别流程：匿名读 Redfish 服务根 → 登录 → 读第一个系统和管理器，
// 得到厂商、型号、序列号、BMC 固件 → 按厂商选驱动。v0.1 的服务器品牌都支持 Redfish。
// Dell 的序列号用服务标签（系统的 SKU），和机箱标签、Dell 支持网站一致。
//
// 网络设备（Request.Type 为 network）不走 Redfish，由 netdev 用 SSH 登录命令行识别和收集。
package drivers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/dylanlabs/logdiag-collector/internal/collect"
	"github.com/dylanlabs/logdiag-collector/internal/collect/dell"
	"github.com/dylanlabs/logdiag-collector/internal/collect/hpe"
	"github.com/dylanlabs/logdiag-collector/internal/collect/huawei"
	"github.com/dylanlabs/logdiag-collector/internal/collect/inspur"
	"github.com/dylanlabs/logdiag-collector/internal/collect/lenovo"
	"github.com/dylanlabs/logdiag-collector/internal/collect/netdev"
	"github.com/dylanlabs/logdiag-collector/internal/redfish"
)

// Brand 是命令行 --brand 可选的品牌。
type Brand struct {
	Key    string // huawei、xfusion、inspur、lenovo、dell、hpe
	Vendor string // 写进 manifest 的厂商名
	Driver collect.Driver
}

// Brands 列出已支持的品牌。识别时 xFusion 要排在 Huawei 前面：超聚变部分固件仍保留 Huawei 的 OEM 键。
var Brands = []Brand{
	{"xfusion", "xFusion", huawei.Driver{}},
	{"huawei", "Huawei", huawei.Driver{}},
	{"inspur", "Inspur", inspur.Driver{}},
	{"lenovo", "Lenovo", lenovo.Driver{}},
	{"dell", "Dell", dell.Driver{}},
	{"hpe", "HPE", hpe.Driver{}},
}

// keywords 是识别品牌时在厂商名、OEM 键里查找的词（小写）。
var keywords = map[string][]string{
	"xfusion": {"xfusion"},
	"huawei":  {"huawei"},
	"inspur":  {"inspur", "ieit"}, // 浪潮信息新固件可能用 IEIT
	"lenovo":  {"lenovo"},
	"dell":    {"dell"},
	"hpe":     {"hpe", "hewlett"},
}

// words 是必须作为完整单词出现的词：Gen9 的制造商和 OEM 键是 HP，按子串找会误认别的名字。
var words = map[string][]string{
	"hpe": {"hp"},
}

// BrandByKey 按命令行写法查找品牌。
func BrandByKey(key string) (Brand, bool) {
	i := slices.IndexFunc(Brands, func(b Brand) bool { return strings.EqualFold(b.Key, key) })
	if i < 0 {
		return Brand{}, false
	}
	return Brands[i], true
}

// Detect 依次按系统的制造商、服务根的 Vendor、OEM 键、产品名判断品牌。
func Detect(root *redfish.ServiceRoot, system, manager *redfish.Resource) (Brand, bool) {
	var sources [][]string
	if system != nil {
		sources = append(sources, []string{system.Manufacturer})
	}
	if root != nil {
		sources = append(sources, []string{root.Vendor}, keysOf(root.Oem))
	}
	if manager != nil {
		sources = append(sources, keysOf(manager.Oem))
	}
	if root != nil {
		sources = append(sources, []string{root.Product})
	}
	for _, src := range sources {
		text := strings.ToLower(strings.Join(src, " "))
		if strings.TrimSpace(text) == "" {
			continue
		}
		tokens := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		for _, b := range Brands {
			for _, kw := range keywords[b.Key] {
				if strings.Contains(text, kw) {
					return b, true
				}
			}
			for _, w := range words[b.Key] {
				if slices.Contains(tokens, w) {
					return b, true
				}
			}
		}
	}
	return Brand{}, false
}

// serialOf 返回写进 manifest 的序列号。Dell 用服务标签（SKU），SerialNumber 是主板序列号。
func serialOf(b Brand, system *redfish.Resource) string {
	if b.Key == "dell" {
		if tag := strings.TrimSpace(system.SKU); tag != "" {
			return tag
		}
	}
	return strings.TrimSpace(system.SerialNumber)
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Request 是一次收集的参数。
type Request struct {
	Address  string // 管理口地址
	User     string
	Password string
	Brand    string // 为空时自动识别
	// AcceptEULA 允许代为接受 Dell SupportAssist 许可协议，见 collect.Job.AcceptEULA。
	AcceptEULA bool

	HTTPClient *http.Client // 由调用方配置证书锁定、老设备 TLS 和录制
	UserAgent  string
	Store      *collect.Store
	Progress   func(collect.Event)
	Sleep      func(context.Context, time.Duration) error // 测试用

	// 以下用于网络设备。
	Type        string           // 设备类型，network 表示网络设备；空表示服务器
	Enable      string           // Cisco 的 enable 密码，可空
	HostKey     string           // 期望的 SSH 主机密钥指纹，空表示首次连接
	NetRecorder *netdev.Recorder // 命令行会话的诊断记录，可为 nil
	NetDial     netdev.Dialer    // 测试用
	NetTiming   netdev.Timing    // 测试用
}

func (req Request) network() bool { return req.Type == netdev.DeviceType }

func (req Request) netRequest() netdev.Request {
	return netdev.Request{
		Address: req.Address, User: req.User, Password: req.Password, Enable: req.Enable, HostKey: req.HostKey,
		Store: req.Store, Progress: req.Progress, Recorder: req.NetRecorder, Dial: req.NetDial, Timing: req.NetTiming,
	}
}

// Collect 识别设备并收集一次日志包。会话在结束、失败或取消时都会删除。
func Collect(ctx context.Context, req Request) (*collect.Result, error) {
	report := func(stage collect.Stage, msg string) {
		if req.Progress != nil {
			req.Progress(collect.Event{Stage: stage, Percent: -1, Message: msg})
		}
	}
	if req.network() {
		res, err := netdev.Collect(ctx, req.netRequest())
		if err != nil {
			return nil, err
		}
		report(collect.StageDone, "已保存到 "+res.Path)
		return res, nil
	}
	var brand Brand
	if req.Brand != "" {
		var ok bool
		if brand, ok = BrandByKey(req.Brand); !ok {
			return nil, fmt.Errorf("不认识品牌 %q，可选：%s", req.Brand, brandKeys())
		}
	}

	c, err := redfish.New(req.Address, redfish.Options{HTTPClient: req.HTTPClient, UserAgent: req.UserAgent, Sleep: req.Sleep})
	if err != nil {
		return nil, err
	}
	job := &collect.Job{
		BaseURL:    c.BaseURL(),
		HTTPClient: req.HTTPClient,
		UserAgent:  req.UserAgent,
		User:       req.User,
		Password:   req.Password,
		AcceptEULA: req.AcceptEULA,
		Store:      req.Store,
		Progress:   req.Progress,
		Sleep:      req.Sleep,
	}
	if u, err := url.Parse(c.BaseURL()); err == nil {
		job.Device.Address = u.Hostname()
	}

	report(collect.StageProbe, "读取 Redfish 服务根")
	root, err := c.ServiceRoot(ctx)
	var httpErr *redfish.HTTPError
	switch {
	case err == nil:
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound && brand.Key == "inspur":
		// 关闭了 Redfish 的浪潮设备：跳过识别，直接走网页接口。
		return brand.Driver.Collect(ctx, job)
	case errors.As(err, &httpErr):
		// 个别固件的服务根也要求登录，登录后再读。
	default:
		return nil, fmt.Errorf("读取服务根：%w", err)
	}

	report(collect.StageProbe, "登录 Redfish 读取型号和序列号")
	if err := c.Login(ctx, req.User, req.Password); err != nil {
		return nil, fmt.Errorf("登录：%w", err)
	}
	defer c.Logout(ctx)
	job.Redfish = c

	system, manager, err := identify(ctx, c, root)
	if err != nil {
		return nil, err
	}
	job.Manager, job.System = manager, system
	if brand.Key == "" {
		var ok bool
		if brand, ok = Detect(root, system, manager); !ok {
			return nil, fmt.Errorf("没认出设备品牌（制造商 %q），可用 --brand 指定：%s：%w",
				system.Manufacturer, brandKeys(), redfish.ErrUnsupported)
		}
	}
	job.Device.Type = "server"
	job.Device.Vendor = brand.Vendor
	job.Device.Model = strings.TrimSpace(system.Model)
	job.Device.Serial = serialOf(brand, system)
	job.Device.Hostname = strings.TrimSpace(system.HostName)
	job.Device.Controller.Firmware = strings.TrimSpace(manager.FirmwareVersion)

	report(collect.StageProbe, fmt.Sprintf("识别为 %s %s（序列号 %s，BMC 固件 %s），使用驱动 %s",
		brand.Vendor, job.Device.Model, job.Device.Serial, job.Device.Controller.Firmware, brand.Driver.Name()))
	res, err := brand.Driver.Collect(ctx, job)
	if err != nil {
		return nil, err
	}
	report(collect.StageDone, "已保存到 "+res.Path)
	return res, nil
}

// Identity 是登录后读到的设备信息，设备表的“识别”用它回填。
type Identity struct {
	Brand        Brand  // 没认出时为零值（Key 为空）；网络设备只有 Key 和 Vendor
	Manufacturer string // 系统的制造商原文
	Model        string
	Serial       string
	Hostname     string
	Firmware     string // BMC 固件版本；网络设备是系统和版本，例如“NX-OS 9.3(8)”
	SSHHostKey   string // 网络设备的 SSH 主机密钥指纹
}

// Vendor 返回写进设备表的品牌：认出时用驱动的厂商名，否则用制造商原文。
func (id *Identity) Vendor() string {
	if id.Brand.Vendor != "" {
		return id.Brand.Vendor
	}
	return id.Manufacturer
}

// Identify 登录读取型号、序列号、主机名和 BMC 固件，然后退出会话。只读，不触发收集。
// req.Brand、Store、Progress 不使用。
func Identify(ctx context.Context, req Request) (*Identity, error) {
	if req.network() {
		id, err := netdev.Identify(ctx, req.netRequest())
		if err != nil {
			return nil, err
		}
		return &Identity{
			Brand:        Brand{Key: strings.ToLower(id.Vendor), Vendor: id.Vendor},
			Manufacturer: id.Vendor, Model: id.Model, Serial: id.Serial, Hostname: id.Hostname,
			Firmware: id.Firmware(), SSHHostKey: id.HostKey,
		}, nil
	}
	c, err := redfish.New(req.Address, redfish.Options{HTTPClient: req.HTTPClient, UserAgent: req.UserAgent, Sleep: req.Sleep})
	if err != nil {
		return nil, err
	}
	root, err := c.ServiceRoot(ctx)
	var httpErr *redfish.HTTPError
	if err != nil && !errors.As(err, &httpErr) {
		return nil, fmt.Errorf("读取服务根：%w", err)
	}
	if err := c.Login(ctx, req.User, req.Password); err != nil {
		return nil, fmt.Errorf("登录：%w", err)
	}
	defer c.Logout(ctx)
	system, manager, err := identify(ctx, c, root)
	if err != nil {
		return nil, err
	}
	id := &Identity{
		Manufacturer: strings.TrimSpace(system.Manufacturer),
		Model:        strings.TrimSpace(system.Model),
		Serial:       strings.TrimSpace(system.SerialNumber),
		Hostname:     strings.TrimSpace(system.HostName),
		Firmware:     strings.TrimSpace(manager.FirmwareVersion),
	}
	if b, ok := Detect(root, system, manager); ok {
		id.Brand = b
		id.Serial = serialOf(b, system)
	}
	return id, nil
}

// identify 读第一个系统和第一个管理器。系统读不到时只用管理器的信息。
func identify(ctx context.Context, c *redfish.Client, root *redfish.ServiceRoot) (*redfish.Resource, *redfish.Resource, error) {
	systems, managers := "/redfish/v1/Systems", "/redfish/v1/Managers"
	if root != nil {
		if root.Systems.ID != "" {
			systems = root.Systems.ID
		}
		if root.Managers.ID != "" {
			managers = root.Managers.ID
		}
	}
	manager, err := first(ctx, c, managers)
	if err != nil {
		return nil, nil, fmt.Errorf("读取管理器：%w", err)
	}
	system, err := first(ctx, c, systems)
	if err != nil {
		system = &redfish.Resource{}
	}
	return system, manager, nil
}

func first(ctx context.Context, c *redfish.Client, coll string) (*redfish.Resource, error) {
	members, err := c.Members(ctx, coll)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("%s 是空的：%w", coll, redfish.ErrUnsupported)
	}
	var r redfish.Resource
	if _, err := c.Get(ctx, members[0], &r); err != nil {
		return nil, err
	}
	if r.ODataID == "" {
		r.ODataID = members[0]
	}
	return &r, nil
}

func brandKeys() string {
	var keys []string
	for _, b := range Brands {
		keys = append(keys, b.Key)
	}
	return strings.Join(keys, "、")
}
