// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/update"
)

var updateExample = flag.Bool("update", false, "重新生成 docs/schema/examples/latest.json")

const changelog = `# 更新记录

## 未发布

- 还没发布的改动。

## 0.1.0（2026-11-27）

首个版本：

- 华为、超聚变、浪潮、联想、Dell、HPE 服务器一键收集日志包。
- 收集结果按客户打成离线上传包，拷到能上网的电脑再传到 LogDiag。

## [0.1.0-rc.1]

- 测试版。
`

func setup(t *testing.T) (dir, exe string) {
	t.Helper()
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(changelog), 0o644)
	exe = filepath.Join(dir, "LogDiagCollector-0.1.0-win-x64.exe")
	os.WriteFile(exe, []byte("MZ 示例文件，大小和 SHA-256 只是演示。"), 0o644)
	return dir, exe
}

func gen(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := run(args, &out, &errb, func() time.Time { return time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC) })
	return out.String(), err
}

func TestLatestJSON(t *testing.T) {
	dir, exe := setup(t)
	cl := filepath.Join(dir, "CHANGELOG.md")
	out, err := gen(t, "-version", "v0.1.0", "-min-supported", "0.1.0", "-changelog", cl, exe)
	if err != nil {
		t.Fatal(err)
	}
	example := "../../docs/schema/examples/latest.json"
	if *updateExample {
		os.WriteFile(example, []byte(out), 0o644)
	}
	if want, _ := os.ReadFile(example); string(want) != out {
		t.Errorf("和 docs/schema/examples/latest.json 不一致（go test ./scripts/latestjson -update 重新生成）：\n%s", out)
	}
	l, err := update.Parse([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if l.Version != "0.1.0" || l.MinSupported != "0.1.0" || l.ReleasedAt != "2026-11-27T02:00:00Z" ||
		!strings.HasPrefix(l.Notes, "首个版本：") || strings.Contains(l.Notes, "未发布") || strings.Contains(l.Notes, "测试版") || len(l.Downloads) != 1 {
		t.Errorf("latest.json：%+v", l)
	}
	d := l.Downloads[0]
	if d.Arch != "amd64" || d.Size == 0 || len(d.SHA256) != 64 ||
		d.URL != "https://logdiag.com/collector/download/LogDiagCollector-0.1.0-win-x64.exe" ||
		d.Mirror != "https://github.com/dylanlabs/logdiag-collector/releases/download/v0.1.0/LogDiagCollector-0.1.0-win-x64.exe" {
		t.Errorf("下载文件：%+v", d)
	}

	// 测试版：CHANGELOG 里有就用，没有就留空。
	if out, err := gen(t, "-version", "v0.1.0-rc.1", "-changelog", cl, exe); err != nil || !strings.Contains(out, `"notes": "- 测试版。"`) {
		t.Errorf("测试版：%s %v", out, err)
	}
	if out, err := gen(t, "-version", "v0.2.0-beta.1", "-changelog", cl, exe); err != nil || strings.Contains(out, `"notes"`) {
		t.Errorf("没有说明的测试版：%s %v", out, err)
	}
	var parsed map[string]any
	if out, _ := gen(t, "-version", "v0.1.0", "-changelog", cl, exe); json.Unmarshal([]byte(out), &parsed) != nil || parsed["min_supported"] != nil {
		t.Errorf("不限制最低版本时不写 min_supported：%s", out)
	}
}

func TestLatestJSONErrors(t *testing.T) {
	dir, exe := setup(t)
	cl := filepath.Join(dir, "CHANGELOG.md")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-version", "v0.2.0", "-changelog", cl, exe}, "没有 0.2.0 的更新说明"},
		{[]string{"-version", "0.1", "-changelog", cl, exe}, "版本号格式不对"},
		{[]string{"-version", "v0.1.0", "-min-supported", "0.2.0", "-changelog", cl, exe}, "比要发布的 0.1.0 还新"},
		{[]string{"-version", "v0.1.0", "-changelog", cl}, "列出要发布的 exe"},
		{[]string{"-version", "v0.1.0", "-changelog", cl, cl}, "不是 build.sh 生成的文件名"},
		{[]string{"-version", "v0.1.0", "-changelog", filepath.Join(dir, "没有这个文件.md"), exe}, "没有 0.1.0 的更新说明"},
	} {
		if _, err := gen(t, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v：%v，应包含 %q", c.args, err, c.want)
		}
	}
}
