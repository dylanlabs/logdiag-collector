// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/dylanlabs/logdiag-collector/internal/xlsx"
)

// column 是设备表导入导出的一列。Names 第一个是导出时用的表头，其余是导入时认的别名。
type column struct {
	key   string
	names []string
	width int
}

// columns 是导出的列，也是导入时认的列。别名覆盖常见的手工表格写法。
var columns = []column{
	{"customer", []string{"客户名称", "客户", "Customer"}, 16},
	{"type", []string{"设备类型", "类型", "Type"}, 10},
	{"vendor", []string{"品牌", "厂商", "厂家", "Vendor", "Brand"}, 12},
	{"model", []string{"型号", "设备型号", "Model"}, 16},
	{"serial", []string{"序列号", "SN", "S/N", "Serial", "Serial number"}, 22},
	{"hostname", []string{"主机名", "Hostname", "Host name"}, 16},
	{"host_ip", []string{"主机IP", "业务IP", "OS IP", "Host IP"}, 16},
	{"os", []string{"操作系统", "系统", "OS"}, 14},
	{"mgmt_ip", []string{"管理口IP", "管理IP", "管理地址", "带外IP", "BMC IP", "BMC", "iBMC IP", "iDRAC IP", "iLO IP", "XCC IP", "IPMI IP", "Management IP", "Mgmt IP"}, 18},
	{"user", []string{"管理口账号", "管理口用户名", "账号", "用户名", "Username", "User"}, 14},
	{"password", []string{"管理口密码", "密码", "Password"}, 14},
	{"credential", []string{"凭据模板", "Credential template"}, 14},
	{"location", []string{"位置", "机房", "机柜", "U位", "Location"}, 16},
	{"notes", []string{"备注", "说明", "Notes", "Remark"}, 24},
}

// normalizeHeader 去掉空白、括号和大小写差异，“管理口 IP（BMC）”和“管理口ip”都能认出来。
func normalizeHeader(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "（("); i > 0 {
		s = s[:i]
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '_', '-', '　', ':', '：':
			return -1
		}
		return r
	}, s)
}

// ExportRows 返回导出用的表格（第一行是表头），不含密码列。ids 为空时导出全部设备。
func (s *Store) ExportRows(ids []string) [][]string {
	list := s.List()
	creds := map[string]string{}
	for _, c := range s.Credentials() {
		creds[c.ID] = c.Name
	}
	var header []string
	for _, c := range columns {
		if c.key != "password" {
			header = append(header, c.names[0])
		}
	}
	rows := [][]string{header}
	for _, d := range list {
		if len(ids) > 0 && !containsID(ids, d.ID) {
			continue
		}
		rows = append(rows, []string{
			d.Customer, TypeNames[d.Type], d.Vendor, d.Model, d.Serial, d.Hostname, d.HostIP, d.OS,
			d.MgmtIP, d.User, creds[d.CredentialID], d.Location, d.Notes,
		})
	}
	return rows
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// TemplateRows 返回导入模板：只有表头。不放示例行，免得忘了删掉被当成设备导入。
func TemplateRows() [][]string {
	var header []string
	for _, c := range columns {
		header = append(header, c.names[0])
	}
	return [][]string{header}
}

// ColumnWidths 返回导出 Excel 时各列的宽度。
func ColumnWidths(withPassword bool) []int {
	var out []int
	for _, c := range columns {
		if c.key != "password" || withPassword {
			out = append(out, c.width)
		}
	}
	return out
}

// WriteCSV 把表格写成 Excel 能直接打开的 CSV（UTF-8 带 BOM、CRLF 换行）。
// 以 = + - @ 开头的单元格前加单引号，防止 Excel 把它当成公式执行。
func WriteCSV(w io.Writer, rows [][]string) error {
	if _, err := w.Write([]byte("\xef\xbb\xbf")); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, v := range row {
			if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
				v = "'" + v
			}
			safe[i] = v
		}
		if err := cw.Write(safe); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// MaxImportBytes 是导入文件的大小上限。
const MaxImportBytes = 20 << 20

// ReadTable 读出 Excel（.xlsx）、CSV 或从 Excel 复制来的制表符文本。
// 文本编码自动判断：UTF-8（可带 BOM）、UTF-16（Excel 的“Unicode 文本”）或 GBK（中文 Windows 上 Excel 默认保存的 CSV）。
func ReadTable(data []byte) ([][]string, error) {
	if len(data) > MaxImportBytes {
		return nil, fmt.Errorf("文件太大（超过 %d MB）", MaxImportBytes>>20)
	}
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return xlsx.Read(bytes.NewReader(data), int64(len(data)))
	}
	if bytes.HasPrefix(data, []byte{0xd0, 0xcf, 0x11, 0xe0}) {
		return nil, xlsx.ErrNotXLSX
	}
	text, err := decodeText(data)
	if err != nil {
		return nil, err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	first, _, _ := strings.Cut(strings.TrimLeft(text, "\n"), "\n")
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	switch {
	case strings.Contains(first, "\t"):
		r.Comma = '\t'
	case !strings.Contains(first, ",") && strings.Contains(first, ";"):
		r.Comma = ';'
	}
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("表格格式不对：%w", err)
	}
	return rows, nil
}

func decodeText(data []byte) (string, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}):
		data = data[3:]
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}), bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		return decodeUTF16(data[2:], data[0] == 0xff), nil
	}
	if utf8.Valid(data) {
		return string(data), nil
	}
	out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
	if err != nil {
		return "", errors.New("无法识别文件编码，请在 Excel 里另存为“CSV UTF-8（逗号分隔）”或 .xlsx")
	}
	return string(out), nil
}

func decodeUTF16(b []byte, little bool) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		if little {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		} else {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		}
	}
	return string(utf16.Decode(u))
}

// 导入的来源格式。
const (
	FormatTable  = "table"  // 本工具导出的表格、模板或用户自己的设备表
	FormatBMCKit = "bmckit" // BMCKit 导出的发现结果
	FormatList   = "list"   // 每行一个地址
)

// ImportRow 是导入预览里的一行。
type ImportRow struct {
	Line       int    `json:"line"`
	Device     Device `json:"device"`
	Credential string `json:"credential,omitempty"` // 表格里写的凭据模板名
	Error      string `json:"error,omitempty"`
	Duplicate  bool   `json:"duplicate,omitempty"`

	patch Patch
}

// ImportPreview 是导入前给用户确认的预览。
type ImportPreview struct {
	Format   string      `json:"format"`
	Columns  []string    `json:"columns"` // 认出的列
	Ignored  []string    `json:"ignored"` // 没认出、会忽略的列
	Rows     []ImportRow `json:"rows"`
	Warnings []string    `json:"warnings,omitempty"`
}

// Ready 返回可以导入的行数。
func (p *ImportPreview) Ready() int {
	n := 0
	for _, r := range p.Rows {
		if r.Error == "" && !r.Duplicate {
			n++
		}
	}
	return n
}

// MaxImportRows 是一次导入的行数上限。
const MaxImportRows = 5000

// PreviewImport 识别表格的格式和列，逐行校验，标出错误和重复，不修改设备库。
func (s *Store) PreviewImport(rows [][]string) (*ImportPreview, error) {
	start := -1
	for i, r := range rows {
		if strings.TrimSpace(strings.Join(r, "")) != "" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, errors.New("表格是空的")
	}
	header := rows[start]
	p := &ImportPreview{}
	var body [][]string
	var parse func([]string) (Patch, string, string)
	switch {
	case isBMCKit(header):
		p.Format = FormatBMCKit
		idx := indexHeader(header)
		parse = func(r []string) (Patch, string, string) { return bmckitRow(r, idx) }
		body = rows[start+1:]
		p.Columns = []string{"管理口 IP（取自“地址或状态”）", "备注（MAC、MAC 厂商、识别线索）"}
	default:
		idx, cols, ignored := mapColumns(header)
		if len(cols) == 0 {
			// 没有表头：当作每行一个地址的清单。
			if _, err := netip.ParseAddr(strings.TrimSpace(header[0])); err != nil {
				return nil, errors.New("没有认出表头。第一行应是“客户名称、管理口IP、管理口账号……”这样的列名，可以先下载导入模板对照")
			}
			p.Format = FormatList
			p.Columns = []string{"管理口 IP"}
			parse = func(r []string) (Patch, string, string) {
				v := firstCell(r)
				return Patch{MgmtIP: &v}, "", ""
			}
			body = rows[start:]
			break
		}
		if len(idx["mgmt_ip"]) == 0 && len(idx["host_ip"]) == 0 {
			return nil, errors.New("没有找到“管理口IP”列（也认“管理IP”“BMC IP”“带外IP”等写法）")
		}
		p.Format = FormatTable
		p.Columns, p.Ignored = cols, ignored
		parse = func(r []string) (Patch, string, string) { return tableRow(r, idx) }
		body = rows[start+1:]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var accepted []*deviceRecord
	skipped := 0
	for i, r := range body {
		if strings.TrimSpace(strings.Join(r, "")) == "" {
			continue
		}
		if len(p.Rows) >= MaxImportRows {
			return nil, fmt.Errorf("一次最多导入 %d 行", MaxImportRows)
		}
		patch, credName, skip := parse(r)
		row := ImportRow{Line: start + i + 1, Credential: credName}
		if p.Format != FormatList {
			row.Line++
		}
		if skip != "" {
			skipped++
			continue
		}
		credErr := ""
		if credName != "" {
			if c := s.credentialByName(credName); c != nil {
				patch.CredentialID = &c.ID
			} else {
				credErr = fmt.Sprintf("凭据模板“%s”不存在，请先在凭据模板里新建", credName)
			}
		}
		d := &deviceRecord{Device: Device{Type: TypeServer}}
		switch err := s.applyPreview(d, patch); {
		case err != nil:
			row.Error = err.Error()
		case credErr != "":
			row.Error = credErr
		case s.duplicate(d, accepted) != nil:
			row.Duplicate = true
		default:
			accepted = append(accepted, d)
		}
		row.Device = d.Device
		row.patch = patch
		p.Rows = append(p.Rows, row)
	}
	if len(p.Rows) == 0 {
		return nil, errors.New("表格里没有设备")
	}
	if skipped > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("跳过了 %d 行：本机、疑似交换机或路由器，以及没有 IP 地址的记录。", skipped))
	}
	if p.Format == FormatBMCKit {
		p.Warnings = append(p.Warnings, "BMCKit 的发现结果没有客户名称和账号：可以在下面填默认客户名称、选凭据模板；导入后也可以多选设备用“批量设置”补上。")
	}
	return p, nil
}

// applyPreview 校验一行，但不加密密码（预览阶段不需要），也不保存。
func (s *Store) applyPreview(d *deviceRecord, p Patch) error {
	pw := p.Password
	p.Password = nil
	if err := s.apply(d, p); err != nil {
		return err
	}
	if pw != nil && *pw != "" {
		d.HasPassword = true
	}
	return nil
}

// CommitImport 导入预览里没有错误、不重复的行。客户为空的行用 defaultCustomer；
// 没有账号的行绑定 credentialID（可空）。返回新建的设备。
func (s *Store) CommitImport(p *ImportPreview, defaultCustomer, credentialID string) ([]Device, error) {
	var patches []Patch
	for _, r := range p.Rows {
		if r.Error != "" || r.Duplicate {
			continue
		}
		patch := r.patch
		if r.Device.Customer == "" && strings.TrimSpace(defaultCustomer) != "" {
			v := defaultCustomer
			patch.Customer = &v
		}
		if credentialID != "" && r.Device.User == "" && patch.CredentialID == nil {
			v := credentialID
			patch.CredentialID = &v
		}
		patches = append(patches, patch)
	}
	if len(patches) == 0 {
		return nil, errors.New("没有可以导入的设备")
	}
	return s.AddMany(patches, true)
}

func firstCell(r []string) string {
	if len(r) == 0 {
		return ""
	}
	return strings.TrimSpace(r[0])
}

// mapColumns 按表头认列，返回每个字段所在的列号（没有时不在 map 里）。
// “位置”可以由多列组成（机房、机柜、U位），导入时用空格连起来。
func mapColumns(header []string) (map[string][]int, []string, []string) {
	idx := map[string][]int{}
	var found, ignored []string
	for i, h := range header {
		n := normalizeHeader(h)
		if n == "" {
			continue
		}
		matched := false
		for _, c := range columns {
			if len(idx[c.key]) > 0 && c.key != "location" {
				continue
			}
			for _, name := range c.names {
				if n == normalizeHeader(name) {
					idx[c.key] = append(idx[c.key], i)
					found = append(found, strings.TrimSpace(h))
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			ignored = append(ignored, strings.TrimSpace(h))
		}
	}
	return idx, found, ignored
}

func cell(r []string, i int) string {
	if i < 0 || i >= len(r) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(r[i], "'"))
}

func tableRow(r []string, idx map[string][]int) (Patch, string, string) {
	var p Patch
	get := func(key string) (string, bool) {
		cols := idx[key]
		if len(cols) == 0 {
			return "", false
		}
		var parts []string
		for _, i := range cols {
			if v := cell(r, i); v != "" {
				parts = append(parts, v)
			}
		}
		return strings.Join(parts, " "), true
	}
	for key, dst := range map[string]**string{
		"customer": &p.Customer, "type": &p.Type, "vendor": &p.Vendor, "model": &p.Model,
		"serial": &p.Serial, "hostname": &p.Hostname, "host_ip": &p.HostIP, "os": &p.OS,
		"mgmt_ip": &p.MgmtIP, "user": &p.User, "location": &p.Location, "notes": &p.Notes,
	} {
		if v, ok := get(key); ok {
			*dst = &v
		}
	}
	if cols := idx["password"]; len(cols) > 0 && cols[0] < len(r) && r[cols[0]] != "" {
		v := r[cols[0]] // 密码不去空白：空格也可能是密码的一部分
		p.Password = &v
	}
	credential, _ := get("credential")
	return p, credential, ""
}

// BMCKit 发现结果的列（中英文表头都可能出现，取决于导出时的界面语言）。
var bmckitColumns = map[string][]string{
	"addr":     {"地址或状态", "地址/状态", "Address or state", "Address / state"},
	"mac":      {"MAC"},
	"role":     {"设备角色", "Role"},
	"local":    {"是否本机", "This laptop"},
	"vendor":   {"MAC注册厂商", "MAC vendor"},
	"identity": {"识别线索", "Identity hints"},
}

func isBMCKit(header []string) bool {
	return indexHeader(header)["addr"] >= 0
}

func indexHeader(header []string) map[string]int {
	idx := map[string]int{}
	for key, names := range bmckitColumns {
		idx[key] = -1
		for i, h := range header {
			for _, n := range names {
				if normalizeHeader(h) == normalizeHeader(n) {
					idx[key] = i
				}
			}
		}
	}
	return idx
}

// bmckitRow 只取有 IP 的行，跳过本机和疑似交换机、路由器；MAC 和识别线索写进备注。
func bmckitRow(r []string, idx map[string]int) (Patch, string, string) {
	addr := cell(r, idx["addr"])
	if _, err := netip.ParseAddr(addr); err != nil {
		return Patch{}, "", "不是 IP"
	}
	switch strings.ToLower(cell(r, idx["local"])) {
	case "是", "yes", "true":
		return Patch{}, "", "本机"
	}
	role := strings.ToLower(cell(r, idx["role"]))
	for _, kw := range []string{"交换机", "路由器", "switch", "router"} {
		if strings.Contains(role, kw) {
			return Patch{}, "", "网络设备"
		}
	}
	var notes []string
	if v := cell(r, idx["mac"]); v != "" {
		notes = append(notes, "MAC "+v)
	}
	if v := cell(r, idx["vendor"]); v != "" {
		notes = append(notes, "MAC 厂商 "+v)
	}
	if v := cell(r, idx["identity"]); v != "" {
		notes = append(notes, v)
	}
	note := ""
	if len(notes) > 0 {
		note = "BMCKit 发现：" + strings.Join(notes, "；")
	}
	vendor := guessVendor(cell(r, idx["vendor"]) + " " + cell(r, idx["identity"]))
	return Patch{MgmtIP: &addr, Notes: &note, Vendor: &vendor}, "", ""
}

// guessVendor 从 MAC 厂商和识别线索里猜品牌，猜不出时为空，登录识别后会被准确值覆盖。
func guessVendor(text string) string {
	t := strings.ToLower(text)
	for _, v := range []struct{ kw, vendor string }{
		{"xfusion", "xFusion"}, {"huawei", "Huawei"}, {"inspur", "Inspur"}, {"ieit", "Inspur"},
		{"lenovo", "Lenovo"}, {"dell", "Dell"}, {"hewlett", "HPE"}, {"hpe", "HPE"}, {"h3c", "H3C"},
		{"ibm", "IBM"}, {"supermicro", "Supermicro"}, {"sugon", "Sugon"}, {"zte", "ZTE"},
	} {
		if strings.Contains(t, v.kw) {
			return v.vendor
		}
	}
	return ""
}
