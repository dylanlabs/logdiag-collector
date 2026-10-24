// SPDX-License-Identifier: GPL-3.0-or-later

package xlsx

import (
	"archive/zip"
	"bytes"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	rows := [][]string{
		{"客户名称", "管理口IP", "序列号"},
		{"示例客户", "10.0.0.10", "2102311SYNTH0001"},
		{"", "fe80::1", "<&>\"'"},
		{"带控制字符\x01", "", "最后"},
	}
	var buf bytes.Buffer
	if err := Write(&buf, "设备列表", rows, []int{16, 18, 20}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{rows[0], rows[1], rows[2], {"带控制字符", "", "最后"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("读回 %q，期望 %q", got, want)
	}
}

// excelLike 按 Excel 的写法组装工作簿：共享字符串、富文本、数字、跳过的单元格和空行。
func excelLike(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="服务器" sheetId="3" r:id="rId7"/><sheet name="其他" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId7" Type="worksheet" Target="/xl/worksheets/sheet3.xml"/></Relationships>`,
		"xl/sharedStrings.xml": `<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<si><t>管理IP</t></si><si><r><t>客户</t></r><r><rPr><b/></rPr><t>名称</t></r></si><si><t>示例客户</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>错的表</t></is></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet3.xml": `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
			`<row r="1"><c r="A1" t="s"><v>1</v></c><c r="C1" t="s"><v>0</v></c></row>` +
			`<row r="3"><c r="A3" t="s"><v>2</v></c><c r="B3"><v>2.102311E+16</v></c><c r="C3" t="str"><v>10.0.0.10</v></c><c r="D3"/></row>` +
			`</sheetData></worksheet>`,
	}
	for name, content := range files {
		f, _ := z.Create(name)
		f.Write([]byte(content))
	}
	z.Close()
	return buf.Bytes()
}

func TestReadExcelStyle(t *testing.T) {
	data := excelLike(t)
	got, err := Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"客户名称", "", "管理IP"}, nil, {"示例客户", "21023110000000000", "10.0.0.10"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("读到 %q，期望 %q", got, want)
	}
}

func TestReadNotXLSX(t *testing.T) {
	data := []byte("客户名称,管理口IP\n")
	if _, err := Read(bytes.NewReader(data), int64(len(data))); err != ErrNotXLSX {
		t.Errorf("CSV 应返回 ErrNotXLSX，得到 %v", err)
	}
}

func TestColumn(t *testing.T) {
	for ref, want := range map[string]int{"A1": 0, "Z9": 25, "AA1": 26, "ab12": 27, "1": -1} {
		if got := column(ref); got != want {
			t.Errorf("column(%q) = %d，期望 %d", ref, got, want)
		}
	}
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 701: "ZZ", 702: "AAA"} {
		if got := colName(i); got != want {
			t.Errorf("colName(%d) = %q，期望 %q", i, got, want)
		}
	}
}
