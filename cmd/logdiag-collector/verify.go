// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/dylanlabs/logdiag-collector/internal/results"
)

// runVerify 检查离线上传包：每个文件的大小和 SHA-256、manifest 和 package.json 是否一致。
// 网站收包时做同样的检查（见 docs/schema/README.md）。
func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "以 JSON 输出检查结果，result 为 ok、problems、not_package 或 not_zip")
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "用法：logdiag-collector verify [选项] <离线上传包.zip>\n\n"+
			"检查离线上传包是否完整：每个文件的大小和 SHA-256、manifest 和 package.json 是否一致。\n"+
			"拷到 U 盘或别的电脑后可以再检查一次。网站收包时做同样的检查。\n\n选项：\n")
		fs.PrintDefaults()
	}
	// 选项可以写在文件名前后。
	var files []string
	for rest := args; ; rest = fs.Args()[1:] {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
	}
	if len(files) != 1 {
		fmt.Fprintln(stderr, "请指定一个离线上传包，例如：logdiag-collector verify LogDiag-示例客户-20261120-1430-5d0c8e1a.zip")
		return 2
	}

	v, err := results.VerifyPackage(files[0])
	result := "ok"
	switch {
	case errors.Is(err, results.ErrNotZip):
		result = "not_zip"
	case errors.Is(err, results.ErrNotPackage):
		result = "not_package"
	case err != nil:
		fmt.Fprintln(stderr, err)
		return 1
	case !v.OK():
		result = "problems"
	}

	if *asJSON {
		out := struct {
			Result  string `json:"result"`
			Message string `json:"message,omitempty"`
			*results.Verification
		}{Result: result, Verification: v}
		if err != nil {
			out.Message = err.Error()
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintf(stdout, "%s\n", data)
	} else {
		switch {
		case err != nil:
			fmt.Fprintln(stdout, err)
		case !v.OK():
			fmt.Fprintf(stdout, "发现 %d 个问题：\n", len(v.Problems))
			for _, p := range v.Problems {
				fmt.Fprintf(stdout, "  - %s\n", p.Message)
			}
			fmt.Fprintln(stdout, "\n有问题的设备网站不会接收。可以回到收集结果里重新打包；文件本身损坏时需要重新收集。")
		default:
			customer := "客户“" + v.Package.Customer + "”"
			if v.Package.Customer == "" {
				customer = "没有填客户"
			}
			fmt.Fprintf(stdout, "离线上传包没有问题：%d 台设备，%s，打包时间 %s。\n", len(v.Package.Devices), customer, v.Package.CreatedAt)
			for _, d := range v.Package.Devices {
				fmt.Fprintf(stdout, "  %s  %s\n", d.Dir, d.LogType)
			}
		}
	}
	if result != "ok" {
		return 1
	}
	return 0
}
