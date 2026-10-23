// SPDX-License-Identifier: GPL-3.0-or-later

// Package inventory 是本机设备库：设备的管理口信息、凭据模板、连通状态和最后收集结果。
//
// 设备库保存在数据文件夹的 devices.json 里，密码用 secret.Box 加密后保存，
// 界面和导出文件都拿不到明文密码；只有收集、识别时由 Access 解密。
package inventory

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dylanlabs/logdiag-collector/internal/appdata"
	"github.com/dylanlabs/logdiag-collector/internal/secret"
)

// FileFormat 是 devices.json 的格式标识。
const FileFormat = "logdiag.devices/1"

// 设备类型。
const (
	TypeServer  = "server"
	TypeStorage = "storage"
	TypeNetwork = "network"
	TypeOther   = "other"
)

// TypeNames 是设备类型的中文名。
var TypeNames = map[string]string{
	TypeServer:  "服务器",
	TypeStorage: "存储",
	TypeNetwork: "网络",
	TypeOther:   "其他",
}

// 管理口的连通状态：网络可达 → 服务可用。只做匿名检查，不登录、不产生审计日志。
const (
	ReachUnknown     = ""
	ReachChecking    = "checking"    // 正在检查（不保存）
	ReachUnreachable = "unreachable" // 连不上管理口端口
	ReachPort        = "reachable"   // 端口能连上，但 Redfish 服务没有正常响应
	ReachService     = "service"     // Redfish 服务可用
)

// 最近一次登录的结果。只在用户点“识别”或收集时登录。
const (
	LoginUnknown    = ""
	LoginChecking   = "checking"    // 正在识别（不保存）
	LoginOK         = "ok"          // 登录成功
	LoginAuthFailed = "auth_failed" // 账号或密码错误
	LoginFailed     = "failed"      // 其他原因失败
)

// Status 是设备的连通状态。
type Status struct {
	Reach        string    `json:"reach"`
	ReachMessage string    `json:"reach_message,omitempty"`
	CheckedAt    time.Time `json:"checked_at,omitzero"`
	Login        string    `json:"login"`
	LoginMessage string    `json:"login_message,omitempty"`
	LoginAt      time.Time `json:"login_at,omitzero"`
}

// LastCollect 是最近一次收集的结果。
type LastCollect struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	Dir     string    `json:"dir,omitempty"` // 成功时是设备文件夹的完整路径
}

// Device 是设备库里的一台设备。界面拿到的就是这个结构，不含密码。
type Device struct {
	ID           string       `json:"id"`
	Customer     string       `json:"customer"`
	Type         string       `json:"type"`
	Vendor       string       `json:"vendor"`
	Model        string       `json:"model"`
	Serial       string       `json:"serial"`
	Hostname     string       `json:"hostname"`
	HostIP       string       `json:"host_ip"`
	OS           string       `json:"os"`
	MgmtIP       string       `json:"mgmt_ip"`
	User         string       `json:"user"`
	CredentialID string       `json:"credential_id,omitempty"`
	Firmware     string       `json:"firmware"`
	Location     string       `json:"location"`
	Notes        string       `json:"notes"`
	LegacyTLS    bool         `json:"legacy_tls"`
	HasPassword  bool         `json:"has_password"`
	Status       Status       `json:"status"`
	LastCollect  *LastCollect `json:"last_collect,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

type deviceRecord struct {
	Device
	Password string `json:"password,omitempty"` // 密文
}

// Credential 是凭据模板：同一客户同一批设备常用同一个账号，改密码时只改一处。
type Credential struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	User        string    `json:"user"`
	Notes       string    `json:"notes"`
	HasPassword bool      `json:"has_password"`
	UsedBy      int       `json:"used_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type credentialRecord struct {
	Credential
	Password string `json:"password,omitempty"`
}

// Patch 是对设备的修改，nil 字段不变。Password 为空字符串表示清除密码。
type Patch struct {
	Customer     *string `json:"customer,omitempty"`
	Type         *string `json:"type,omitempty"`
	Vendor       *string `json:"vendor,omitempty"`
	Model        *string `json:"model,omitempty"`
	Serial       *string `json:"serial,omitempty"`
	Hostname     *string `json:"hostname,omitempty"`
	HostIP       *string `json:"host_ip,omitempty"`
	OS           *string `json:"os,omitempty"`
	MgmtIP       *string `json:"mgmt_ip,omitempty"`
	User         *string `json:"user,omitempty"`
	Password     *string `json:"password,omitempty"`
	CredentialID *string `json:"credential_id,omitempty"`
	Location     *string `json:"location,omitempty"`
	Notes        *string `json:"notes,omitempty"`
	LegacyTLS    *bool   `json:"legacy_tls,omitempty"`
}

// CredentialPatch 是对凭据模板的修改，nil 字段不变。
type CredentialPatch struct {
	Name     *string `json:"name,omitempty"`
	User     *string `json:"user,omitempty"`
	Password *string `json:"password,omitempty"`
	Notes    *string `json:"notes,omitempty"`
}

// ErrNotFound 表示设备或凭据模板不存在（可能已被删除）。
var ErrNotFound = errors.New("设备或凭据模板不存在，可能已被删除")

// Store 是设备库，可以被多个 goroutine 同时使用。
type Store struct {
	path string
	box  secret.Box
	now  func() time.Time

	mu      sync.Mutex
	devices []*deviceRecord
	creds   []*credentialRecord
	flush   *time.Timer // 状态变化攒一会儿再写盘
	closed  bool
}

type fileData struct {
	Format      string              `json:"format"`
	Devices     []*deviceRecord     `json:"devices"`
	Credentials []*credentialRecord `json:"credentials"`
}

// Open 读取 dir 下的 devices.json，不存在时新建空库。
// 文件损坏时改名备份后新建空库，第二个返回值说明发生了什么。
func Open(dir string, box secret.Box) (*Store, string, error) {
	s := &Store{path: filepath.Join(dir, "devices.json"), box: box, now: time.Now}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("读取设备库：%w", err)
	}
	var f fileData
	if err := json.Unmarshal(data, &f); err != nil || f.Format != FileFormat {
		backup := s.path + ".bad-" + time.Now().Format("20060102-150405")
		if err := os.Rename(s.path, backup); err != nil {
			return nil, "", fmt.Errorf("设备库文件已损坏，也无法改名备份：%w", err)
		}
		return s, "设备库文件无法读取，已新建空的设备库，原文件备份为 " + backup, nil
	}
	for _, d := range f.Devices {
		if d == nil || d.ID == "" {
			continue
		}
		// 上次退出时正在进行的检查不再有效。
		if d.Status.Reach == ReachChecking {
			d.Status.Reach = ReachUnknown
		}
		if d.Status.Login == LoginChecking {
			d.Status.Login = LoginUnknown
		}
		d.HasPassword = d.Password != ""
		s.devices = append(s.devices, d)
	}
	for _, c := range f.Credentials {
		if c != nil && c.ID != "" {
			c.HasPassword = c.Password != ""
			s.creds = append(s.creds, c)
		}
	}
	return s, "", nil
}

// Path 返回 devices.json 的位置。
func (s *Store) Path() string { return s.path }

// Close 把还没写盘的状态变化写入文件。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.flush != nil && s.flush.Stop() {
		s.flush = nil
		return s.saveLocked()
	}
	return nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(fileData{Format: FileFormat, Devices: s.devices, Credentials: s.creds}, "", "  ")
	if err != nil {
		return err
	}
	if err := appdata.WriteFile(s.path, append(data, '\n')); err != nil {
		return fmt.Errorf("保存设备库：%w", err)
	}
	return nil
}

// saveSoon 在一秒后写盘：批量检查几百台设备时不必每台都写一次。
func (s *Store) saveSoon() {
	if s.flush != nil || s.closed {
		return
	}
	s.flush = time.AfterFunc(time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.flush = nil
		s.saveLocked()
	})
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// List 返回全部设备，按客户、管理口 IP 排序。
func (s *Store) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, len(s.devices))
	for i, d := range s.devices {
		out[i] = d.Device
	}
	slices.SortStableFunc(out, func(a, b Device) int {
		return cmp.Or(strings.Compare(a.Customer, b.Customer), compareAddr(a.MgmtIP, b.MgmtIP), compareAddr(a.HostIP, b.HostIP))
	})
	return out
}

// compareAddr 让 10.0.0.9 排在 10.0.0.10 前面。
func compareAddr(a, b string) int {
	ia, ea := netipOf(a)
	ib, eb := netipOf(b)
	if ea == nil && eb == nil {
		return ia.Compare(ib)
	}
	return strings.Compare(a, b)
}

// Get 返回一台设备。
func (s *Store) Get(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.find(id); d != nil {
		return d.Device, true
	}
	return Device{}, false
}

// Customers 返回所有客户名称（去重、排序，不含空值）。
func (s *Store) Customers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, d := range s.devices {
		if d.Customer != "" && !slices.Contains(out, d.Customer) {
			out = append(out, d.Customer)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Store) find(id string) *deviceRecord {
	for _, d := range s.devices {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (s *Store) findCred(id string) *credentialRecord {
	for _, c := range s.creds {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// Add 新建一台设备。
func (s *Store) Add(p Patch) (Device, error) {
	added, err := s.AddMany([]Patch{p}, false)
	if err != nil {
		return Device{}, err
	}
	return added[0], nil
}

// AddMany 一次新建多台设备。skipDuplicates 为 true 时跳过同一客户下管理口 IP 重复的设备，
// 否则遇到重复就整体失败。返回新建的设备和跳过的数量。
func (s *Store) AddMany(ps []Patch, skipDuplicates bool) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var added []*deviceRecord
	for i, p := range ps {
		d := &deviceRecord{Device: Device{ID: newID(), Type: TypeServer, CreatedAt: now, UpdatedAt: now}}
		if err := s.apply(d, p); err != nil {
			if len(ps) > 1 {
				return nil, fmt.Errorf("第 %d 台：%w", i+1, err)
			}
			return nil, err
		}
		if s.duplicate(d, added) != nil {
			if skipDuplicates {
				continue
			}
			return nil, fmt.Errorf("客户“%s”下已有管理口 IP 为 %s 的设备", orNone(d.Customer), d.key())
		}
		added = append(added, d)
	}
	s.devices = append(s.devices, added...)
	if err := s.saveLocked(); err != nil {
		s.devices = s.devices[:len(s.devices)-len(added)]
		return nil, err
	}
	out := make([]Device, len(added))
	for i, d := range added {
		out[i] = d.Device
	}
	return out, nil
}

func orNone(customer string) string {
	if customer == "" {
		return "未填客户"
	}
	return customer
}

// key 是判断重复的依据：同一客户下的管理口 IP；没有管理口时用主机 IP。
func (d *deviceRecord) key() string {
	if d.MgmtIP != "" {
		return strings.ToLower(d.MgmtIP)
	}
	return "主机 " + strings.ToLower(d.HostIP)
}

func (s *Store) duplicate(d *deviceRecord, extra []*deviceRecord) *deviceRecord {
	for _, list := range [][]*deviceRecord{s.devices, extra} {
		for _, o := range list {
			if o.ID != d.ID && strings.EqualFold(o.Customer, d.Customer) && o.key() == d.key() {
				return o
			}
		}
	}
	return nil
}

// Update 把同一份修改应用到多台设备（编辑一台或批量设置）。
func (s *Store) Update(ids []string, p Patch) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var targets []*deviceRecord
	for _, id := range ids {
		d := s.find(id)
		if d == nil {
			return nil, ErrNotFound
		}
		targets = append(targets, d)
	}
	// 先在副本上改，全部通过校验后再替换，避免改到一半失败。
	updated := make([]*deviceRecord, len(targets))
	for i, d := range targets {
		c := *d
		if c.LastCollect != nil {
			lc := *c.LastCollect
			c.LastCollect = &lc
		}
		if err := s.apply(&c, p); err != nil {
			if len(targets) > 1 {
				return nil, fmt.Errorf("%s：%w", d.label(), err)
			}
			return nil, err
		}
		if c.User != d.User || c.Password != d.Password || c.CredentialID != d.CredentialID {
			// 账号变了，上次的登录结果不再算数。
			c.Status.Login, c.Status.LoginMessage = LoginUnknown, ""
		}
		c.UpdatedAt = s.now()
		updated[i] = &c
	}
	for i, c := range updated {
		if o := s.duplicateAmong(c, updated[:i]); o != nil {
			return nil, fmt.Errorf("修改后客户“%s”下会有两台管理口 IP 为 %s 的设备", orNone(c.Customer), c.key())
		}
	}
	old := make([]deviceRecord, len(targets))
	for i, d := range targets {
		old[i] = *d
		*d = *updated[i]
	}
	if err := s.saveLocked(); err != nil {
		for i, d := range targets {
			*d = old[i]
		}
		return nil, err
	}
	out := make([]Device, len(targets))
	for i, d := range targets {
		out[i] = d.Device
	}
	return out, nil
}

// duplicateAmong 检查修改后的设备是否与库里其他设备或同批修改的设备重复。
func (s *Store) duplicateAmong(d *deviceRecord, batch []*deviceRecord) *deviceRecord {
	for _, o := range s.devices {
		if o.ID == d.ID || slices.ContainsFunc(batch, func(b *deviceRecord) bool { return b.ID == o.ID }) {
			continue
		}
		if strings.EqualFold(o.Customer, d.Customer) && o.key() == d.key() {
			return o
		}
	}
	for _, o := range batch {
		if strings.EqualFold(o.Customer, d.Customer) && o.key() == d.key() {
			return o
		}
	}
	return nil
}

func (d *deviceRecord) label() string {
	return cmp.Or(d.MgmtIP, d.HostIP, d.ID)
}

// Delete 删除设备，返回删除的数量。
func (s *Store) Delete(ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.devices
	s.devices = slices.DeleteFunc(slices.Clone(s.devices), func(d *deviceRecord) bool { return slices.Contains(ids, d.ID) })
	n := len(before) - len(s.devices)
	if n == 0 {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		s.devices = before
		return 0, err
	}
	return n, nil
}

// 字段长度上限（按字符数）。
const (
	maxField    = 128
	maxNotes    = 2000
	maxPassword = 256
)

// apply 校验并应用修改。
func (s *Store) apply(d *deviceRecord, p Patch) error {
	text := []struct {
		dst   *string
		src   *string
		name  string
		limit int
	}{
		{&d.Customer, p.Customer, "客户名称", maxField},
		{&d.Vendor, p.Vendor, "品牌", maxField},
		{&d.Model, p.Model, "型号", maxField},
		{&d.Serial, p.Serial, "序列号", maxField},
		{&d.Hostname, p.Hostname, "主机名", maxField},
		{&d.OS, p.OS, "操作系统", maxField},
		{&d.User, p.User, "管理口账号", maxField},
		{&d.Location, p.Location, "位置", maxField},
		{&d.Notes, p.Notes, "备注", maxNotes},
	}
	for _, f := range text {
		if f.src == nil {
			continue
		}
		v := strings.TrimSpace(*f.src)
		if f.dst != &d.Notes {
			v = strings.Join(strings.Fields(v), " ")
		}
		if utf8.RuneCountInString(v) > f.limit {
			return fmt.Errorf("%s太长（最多 %d 个字）", f.name, f.limit)
		}
		*f.dst = v
	}
	if p.Type != nil {
		t, ok := ParseType(*p.Type)
		if !ok {
			return fmt.Errorf("设备类型 %q 无效，可选：服务器、存储、网络、其他", *p.Type)
		}
		d.Type = t
	}
	for _, f := range []struct {
		dst  *string
		src  *string
		name string
	}{{&d.MgmtIP, p.MgmtIP, "管理口 IP"}, {&d.HostIP, p.HostIP, "主机 IP"}} {
		if f.src == nil {
			continue
		}
		v, err := NormalizeAddress(*f.src)
		if err != nil {
			return fmt.Errorf("%s：%w", f.name, err)
		}
		*f.dst = v
	}
	if p.CredentialID != nil {
		id := strings.TrimSpace(*p.CredentialID)
		if id != "" && s.findCred(id) == nil {
			return errors.New("所选的凭据模板不存在，可能已被删除")
		}
		d.CredentialID = id
	}
	if p.Password != nil {
		if utf8.RuneCountInString(*p.Password) > maxPassword {
			return fmt.Errorf("密码太长（最多 %d 个字符）", maxPassword)
		}
		sealed, err := s.box.Seal(*p.Password)
		if err != nil {
			return err
		}
		d.Password = sealed
		d.HasPassword = sealed != ""
	}
	if p.LegacyTLS != nil {
		d.LegacyTLS = *p.LegacyTLS
	}
	if d.MgmtIP == "" && d.HostIP == "" {
		return errors.New("请至少填写管理口 IP 或主机 IP")
	}
	return nil
}

// ParseType 识别设备类型，接受中英文和“机架服务器”“核心交换机”这样的写法；空值按服务器处理。
func ParseType(s string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch {
	case v == "" || v == TypeServer || strings.Contains(v, "服务器") || v == "srv" || v == "主机":
		return TypeServer, true
	case v == TypeStorage || strings.Contains(v, "存储") || v == "san" || v == "nas":
		return TypeStorage, true
	case v == TypeNetwork || v == "switch" || v == "router" || v == "firewall" ||
		strings.Contains(v, "网络") || strings.Contains(v, "交换") || strings.Contains(v, "路由") || strings.Contains(v, "防火墙"):
		return TypeNetwork, true
	case v == TypeOther || v == "其他" || v == "其它":
		return TypeOther, true
	}
	return "", false
}

// NormalizeAddress 整理管理口或主机地址：去掉空白、从浏览器复制来的 https:// 和末尾的路径。
// 支持 IPv4、IPv6、主机名，可以带端口，例如 10.0.0.10:8443。http:// 开头的地址原样保留。
func NormalizeAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.ContainsAny(s, " \t\r\n\\") {
		return "", fmt.Errorf("%q 里有空格或反斜杠", s)
	}
	keepHTTP := false
	switch lower := strings.ToLower(s); {
	case strings.HasPrefix(lower, "https://"):
		s = s[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		s = s[len("http://"):]
		keepHTTP = true
	case strings.Contains(s, "://"):
		return "", fmt.Errorf("%q 的协议不对，只支持 https", s)
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	host := s
	if ip := net.ParseIP(s); ip == nil {
		u, err := url.Parse("https://" + s)
		if err != nil || u.Hostname() == "" || u.User != nil {
			return "", fmt.Errorf("%q 不是有效的 IP 地址或主机名", s)
		}
		host = u.Hostname()
		if net.ParseIP(host) == nil && !validHostname(host) {
			return "", fmt.Errorf("%q 不是有效的 IP 地址或主机名", s)
		}
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
				return "", fmt.Errorf("%q 的端口无效", s)
			}
		}
	}
	if len(host) > 253 {
		return "", fmt.Errorf("%q 太长", s)
	}
	if keepHTTP {
		return "http://" + s, nil
	}
	return s, nil
}

// validHostname 只认 ASCII 主机名（字母、数字、连字符、下划线、点）。用来挡住填错列的情况，
// 例如把客户名称填进了 IP 列；全是数字的（例如 10.0.0.256）也不算主机名。
func validHostname(h string) bool {
	numeric := true
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range label {
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
				numeric = false
			default:
				return false
			}
		}
	}
	return !numeric
}

// Access 是登录一台设备需要的信息，含明文密码，只在收集和识别时使用。
type Access struct {
	Device   Device
	User     string
	Password string
}

// Access 返回设备的账号和密码；设备绑定了凭据模板时用模板里的账号。
func (s *Store) Access(id string) (Access, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return Access{}, ErrNotFound
	}
	a := Access{Device: d.Device, User: d.User}
	sealed := d.Password
	if d.CredentialID != "" {
		c := s.findCred(d.CredentialID)
		if c == nil {
			return a, errors.New("设备绑定的凭据模板已被删除，请重新设置账号")
		}
		a.User, sealed = c.User, c.Password
	}
	switch {
	case d.MgmtIP == "":
		return a, errors.New("没有填写管理口 IP")
	case a.User == "":
		return a, errors.New("没有填写管理口账号")
	case sealed == "":
		return a, errors.New("没有填写管理口密码")
	}
	plain, err := s.box.Open(sealed)
	if err != nil {
		return a, err
	}
	a.Password = plain
	return a, nil
}

// SetStatus 修改设备的连通状态，稍后写盘。返回修改后的设备。
func (s *Store) SetStatus(id string, fn func(*Status)) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return Device{}, false
	}
	fn(&d.Status)
	s.saveSoon()
	return d.Device, true
}

// Identity 是登录后从设备读到的信息。
type Identity struct {
	Vendor, Model, Serial, Hostname, Firmware string
}

// SetIdentity 用设备上读到的信息回填品牌、型号、序列号和 BMC 固件；主机名只在原来为空时回填。
func (s *Store) SetIdentity(id string, ident Identity) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return Device{}, false
	}
	for _, f := range []struct {
		dst *string
		v   string
	}{
		{&d.Vendor, ident.Vendor}, {&d.Model, ident.Model}, {&d.Serial, ident.Serial}, {&d.Firmware, ident.Firmware},
	} {
		if v := strings.TrimSpace(f.v); v != "" {
			*f.dst = truncate(v, maxField)
		}
	}
	if d.Hostname == "" {
		d.Hostname = truncate(strings.TrimSpace(ident.Hostname), maxField)
	}
	s.saveSoon()
	return d.Device, true
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// SetLastCollect 记录最近一次收集的结果。
func (s *Store) SetLastCollect(id string, lc LastCollect) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.find(id)
	if d == nil {
		return Device{}, false
	}
	d.LastCollect = &lc
	s.saveSoon()
	return d.Device, true
}

// Credentials 返回全部凭据模板（不含密码），按名称排序。
func (s *Store) Credentials() []Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Credential, len(s.creds))
	for i, c := range s.creds {
		out[i] = c.Credential
		out[i].UsedBy = s.usedBy(c.ID)
	}
	slices.SortFunc(out, func(a, b Credential) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s *Store) usedBy(id string) int {
	n := 0
	for _, d := range s.devices {
		if d.CredentialID == id {
			n++
		}
	}
	return n
}

// AddCredential 新建凭据模板。
func (s *Store) AddCredential(p CredentialPatch) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c := &credentialRecord{Credential: Credential{ID: newID(), CreatedAt: now, UpdatedAt: now}}
	if err := s.applyCred(c, p); err != nil {
		return Credential{}, err
	}
	s.creds = append(s.creds, c)
	if err := s.saveLocked(); err != nil {
		s.creds = s.creds[:len(s.creds)-1]
		return Credential{}, err
	}
	return c.Credential, nil
}

// UpdateCredential 修改凭据模板。改了账号或密码时，用它的设备的登录结果会清除。
func (s *Store) UpdateCredential(id string, p CredentialPatch) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.findCred(id)
	if c == nil {
		return Credential{}, ErrNotFound
	}
	old := *c
	if err := s.applyCred(c, p); err != nil {
		*c = old
		return Credential{}, err
	}
	c.UpdatedAt = s.now()
	if c.User != old.User || c.Password != old.Password {
		for _, d := range s.devices {
			if d.CredentialID == id {
				d.Status.Login, d.Status.LoginMessage = LoginUnknown, ""
			}
		}
	}
	if err := s.saveLocked(); err != nil {
		*c = old
		return Credential{}, err
	}
	out := c.Credential
	out.UsedBy = s.usedBy(id)
	return out, nil
}

// DeleteCredential 删除凭据模板，用它的设备改为没有账号。返回受影响的设备数。
func (s *Store) DeleteCredential(id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.creds, func(c *credentialRecord) bool { return c.ID == id })
	if i < 0 {
		return 0, ErrNotFound
	}
	n := 0
	for _, d := range s.devices {
		if d.CredentialID == id {
			d.CredentialID = ""
			n++
		}
	}
	s.creds = slices.Delete(s.creds, i, i+1)
	return n, s.saveLocked()
}

func (s *Store) applyCred(c *credentialRecord, p CredentialPatch) error {
	if p.Name != nil {
		c.Name = strings.Join(strings.Fields(*p.Name), " ")
	}
	if p.User != nil {
		c.User = strings.TrimSpace(*p.User)
	}
	if p.Notes != nil {
		c.Notes = strings.TrimSpace(*p.Notes)
	}
	switch {
	case c.Name == "":
		return errors.New("请填写模板名称")
	case c.User == "":
		return errors.New("请填写账号")
	case utf8.RuneCountInString(c.Name) > maxField || utf8.RuneCountInString(c.User) > maxField:
		return fmt.Errorf("名称或账号太长（最多 %d 个字）", maxField)
	case utf8.RuneCountInString(c.Notes) > maxNotes:
		return fmt.Errorf("备注太长（最多 %d 个字）", maxNotes)
	}
	for _, o := range s.creds {
		if o.ID != c.ID && strings.EqualFold(o.Name, c.Name) {
			return fmt.Errorf("已有名为“%s”的凭据模板", c.Name)
		}
	}
	if p.Password != nil {
		if utf8.RuneCountInString(*p.Password) > maxPassword {
			return fmt.Errorf("密码太长（最多 %d 个字符）", maxPassword)
		}
		sealed, err := s.box.Seal(*p.Password)
		if err != nil {
			return err
		}
		c.Password = sealed
		c.HasPassword = sealed != ""
	}
	return nil
}

// credentialByName 按名称查找凭据模板（不区分大小写）。
func (s *Store) credentialByName(name string) *credentialRecord {
	for _, c := range s.creds {
		if strings.EqualFold(c.Name, strings.TrimSpace(name)) {
			return c
		}
	}
	return nil
}
