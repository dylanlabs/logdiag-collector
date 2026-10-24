// SPDX-License-Identifier: GPL-3.0-or-later

// Package xlsx 读写最简单的 Excel 工作簿：只有文字的一张表。
//
// 设备表的导入导出只需要这些，不必为此引入完整的 Excel 库。
// 读取时只取第一张工作表的单元格文字，忽略公式、样式和合并单元格。
package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// 读取的上限，防止异常文件（例如压缩炸弹）占满内存。
const (
	maxPart = 64 << 20
	maxRows = 100000
	maxCols = 200
)

// ErrNotXLSX 表示文件不是 .xlsx 工作簿（例如老式的 .xls）。
var ErrNotXLSX = errors.New("不是 Excel 工作簿（.xlsx）；老式的 .xls 请在 Excel 里另存为 .xlsx 或 CSV")

// Read 读出第一张工作表的全部单元格文字，每行去掉末尾的空单元格。
func Read(r io.ReaderAt, size int64) ([][]string, error) {
	z, err := zip.NewReader(r, size)
	if err != nil {
		return nil, ErrNotXLSX
	}
	files := make(map[string]*zip.File, len(z.File))
	for _, f := range z.File {
		files[strings.TrimPrefix(f.Name, "/")] = f
	}
	sheet, err := firstSheet(files)
	if err != nil {
		return nil, err
	}
	var shared []string
	if f := files["xl/sharedStrings.xml"]; f != nil {
		if shared, err = readShared(f); err != nil {
			return nil, err
		}
	}
	f := files[sheet]
	if f == nil {
		return nil, fmt.Errorf("工作簿里找不到工作表 %s", sheet)
	}
	return readSheet(f, shared)
}

func open(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxPart+1))
	if err != nil {
		return nil, fmt.Errorf("读取 %s：%w", f.Name, err)
	}
	if len(data) > maxPart {
		return nil, fmt.Errorf("%s 太大", f.Name)
	}
	return data, nil
}

// firstSheet 按 workbook.xml 和它的关系文件找到第一张工作表的路径。
func firstSheet(files map[string]*zip.File) (string, error) {
	wb := files["xl/workbook.xml"]
	if wb == nil {
		return "", ErrNotXLSX
	}
	data, err := open(wb)
	if err != nil {
		return "", err
	}
	var book struct {
		Sheets []struct {
			ID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(data, &book); err != nil {
		return "", fmt.Errorf("读取 workbook.xml：%w", err)
	}
	if len(book.Sheets) == 0 {
		return "", errors.New("工作簿里没有工作表")
	}
	if rels := files["xl/_rels/workbook.xml.rels"]; rels != nil {
		data, err := open(rels)
		if err != nil {
			return "", err
		}
		var r struct {
			Rels []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		if err := xml.Unmarshal(data, &r); err != nil {
			return "", fmt.Errorf("读取 workbook.xml.rels：%w", err)
		}
		for _, rel := range r.Rels {
			if rel.ID == book.Sheets[0].ID {
				if strings.HasPrefix(rel.Target, "/") {
					return strings.TrimPrefix(rel.Target, "/"), nil
				}
				return path.Clean("xl/" + rel.Target), nil
			}
		}
	}
	return "xl/worksheets/sheet1.xml", nil
}

// richText 是 <si> 或 <is>：要么直接有 <t>，要么由多段 <r><t> 拼成。
type richText struct {
	T    string `xml:"t"`
	Runs []struct {
		T string `xml:"t"`
	} `xml:"r"`
}

func (r richText) String() string {
	if len(r.Runs) == 0 {
		return r.T
	}
	var b strings.Builder
	b.WriteString(r.T)
	for _, run := range r.Runs {
		b.WriteString(run.T)
	}
	return b.String()
}

func readShared(f *zip.File) ([]string, error) {
	data, err := open(f)
	if err != nil {
		return nil, err
	}
	var sst struct {
		Items []richText `xml:"si"`
	}
	if err := xml.Unmarshal(data, &sst); err != nil {
		return nil, fmt.Errorf("读取 sharedStrings.xml：%w", err)
	}
	out := make([]string, len(sst.Items))
	for i, it := range sst.Items {
		out[i] = it.String()
	}
	return out, nil
}

func readSheet(f *zip.File, shared []string) ([][]string, error) {
	data, err := open(f)
	if err != nil {
		return nil, err
	}
	var ws struct {
		Rows []struct {
			R     int `xml:"r,attr"`
			Cells []struct {
				Ref    string   `xml:"r,attr"`
				Type   string   `xml:"t,attr"`
				Value  string   `xml:"v"`
				Inline richText `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(data, &ws); err != nil {
		return nil, fmt.Errorf("读取工作表：%w", err)
	}
	var out [][]string
	for i, row := range ws.Rows {
		r := row.R
		if r <= 0 {
			r = i + 1
		}
		if r > maxRows {
			return nil, fmt.Errorf("工作表超过 %d 行", maxRows)
		}
		for len(out) < r {
			out = append(out, nil)
		}
		var cells []string
		for j, c := range row.Cells {
			col := j
			if c.Ref != "" {
				if col = column(c.Ref); col < 0 {
					return nil, fmt.Errorf("单元格引用 %q 无效", c.Ref)
				}
			}
			if col >= maxCols {
				continue
			}
			var v string
			switch c.Type {
			case "s":
				n, err := strconv.Atoi(strings.TrimSpace(c.Value))
				if err != nil || n < 0 || n >= len(shared) {
					return nil, fmt.Errorf("单元格 %s 引用的共享字符串不存在", c.Ref)
				}
				v = shared[n]
			case "inlineStr":
				v = c.Inline.String()
			case "n", "":
				v = number(c.Value)
			default: // str（公式结果）、b、e
				v = c.Value
			}
			for len(cells) <= col {
				cells = append(cells, "")
			}
			cells[col] = v
		}
		for len(cells) > 0 && strings.TrimSpace(cells[len(cells)-1]) == "" {
			cells = cells[:len(cells)-1]
		}
		out[r-1] = cells
	}
	return out, nil
}

// column 把 "AB12" 这样的引用换成从 0 开始的列号。
func column(ref string) int {
	n := 0
	i := 0
	for ; i < len(ref); i++ {
		c := ref[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A'+1)
		if n > 1<<20 {
			return -1
		}
	}
	if i == 0 {
		return -1
	}
	return n - 1
}

// number 把科学计数法的整数还原成普通写法，例如序列号 2.102311E+16。
func number(v string) string {
	v = strings.TrimSpace(v)
	if !strings.ContainsAny(v, "eE") {
		return v
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f != float64(int64(f)) {
		return v
	}
	return strconv.FormatInt(int64(f), 10)
}

// Write 把 rows 写成只有一张工作表的 .xlsx，第一行加粗并冻结，所有单元格按文字保存
// （IP、序列号不会被 Excel 改成数字或日期）。
func Write(w io.Writer, sheetName string, rows [][]string, widths []int) error {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	add := func(name, content string) error {
		f, err := z.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, content)
		return err
	}
	parts := [][2]string{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rootRels},
		{"xl/workbook.xml", fmt.Sprintf(workbook, escape(sheetName))},
		{"xl/_rels/workbook.xml.rels", workbookRels},
		{"xl/styles.xml", styles},
		{"xl/worksheets/sheet1.xml", sheet(rows, widths)},
	}
	for _, p := range parts {
		if err := add(p[0], p[1]); err != nil {
			return err
		}
	}
	if err := z.Close(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func sheet(rows [][]string, widths []int) string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	b.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
	if len(widths) > 0 {
		b.WriteString("<cols>")
		for i, w := range widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, i+1, i+1, w)
		}
		b.WriteString("</cols>")
	}
	b.WriteString("<sheetData>")
	for i, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, i+1)
		for j, v := range row {
			style := ""
			if i == 0 {
				style = ` s="1"`
			}
			fmt.Fprintf(&b, `<c r="%s%d" t="inlineStr"%s><is><t xml:space="preserve">%s</t></is></c>`, colName(j), i+1, style, escape(v))
		}
		b.WriteString("</row>")
	}
	b.WriteString("</sheetData></worksheet>")
	return b.String()
}

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func escape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		// XML 1.0 不允许大部分控制字符，Excel 遇到会报文件损坏。
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			continue
		}
		xml.EscapeText(&b, []byte(string(r)))
	}
	return b.String()
}

const contentTypes = xml.Header + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
	`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
	`<Default Extension="xml" ContentType="application/xml"/>` +
	`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
	`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
	`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
	`</Types>`

const rootRels = xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`</Relationships>`

const workbook = xml.Header + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
	`<sheets><sheet name="%s" sheetId="1" r:id="rId1"/></sheets></workbook>`

const workbookRels = xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
	`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
	`</Relationships>`

// styles 有两种单元格格式：0 普通文字，1 加粗（表头）。两种都用“文本”数字格式（@）。
const styles = xml.Header + `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
	`<fonts count="2"><font><sz val="11"/><name val="Microsoft YaHei"/></font><font><b/><sz val="11"/><name val="Microsoft YaHei"/></font></fonts>` +
	`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
	`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
	`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
	`<cellXfs count="2"><xf numFmtId="49" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="49" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1" applyNumberFormat="1"/></cellXfs>` +
	`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>` +
	`</styleSheet>`
