#!/usr/bin/env bash
# 编译 Windows 可执行文件（带版本资源），在 Linux、macOS 或 Windows 的 Git Bash 里都能运行。
#
#   scripts/build.sh                 开发版，版本号 0.0.0-dev.g<提交>
#   scripts/build.sh v0.1.0          正式版（发布流水线传入 tag）
#   scripts/build.sh v0.1.0 arm64    其他架构
#
# 标准输出只打印生成的文件路径，便于流水线使用。
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:-}"
arch="${2:-amd64}"
if [[ -z "$version" ]]; then
  version="0.0.0-dev.g$(git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)"
fi
version="${version#v}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "版本号格式不对：$version（应为 1.2.3 或 1.2.3-beta.1）" >&2
  exit 1
fi
case "$arch" in
  amd64) suffix=x64 ;;
  arm64) suffix=arm64 ;;
  *) echo "不支持的架构：$arch" >&2; exit 1 ;;
esac

pkg=./cmd/logdiag-collector
out="dist/LogDiagCollector-${version}-win-${suffix}.exe"
mkdir -p dist

# 版本资源：文件属性里显示产品名、版本、发布者。缺少版本信息是杀毒软件误报的常见原因之一。
rm -f "$pkg"/rsrc_windows_*.syso
go tool go-winres make \
  --in "$pkg/winres/winres.json" \
  --out "$pkg/rsrc" \
  --arch "$arch" \
  --product-version "$version" \
  --file-version "$version" >&2

# -trimpath 去掉本机路径，同一提交在任何机器上编出相同的文件；不加壳（UPX 等）以免误报。
CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build \
  -trimpath \
  -ldflags "-s -w -X github.com/dylanlabs/logdiag-collector/internal/buildinfo.Version=$version" \
  -o "$out" "$pkg"

rm -f "$pkg"/rsrc_windows_*.syso
echo "$out"
