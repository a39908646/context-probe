#!/usr/bin/env bash
# 构建 context-probe 插件（Linux/macOS）
# 产物输出到 dist/：context-probe.so + context-probe.h
set -euo pipefail
command -v go >/dev/null || { echo "未找到 go，请先安装 Go >= 1.24"; exit 1; }
mkdir -p dist
ext=so
[[ "$(go env GOOS)" == "windows" ]] && ext=dll
go vet .
go build -buildmode=c-shared -o "dist/context-probe.$ext" .
echo "构建完成: dist/context-probe.$ext"
