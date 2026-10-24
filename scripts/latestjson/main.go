// SPDX-License-Identifier: GPL-3.0-or-later

// latestjson 生成 latest.json（格式见 docs/schema/latest-1.schema.json），由发布流水线在编译后运行：
//
//	go run ./scripts/latestjson -version v0.1.0 -min-supported 0.1.0 dist/*.exe > dist/latest.json
//
// 更新说明取自 CHANGELOG.md 里这个版本的一节（“## 0.1.0”到下一个“## ”之前）。正式版必须有这一节，
// 否则报错、不发布；测试版（带 -）没有时说明留空。
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dylanlabs/logdiag-collector/internal/buildinfo"
	"github.com/dylanlabs/logdiag-collector/internal/update"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, "latestjson：", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) error {
	fs := flag.NewFlagSet("latestjson", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "版本号或标签，例如 v0.1.0（必填）")
	minSupported := fs.String("min-supported", "", "仍然支持的最低版本，低于它的版本检查更新时提示必须升级；空表示不限制")
	changelog := fs.String("changelog", "CHANGELOG.md", "更新记录文件")
	releasedAt := fs.String("released-at", "", "发布时间（RFC 3339），默认为现在")
	page := fs.String("page", "https://logdiag.com/collector/", "下载页")
	downloadBase := fs.String("download-base", "https://logdiag.com/collector/download/", "网站上下载文件的地址前缀")
	repo := fs.String("repo", "dylanlabs/logdiag-collector", "GitHub 仓库，备用下载地址指向它的 Release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	v, ok := update.ParseVersion(*version)
	if !ok || strings.Contains(*version, "+") {
		return fmt.Errorf("版本号格式不对：%q", *version)
	}
	if *minSupported != "" {
		min, ok := update.ParseVersion(*minSupported)
		if !ok || min.Compare(v) > 0 {
			return fmt.Errorf("-min-supported %q 不是版本号，或比要发布的 %s 还新", *minSupported, v)
		}
		*minSupported = min.String()
	}
	if fs.NArg() == 0 {
		return errors.New("请在参数里列出要发布的 exe")
	}
	at := now().UTC().Format(time.RFC3339)
	if *releasedAt != "" {
		t, err := time.Parse(time.RFC3339, *releasedAt)
		if err != nil {
			return fmt.Errorf("-released-at：%w", err)
		}
		at = t.Format(time.RFC3339)
	}

	notes, err := changelogSection(*changelog, v.String())
	if err != nil {
		return err
	}
	if notes == "" && len(v.Pre) == 0 {
		return fmt.Errorf("%s 里没有 %s 的更新说明：请加一节“## %s”，写明这个版本给用户的变化，再重新推送标签", *changelog, v, v)
	}
	if n := len([]rune(notes)); n > 600 {
		fmt.Fprintf(stderr, "提示：%s 的更新说明有 %d 字，检查更新时只显示前 600 字\n", v, n)
	}

	tag := "v" + v.String()
	l := update.Latest{
		Schema:       update.Schema,
		Product:      buildinfo.ProductName,
		Version:      v.String(),
		ReleasedAt:   at,
		URL:          *page,
		Notes:        notes,
		MinSupported: *minSupported,
	}
	for _, path := range fs.Args() {
		d, err := download(path)
		if err != nil {
			return err
		}
		d.URL = strings.TrimSuffix(*downloadBase, "/") + "/" + d.Name
		d.Mirror = "https://github.com/" + *repo + "/releases/download/" + tag + "/" + d.Name
		l.Downloads = append(l.Downloads, d)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", data)
	return err
}

var archRE = regexp.MustCompile(`-win-(x64|arm64)\.exe$`)

// download 读 exe 的大小和 SHA-256；文件名必须是 build.sh 的格式 LogDiagCollector-<版本>-win-<架构>.exe。
func download(path string) (update.Download, error) {
	name := filepath.Base(path)
	m := archRE.FindStringSubmatch(name)
	if m == nil {
		return update.Download{}, fmt.Errorf("%s 不是 build.sh 生成的文件名（应以 -win-x64.exe 或 -win-arm64.exe 结尾）", name)
	}
	f, err := os.Open(path)
	if err != nil {
		return update.Download{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return update.Download{}, err
	}
	arch := map[string]string{"x64": "amd64", "arm64": "arm64"}[m[1]]
	return update.Download{Name: name, OS: "windows", Arch: arch, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// changelogSection 返回 CHANGELOG 里标题为“## <版本>”（可带 v 前缀、方括号，后面可以写日期）的一节正文。
func changelogSection(path, version string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	head := regexp.MustCompile(`^##\s+\[?v?` + regexp.QuoteMeta(version) + `([^0-9A-Za-z.+-]|$)`)
	var body bytes.Buffer
	in := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## ") || line == "##" {
			if in {
				break
			}
			in = head.MatchString(line)
			continue
		}
		if in {
			body.WriteString(line + "\n")
		}
	}
	return strings.TrimSpace(body.String()), sc.Err()
}
