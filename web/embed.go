// SPDX-License-Identifier: GPL-3.0-or-later

// Package web 内嵌本机界面的静态文件。
//
// 前端不用 npm 构建链：原生 ES Module + CSS，改完文件重新编译即可，构建不联网。
package web

import "embed"

// FS 包含 index.html 和 assets/ 下的文件。
//
//go:embed index.html assets
var FS embed.FS
