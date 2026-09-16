# 构建 context-probe 插件（Windows）
# 产物输出到 dist/：context-probe.dll + context-probe.h
$ErrorActionPreference = "Stop"
if (!(Get-Command go -ErrorAction SilentlyContinue)) { throw "未找到 go，请先安装 Go >= 1.24" }
New-Item -ItemType Directory -Force -Path dist | Out-Null
# 同步 README 顶部版本行到 main.go 的 pluginVersion（版本号单处定义）
go test -run TestReadmeVersionInSync -update . | Out-Null
go vet .
go build -buildmode=c-shared -o dist/context-probe.dll .
Write-Host "构建完成: dist/context-probe.dll" -ForegroundColor Green
