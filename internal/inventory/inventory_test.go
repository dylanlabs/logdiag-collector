// SPDX-License-Identifier: GPL-3.0-or-later

package inventory

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dylanlabs/logdiag-collector/internal/secret"
	"github.com/dylanlabs/logdiag-collector/internal/xlsx"
)

func ptr[T any](v T) *T { return &v }

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	box, err := secret.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, warn, err := Open(dir, box)
	if err != nil || warn != "" {
		t.Fatalf("Open: %v %s", err, warn)
	}
	return s
}

func TestAddUpdateDeletePersist(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	d, err := s.Add(Patch{
		Customer: ptr(" 示例客户 "), MgmtIP: ptr("https://10.0.0.10/login.html"), User: ptr("admin"),
		Password: ptr("s3cret!"), Type: ptr("机架服务器"), Notes: ptr("第一行\n第二行"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Customer != "示例客户" || d.MgmtIP != "10.0.0.10" || d.Type != TypeServer || !d.HasPassword || d.Notes != "第一行\n第二行" {
		t.Errorf("新建的设备不对：%+v", d)
	}
	if _, err := s.Add(Patch{Customer: ptr("示例客户"), MgmtIP: ptr("10.0.0.10")}); err == nil {
		t.Error("同一客户下重复的管理口 IP 应拒绝")
	}
	if _, err := s.Add(Patch{Customer: ptr("另一个客户"), MgmtIP: ptr("10.0.0.10")}); err != nil {
		t.Errorf("不同客户可以有相同的 IP：%v", err)
	}
	if _, err := s.Add(Patch{Customer: ptr("示例客户")}); err == nil {
		t.Error("没有任何 IP 应拒绝")
	}

	data, _ := os.ReadFile(filepath.Join(dir, "devices.json"))
	if bytes.Contains(data, []byte("s3cret!")) {
		t.Fatal("devices.json 里出现了明文密码")
	}

	a, err := s.Access(d.ID)
	if err != nil || a.User != "admin" || a.Password != "s3cret!" {
		t.Fatalf("Access = %+v, %v", a, err)
	}

	s.SetStatus(d.ID, func(st *Status) { st.Login = LoginAuthFailed })
	up, err := s.Update([]string{d.ID}, Patch{Password: ptr("n3w")})
	if err != nil {
		t.Fatal(err)
	}
	if up[0].Status.Login != LoginUnknown {
		t.Error("改密码后应清除认证失败状态")
	}
	s.SetStatus(d.ID, func(st *Status) { st.Login = LoginOK })
	up, _ = s.Update([]string{d.ID}, Patch{Notes: ptr("只改备注"), User: ptr("admin")})
	if up[0].Status.Login != LoginOK {
		t.Error("账号没变时不应清除登录状态")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openStore(t, dir)
	if n := len(s2.List()); n != 2 {
		t.Fatalf("重新打开后有 %d 台设备，期望 2", n)
	}
	a, err = s2.Access(d.ID)
	if err != nil || a.Password != "n3w" {
		t.Errorf("重新打开后 Access = %+v, %v", a, err)
	}
	if n, err := s2.Delete([]string{d.ID, "missing"}); n != 1 || err != nil {
		t.Errorf("Delete = %d, %v", n, err)
	}
}

func TestUpdateRejectsDuplicates(t *testing.T) {
	s := openStore(t, t.TempDir())
	a, _ := s.Add(Patch{Customer: ptr("甲"), MgmtIP: ptr("10.0.0.1")})
	b, _ := s.Add(Patch{Customer: ptr("乙"), MgmtIP: ptr("10.0.0.1")})
	if _, err := s.Update([]string{b.ID}, Patch{Customer: ptr("甲")}); err == nil {
		t.Error("改成同一客户后 IP 重复，应拒绝")
	}
	if _, err := s.Update([]string{a.ID, b.ID}, Patch{Customer: ptr("丙")}); err == nil {
		t.Error("批量改客户后两台 IP 重复，应拒绝")
	}
	if d, _ := s.Get(b.ID); d.Customer != "乙" {
		t.Error("失败的修改不应生效")
	}
}

func TestCredentials(t *testing.T) {
	s := openStore(t, t.TempDir())
	c, err := s.AddCredential(CredentialPatch{Name: ptr("机房A"), User: ptr("Administrator"), Password: ptr("Admin@9000")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCredential(CredentialPatch{Name: ptr("机房a"), User: ptr("root")}); err == nil {
		t.Error("重名的模板应拒绝")
	}
	d, _ := s.Add(Patch{MgmtIP: ptr("10.0.0.20"), User: ptr("ignored"), CredentialID: ptr(c.ID)})
	a, err := s.Access(d.ID)
	if err != nil || a.User != "Administrator" || a.Password != "Admin@9000" {
		t.Fatalf("模板账号 Access = %+v, %v", a, err)
	}
	if got := s.Credentials()[0].UsedBy; got != 1 {
		t.Errorf("UsedBy = %d", got)
	}
	s.SetStatus(d.ID, func(st *Status) { st.Login = LoginAuthFailed })
	if _, err := s.UpdateCredential(c.ID, CredentialPatch{Password: ptr("new")}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(d.ID); d.Status.Login != LoginUnknown {
		t.Error("模板改密码后，用它的设备应清除认证失败状态")
	}
	if n, err := s.DeleteCredential(c.ID); n != 1 || err != nil {
		t.Errorf("DeleteCredential = %d, %v", n, err)
	}
	if _, err := s.Access(d.ID); err == nil {
		t.Error("模板删除后设备没有密码，Access 应报错")
	}
}

func TestNormalizeAddress(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.10": "10.0.0.10", " https://10.0.0.10:8443/ui ": "10.0.0.10:8443", "fe80::1": "fe80::1",
		"[fe80::1]:443": "[fe80::1]:443", "bmc-01.example.com": "bmc-01.example.com", "http://10.0.0.9": "http://10.0.0.9", "": "",
	} {
		if got, err := NormalizeAddress(in); err != nil || got != want {
			t.Errorf("NormalizeAddress(%q) = %q, %v，期望 %q", in, got, err, want)
		}
	}
	for _, in := range []string{"10.0.0.1 10.0.0.2", `\\server`, "ftp://10.0.0.1", "10.0.0.1:99999", "user@10.0.0.1", "不是地址", "10.0.0.256", "bmc..lab"} {
		if got, err := NormalizeAddress(in); err == nil {
			t.Errorf("NormalizeAddress(%q) = %q，应报错", in, got)
		}
	}
}

func TestExpandAddresses(t *testing.T) {
	got, err := ExpandAddresses("10.0.0.10-12\n10.0.0.11，bmc-01.example.com; 192.168.1.0/30 10.0.1.1-10.0.1.2")
	want := []string{"10.0.0.10", "10.0.0.11", "10.0.0.12", "bmc-01.example.com", "192.168.1.1", "192.168.1.2", "10.0.1.1", "10.0.1.2"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandAddresses = %v, %v\n期望 %v", got, err, want)
	}
	for _, in := range []string{"", "10.0.0.50-10", "10.0.0.0/8", "10.0.0.1-10.0.9.255"} {
		if _, err := ExpandAddresses(in); err == nil {
			t.Errorf("ExpandAddresses(%q) 应报错", in)
		}
	}
}

func TestImportTable(t *testing.T) {
	s := openStore(t, t.TempDir())
	s.AddCredential(CredentialPatch{Name: ptr("机房A"), User: ptr("root"), Password: ptr("x")})
	s.Add(Patch{Customer: ptr("示例客户"), MgmtIP: ptr("10.0.0.1")})
	// 用户自己的表格：列名写法不同、多出一列、机房和机柜分两列。
	csv := "客户名称,品牌,管理 IP,管理口用户名,管理口密码,系统,机房,机柜,资产编号,凭据模板\r\n" +
		"示例客户,Huawei,10.0.0.1,admin,pw,,,,A-1,\r\n" + // 与已有设备重复
		"示例客户,Huawei,10.0.0.2,admin, pw ,CentOS 7,3楼,A01,A-2,\r\n" +
		",Inspur,not an ip,admin,pw,,,,A-3,\r\n" +
		"示例客户,,10.0.0.3,,,,,,A-4,机房A\r\n" +
		"示例客户,,10.0.0.4,,,,,,A-5,不存在的模板\r\n" +
		",,,,,,,,,\r\n"
	rows, err := ReadTable([]byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PreviewImport(rows)
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatTable || !reflect.DeepEqual(p.Ignored, []string{"资产编号"}) {
		t.Errorf("格式 %s，忽略的列 %v", p.Format, p.Ignored)
	}
	if len(p.Rows) != 5 || !p.Rows[0].Duplicate || p.Rows[2].Error == "" || p.Rows[4].Error == "" || p.Ready() != 2 {
		t.Fatalf("预览不对：%+v", p.Rows)
	}
	if r := p.Rows[1]; r.Line != 3 || r.Device.Location != "3楼 A01" || !r.Device.HasPassword || r.Device.OS != "CentOS 7" {
		t.Errorf("第 3 行：%+v", r)
	}
	added, err := s.CommitImport(p, "默认客户", "")
	if err != nil || len(added) != 2 {
		t.Fatalf("CommitImport = %v, %v", added, err)
	}
	a, err := s.Access(added[0].ID)
	if err != nil || a.Password != " pw " {
		t.Errorf("导入的密码 = %q, %v（密码不应去掉空格）", a.Password, err)
	}
	if a, err := s.Access(added[1].ID); err != nil || a.User != "root" {
		t.Errorf("按名称绑定的凭据模板：%+v, %v", a, err)
	}
}

func TestImportBMCKit(t *testing.T) {
	s := openStore(t, t.TempDir())
	csv := "\xef\xbb\xbf地址或状态,MAC,设备角色,VLAN,依据,可信程度,掩码,DHCP或附注,观测次数,是否本机,MAC注册厂商,识别线索\r\n" +
		`"192.168.1.100","70:7B:E8:00:00:01","地址线索","未见标签","ARP","源地址已观测","","同一报文可能被记录多次",3,"否","Huawei Technologies Co.,Ltd","Redfish：iBMC"` + "\r\n" +
		`"192.168.1.10","00:11:22:33:44:55","地址线索","未见标签","ARP","源地址已观测","","",1,"是","",""` + "\r\n" +
		`"仅 MAC","02:00:00:00:00:10","疑似交换机","未见标签","LLDP","","","",1,"否","",""` + "\r\n"
	rows, err := ReadTable([]byte(csv))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PreviewImport(rows)
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatBMCKit || len(p.Rows) != 1 || len(p.Warnings) != 2 {
		t.Fatalf("预览不对：%+v", p)
	}
	d := p.Rows[0].Device
	if d.MgmtIP != "192.168.1.100" || d.Vendor != "Huawei" || !strings.Contains(d.Notes, "Redfish：iBMC") {
		t.Errorf("BMCKit 行：%+v", d)
	}
	added, err := s.CommitImport(p, "新客户", "")
	if err != nil || added[0].Customer != "新客户" {
		t.Errorf("CommitImport = %+v, %v", added, err)
	}
}

func TestImportListAndEncodings(t *testing.T) {
	s := openStore(t, t.TempDir())
	// GBK 编码的 CSV（中文 Windows 上 Excel 默认的保存方式）：“客户名称,管理口IP”。
	gbk := []byte{0xbf, 0xcd, 0xbb, 0xa7, 0xc3, 0xfb, 0xb3, 0xc6, ',', 0xb9, 0xdc, 0xc0, 0xed, 0xbf, 0xda, 'I', 'P', '\r', '\n'}
	gbk = append(gbk, []byte{0xbc, 0xd7, ',', '1', '0', '.', '0', '.', '0', '.', '7', '\r', '\n'}...)
	rows, err := ReadTable(gbk)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"客户名称", "管理口IP"}, {"甲", "10.0.0.7"}}; !reflect.DeepEqual(rows, want) {
		t.Errorf("GBK 解码 = %q", rows)
	}
	// 从 Excel 复制的两列（制表符分隔）。
	rows, _ = ReadTable([]byte("管理口IP\t客户\n10.0.0.8\t乙\n"))
	if p, err := s.PreviewImport(rows); err != nil || p.Rows[0].Device.Customer != "乙" {
		t.Errorf("制表符表格：%+v, %v", p, err)
	}
	// 没有表头，每行一个地址。
	rows, _ = ReadTable([]byte("10.0.0.21\n10.0.0.22\n"))
	p, err := s.PreviewImport(rows)
	if err != nil || p.Format != FormatList || len(p.Rows) != 2 || p.Rows[1].Line != 2 {
		t.Errorf("地址清单：%+v, %v", p, err)
	}
	if _, err := s.PreviewImport([][]string{{"名称", "备注"}}); err == nil {
		t.Error("认不出表头时应报错")
	}
}

func TestExport(t *testing.T) {
	s := openStore(t, t.TempDir())
	c, _ := s.AddCredential(CredentialPatch{Name: ptr("模板"), User: ptr("root"), Password: ptr("x")})
	s.Add(Patch{Customer: ptr("甲"), MgmtIP: ptr("10.0.0.10"), User: ptr("admin"), Password: ptr("secret-pw"), Notes: ptr("=cmd")})
	s.Add(Patch{Customer: ptr("甲"), MgmtIP: ptr("10.0.0.9"), CredentialID: ptr(c.ID)})
	rows := s.ExportRows(nil)
	if len(rows) != 3 || rows[1][8] != "10.0.0.9" || rows[1][10] != "模板" {
		t.Fatalf("导出行：%q", rows)
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "secret-pw") || !strings.Contains(out, "'=cmd") || !strings.HasPrefix(out, "\xef\xbb\xbf") {
		t.Errorf("CSV 不对：%s", out)
	}
	// 导出的表格能原样导回（换一个空库）。
	var xb bytes.Buffer
	if err := xlsx.Write(&xb, "设备列表", rows, ColumnWidths(false)); err != nil {
		t.Fatal(err)
	}
	back, err := ReadTable(xb.Bytes())
	if err != nil || len(back) != len(rows) || back[2][12] != "=cmd" || back[1][10] != "模板" {
		t.Fatalf("xlsx 读回 %q, %v", back, err)
	}
	s2 := openStore(t, t.TempDir())
	p, err := s2.PreviewImport(back)
	if err != nil || p.Ready() != 1 || len(p.Ignored) != 0 {
		t.Fatalf("导出的表格导回：%+v, %v", p, err)
	}
	if r := p.Rows[0]; r.Error == "" || r.Device.MgmtIP != "10.0.0.9" {
		t.Errorf("新库里没有“模板”这个凭据模板，应报错并仍显示设备信息：%+v", r)
	}
}

func TestOSCredentialsAndHostKey(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	d, err := s.Add(Patch{
		Customer: ptr("客户"), HostIP: ptr("10.0.0.20"), Type: ptr("server"),
		OSUser: ptr("ops"), OSPassword: ptr("os-s3cret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasOSPassword || d.OSUser != "ops" {
		t.Errorf("OS 账号没保存：%+v", d)
	}
	// 明文 OS 密码不落盘、不进 List。
	if data, _ := os.ReadFile(filepath.Join(dir, "devices.json")); bytes.Contains(data, []byte("os-s3cret")) {
		t.Fatal("devices.json 里出现了明文 OS 密码")
	}
	a, err := s.OSAccess(d.ID)
	if err != nil || a.User != "ops" || a.Password != "os-s3cret" {
		t.Fatalf("OSAccess = %+v，err=%v", a, err)
	}

	// 记录主机密钥：首次写入，之后不覆盖。
	s.SetSSHHostKey(d.ID, "256 SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKK root@h (ED25519)")
	got, _ := s.Get(d.ID)
	if got.SSHHostKey != "SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKK" {
		t.Errorf("主机密钥没有规整保存：%q", got.SSHHostKey)
	}
	s.SetSSHHostKey(d.ID, "SHA256:ZZZZ")
	if got, _ := s.Get(d.ID); got.SSHHostKey != "SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKK" {
		t.Error("已有主机密钥不应被覆盖")
	}

	// 改了主机 IP（服务器的 SSH 地址）后，记录的主机密钥自动清除。
	if _, err := s.Update([]string{d.ID}, Patch{HostIP: ptr("10.0.0.21")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(d.ID); got.SSHHostKey != "" {
		t.Errorf("改了主机 IP 应清除主机密钥，仍是 %q", got.SSHHostKey)
	}

	// 对服务器来说，改管理口 IP 不影响 SSH 主机密钥。
	s.SetSSHHostKey(d.ID, "SHA256:KEEPKEEPKEEP")
	if _, err := s.Update([]string{d.ID}, Patch{MgmtIP: ptr("10.1.0.9")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(d.ID); got.SSHHostKey != "SHA256:KEEPKEEPKEEP" {
		t.Errorf("服务器改管理口 IP 不应清除 SSH 主机密钥，却变成 %q", got.SSHHostKey)
	}

	// 清除 OS 密码。
	if _, err := s.Update([]string{d.ID}, Patch{OSPassword: ptr("")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(d.ID); got.HasOSPassword {
		t.Error("传空字符串应清除 OS 密码")
	}
}
