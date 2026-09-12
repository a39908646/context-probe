# context-probe

> 当前版本：**0.2.0**（发布时递增，格式须为数字开头的点分版本，如 `0.2.0`；宿主据此检测插件更新）

CLIProxyAPI 标准动态库插件（Management API 能力）：探测各 `openai-compatibility`
渠道的真实上下文 / 输出上限，并把精确的 `max-context-length` 写回 `config.yaml`
（宿主 file watcher 自动热加载）。

值解析优先级：**元数据 > 报错提取 > 已接受（声称值）> 映射表回落（仅未校验渠道）> 保持现值**。
仅写回 `max-context-length`；`payload.override` 仍由脚本 / 手动管理。

## 目录结构

```
context-probe-plugin/
├── main.go              # 源码（全部逻辑，单文件）
├── go.mod               # Go module 定义（源码）
├── build.ps1            # 构建脚本（Windows）
├── build.sh             # 构建脚本（Linux/macOS）
├── README.md            # 本文档
├── .gitignore           # 忽略构建产物
└── dist/                # ★ 构建产物目录（不提交 git）
    ├── context-probe.dll    # 插件本体（c-shared 动态库）
    └── context-probe.h      # cgo 自动生成的 C 头文件（随 DLL 生成）
```

### 源码 vs 构建产物

| 类别 | 文件 | 说明 |
|------|------|------|
| 源码 | `main.go`、`go.mod` | 需要版本管理、可编辑修改 |
| 构建脚本 | `build.ps1`、`build.sh` | 生成产物用，需要版本管理 |
| 文档 | `README.md`、`.gitignore` | 需要版本管理 |
| **构建产物** | `dist/context-probe.dll` | **由源码编译生成，不要手工编辑** |
| **构建产物** | `dist/context-probe.h` | **cgo 编译时自动生成，不要手工编辑** |

## 构建

要求：Go ≥ 1.24。

```powershell
# Windows（PowerShell）
.\build.ps1
```

```bash
# Linux / macOS
./build.sh
```

或手动执行（等价）：

```bash
go build -buildmode=c-shared -o dist/context-probe.dll .
```

注意：`-buildmode=c-shared` 是必须的，普通 `go build` 产出的是可执行文件而非插件。

## 部署

1. 把 `dist/context-probe.dll` 复制到 CLIProxyAPI 的 `plugins/` 目录
   （或 `plugins/<goos>/<goarch>/`，文件名即插件 ID：`context-probe`）；
2. **重启 CLIProxyAPI**（Go 插件在进程启动时加载，替换文件后必须重启）；
3. 管理台侧栏出现「Context Probe」页面。

## 功能页（Management API `/probe`）

- **开始探测**：后台全量探测，页面 3 秒软刷新（无闪烁），显示进度条 + 当前探测模型；
- **状态筛选**：点击「可用 / 死渠道 / 未知」徽章筛选表格行；
- **单点重试**：供应商标题旁 ↻ 重试整个供应商；模型行状态旁 ↻ 只重试该模型
  （`?op=probe&provider=X&model=Y`），结果合并进现有报告，不影响其它结果；
- **应用写回**：按报告把 `max-context-length` 写回 `config.yaml`（自动备份 `.bak.plugin.<时间戳>`）；
- **JSON 报告**：查看 / 下载 `model-context-probe.json`。

## 配置项（config.yaml `plugins.configs.context-probe`）

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `config_path` | string | 自动探测 | config.yaml 路径 |
| `mapping_path` | string | 同目录 `model-context-map.json` | 映射表（未校验渠道回落值） |
| `providers` | string | 空 | 供应商过滤（子串，逗号分隔，空 = 全部） |
| `probe_timeout_seconds` | int | 60 | 单请求超时秒数 |
| `probe_delay_seconds` | float | 0.3 | 请求间隔秒数 |
| `auto_apply` | bool | true | 探测后自动写回 |
| `metadata_only` | bool | false | 仅元数据模式，不发测试请求 |

## 说明

- 429（并发/限流）、502/503/504（网关不可用）标记为**临时失败**（状态「未知」），可单点重试；
- 已停用（`disabled: true`）的供应商不参与探测；
- 探测只读，写回仅改动 `max-context-length` 一项。
