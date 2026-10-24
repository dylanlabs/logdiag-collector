// SPDX-License-Identifier: GPL-3.0-or-later

// Package schema 只有测试：几份 JSON Schema 本身要合法，examples/ 下的样例和样例包里的 JSON
// 要符合 schema，常见的不合规写法要被拦住。网站可以直接用这几份 schema 校验。
package schema

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compile(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile(name)
	if err != nil {
		t.Fatalf("%s：%v", name, err)
	}
	return s
}

func decode(t *testing.T, data []byte) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExamplesMatchSchemas(t *testing.T) {
	manifest := compile(t, "manifest-1.schema.json")
	pkg := compile(t, "package-1.schema.json")
	latest := compile(t, "latest-1.schema.json")

	for name, s := range map[string]*jsonschema.Schema{
		"examples/manifest.json": manifest,
		"examples/package.json":  pkg,
		"examples/latest.json":   latest,
	} {
		if err := s.Validate(decode(t, readFile(t, name))); err != nil {
			t.Errorf("%s：%v", name, err)
		}
	}

	// 只有 version、url、notes 的 BMCKit 格式也符合 latest.json 的 schema。
	if err := latest.Validate(decode(t, []byte(`{"version": "1.6.1", "url": "https://logdiag.com/bmckit/", "notes": "修复若干问题。"}`))); err != nil {
		t.Errorf("BMCKit 格式：%v", err)
	}

	// 样例包里的 package.json 和每个 manifest.json（含 HPE 带附带文件的那份）。
	z, err := zip.OpenReader("examples/packages/ok.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	n := 0
	for _, f := range z.File {
		var s *jsonschema.Schema
		switch {
		case f.Name == "package.json":
			s = pkg
		case path.Base(f.Name) == "manifest.json":
			s = manifest
		default:
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		if err := s.Validate(decode(t, data)); err != nil {
			t.Errorf("ok.zip 里的 %s：%v", f.Name, err)
		}
		n++
	}
	if n != 4 {
		t.Errorf("ok.zip 里应有 1 个 package.json 和 3 个 manifest.json，实际 %d 个", n)
	}
}

// mutate 解析 JSON，改一处后返回。
func mutate(t *testing.T, data []byte, change func(m map[string]any)) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	out, _ := json.Marshal(m)
	return decode(t, out)
}

func obj(v any) map[string]any { return v.(map[string]any) }
func arr(v any) []any          { return v.([]any) }

func TestSchemasRejectBadDocuments(t *testing.T) {
	manifest := compile(t, "manifest-1.schema.json")
	pkg := compile(t, "package-1.schema.json")
	latest := compile(t, "latest-1.schema.json")
	m := readFile(t, "examples/manifest.json")
	p := readFile(t, "examples/package.json")
	l := readFile(t, "examples/latest.json")

	cases := map[string]struct {
		schema *jsonschema.Schema
		doc    []byte
		change func(m map[string]any)
	}{
		"manifest 文件名带路径":             {manifest, m, func(m map[string]any) { obj(arr(m["files"])[0])["name"] = "../dump_info.tar.gz" }},
		"manifest 文件名带冒号":             {manifest, m, func(m map[string]any) { obj(arr(m["files"])[0])["name"] = "dump.tar.gz:hidden" }},
		"manifest 文件名是 ..":            {manifest, m, func(m map[string]any) { obj(arr(m["files"])[0])["name"] = ".." }},
		"manifest 里的 role 是 manifest": {manifest, m, func(m map[string]any) { obj(arr(m["files"])[0])["role"] = "manifest" }},
		"manifest 没有 main 文件":         {manifest, m, func(m map[string]any) { obj(arr(m["files"])[0])["role"] = "extra" }},
		"manifest 文件大小为 0":            {manifest, m, func(m map[string]any) { obj(arr(m["files"])[0])["size"] = 0 }},
		"log.type 有大写":                {manifest, m, func(m map[string]any) { obj(m["log"])["type"] = "Huawei.iBMC.Dump" }},
		"log.type 只有一段":               {manifest, m, func(m map[string]any) { obj(m["log"])["type"] = "dump" }},
		"device.type 不在枚举里":           {manifest, m, func(m map[string]any) { obj(m["device"])["type"] = "switch" }},
		"collected_at 没有时区":           {manifest, m, func(m map[string]any) { m["collected_at"] = "2026-11-20 14:03:00" }},
		"batch_id 不是 UUID":            {manifest, m, func(m map[string]any) { m["batch_id"] = "batch-1" }},
		"缺 redaction":                 {manifest, m, func(m map[string]any) { delete(m, "redaction") }},
		"manifest 大版本不对":              {manifest, m, func(m map[string]any) { m["schema"] = "logdiag.manifest/2" }},
		"batch_ids 为 null":            {pkg, p, func(m map[string]any) { m["batch_ids"] = nil }},
		"设备文件夹名带 /":                   {pkg, p, func(m map[string]any) { obj(arr(m["devices"])[0])["dir"] = "a/b" }},
		"设备文件夹名带 \\":                  {pkg, p, func(m map[string]any) { obj(arr(m["devices"])[0])["dir"] = `a\b` }},
		"SHA-256 用大写":                 {pkg, p, func(m map[string]any) { obj(arr(obj(arr(m["devices"])[0])["files"])[1])["sha256"] = "ABCDEF" }},
		"没有设备":                        {pkg, p, func(m map[string]any) { m["devices"] = []any{} }},
		"package 大版本不对":               {pkg, p, func(m map[string]any) { m["schema"] = "logdiag.package/2" }},
		"下载页不是 HTTPS":                 {latest, l, func(m map[string]any) { m["url"] = "http://logdiag.com/collector/" }},
		"版本号不对":                       {latest, l, func(m map[string]any) { m["version"] = "latest" }},
		"缺下载页":                        {latest, l, func(m map[string]any) { delete(m, "url") }},
	}
	for name, c := range cases {
		if err := c.schema.Validate(mutate(t, c.doc, c.change)); err == nil {
			t.Errorf("%s：应该不符合 schema", name)
		}
	}
}
