// SPDX-License-Identifier: GPL-3.0-or-later

package results

import (
	"strings"
	"testing"
)

// 样例包覆盖不到的 JSON 写法；网站的两个实现（Python、浏览器）有同样的用例。
func TestParseSchemaFirst(t *testing.T) {
	// 更新的大版本可以改字段的类型：先认出版本，不能报成格式不对。
	if _, code, msg := parsePackage([]byte(`{"schema": "logdiag.package/2", "devices": "改成了别的类型"}`)); code != CodeSchemaUnsupported {
		t.Errorf("package 新版本：%s %s", code, msg)
	}
	if _, code, msg := parseManifest([]byte(`{"schema": "logdiag.manifest/2", "files": {}}`)); code != CodeSchemaUnsupported {
		t.Errorf("manifest 新版本：%s %s", code, msg)
	}
	for data, want := range map[string]string{
		`[]`:                   "不是 JSON 对象",
		`null`:                 "不是 JSON 对象",
		`{"schema": 1}`:        "schema 的类型不对",
		"\ufeff{}":             "不是合法的 JSON",
		`{} {}`:                "多余的内容",
		`{"schema": "x"} junk`: "不是合法的 JSON",
	} {
		_, code, msg := parseManifest([]byte(data))
		if code != CodeManifestInvalid || !strings.Contains(msg, want) {
			t.Errorf("%q：%s %s，应包含 %q", data, code, msg, want)
		}
	}
	// 第 1 版里字段类型不对照旧是格式不对。
	if _, code, msg := parsePackage([]byte(`{"schema": "logdiag.package/1", "devices": "x"}`)); code != CodePackageInvalid || !strings.Contains(msg, "devices 的类型不对") {
		t.Errorf("第 1 版字段类型不对：%s %s", code, msg)
	}
}
