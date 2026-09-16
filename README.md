# context-probe

> 当前版本：**0.9.3**（唯一手写处为 `main.go` 的 `pluginVersion`；本行由构建脚本 / `go test -run TestReadmeVersionInSync -update` 自动同步。
> 版本规范（宿主校验）：非空、**不以 `v` 开头**、匹配 `^[0-9][0-9A-Za-z.+-]*$`；更新检测按点分整数逐段比较，故推荐纯数字点分 `MAJOR.MINOR.PATCH`）

CLIProxyAPI 标准动态库插件（Management API 能力）：探测各 `openai-compatibility`
渠道的真实上下文上限（输出上限仅在接口明确给出时取用），并把精确的 `max-context-length` 写回 `config.yaml`
（宿主 file watcher 自动热加载）。

值解析优先级：**元数据 > 报错提取 > 已接受（config 现值）> 保持现值**。

探测起点（真正发出去的 `max_tokens`）按优先级取：`config.yaml` 现值 → 默认 **1500000**。
默认值刻意取得足够大：渠道几乎必定报错，插件即从报错文本提取真实上限（上下文 / 输出）并回写 `config.yaml`。
输出上限不再主动探测：仅当接口**明确给出**（`/models` 元数据或报错文本中的合法上限）时才写回，接口不给值时保持不写。
若站点报错只提输出上限、不提上下文，插件会用「超大输入 + `max_tokens=1`」**自动再探一次**逼出上下文上限（无需手动触发）。

写回两个目标：
1. `max-context-length`（上下文上限）→ 各供应商 `models` 条目内联字段；
2. **输出上限** → 全局 `payload.override` 规则的 `params: max_tokens`（按请求模型名匹配）。
   写回方式为**追加规则**（同名同值已覆盖则跳过；同名不同值追加新规则，宿主 last-write-wins 后置生效），不改动既有规则，避免破坏手工特例。

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

进入页面先展示**模型列表页**：
- 按供应商分组列出 `config.yaml` 中可探测的模型（未启用/不可探测的供应商不展示）；供应商表头可**折叠**（▾）、可**整组勾选**（勾选即选中该供应商下所有模型）；
- 每个模型显示：上下文、输出上限、来源、上次状态、最近一次探测详情（优先用上次探测报告，缺失时回落 config 现值 / —）；
- 工具栏：全选 / 全不选 / 已选计数 / JSON 报告；
- 右下角**浮动「▶ 开始探测」**：对已勾选模型发起后台探测，随后跳转探测页。

探测页：
- 顶部：探测状态、进度条、状态计数标签（合计 / 可用 / 死渠道 / 未知 / 待探测，点击筛选行）；
- 下方：只展示**参与本次探测**的供应商与模型，同样可按供应商/模型勾选（默认全选）；
- 右下角浮动按钮：**↻ 重试**（对本次列表中勾选的模型再次探测）、**⬇ 数据回写**（手动触发写回）、**✔ 完成**（返回模型列表页，显示回写后的最新数据）；
- 探测中每 3 秒软刷新（无闪烁）；探测完成后按 `auto_apply` 自动写回。

- **写回**：把 `max-context-length` 写回 `config.yaml`（自动备份 `.bak.plugin.<时间戳>`）；输出上限仅在接口明确给出时追加 `payload.override` 的 `max_tokens` 规则。
- **JSON 报告**：查看 / 下载 `model-context-probe.json`。

## 配置项（config.yaml `plugins.configs.context-probe`）

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `config_path` | string | 自动探测 | config.yaml 路径 |
| `providers` | string | 空 | 供应商过滤（子串，逗号分隔，空 = 全部） |
| `probe_timeout_seconds` | int | 60 | 单请求超时秒数 |
| `probe_delay_seconds` | float | 0.3 | 请求间隔秒数 |
| `auto_apply` | bool | true | 探测后自动写回 |
| `metadata_only` | bool | false | 仅元数据模式，不发测试请求 |

## 说明

- 429（并发/限流）、502/503/504（网关不可用）标记为**临时失败**（状态「未知」），可单点重试；
- 已停用（`disabled: true`）或未配置 base-url/api-key 的供应商**不展示、不参与探测**；
- **自定义请求头**：探测请求（chat/completions 与 /models 元数据）会带上供应商 `headers:` 中的静态自定义头，并发送宿主同款 `User-Agent: cli-proxy-openai-compat`；`$` 前缀的动态值（宿主从下游客户端请求复制）探测时无法还原，自动跳过；自定义头可覆盖默认 Authorization/Content-Type/UA（与宿主行为一致）；
- **上下文自动补探**：probe1 用 `max_tokens=1500000` 触发报错。若报错只暴露了输出上限、没提上下文（部分站点先校验 `max_tokens`），插件会**自动**再发一次「超大输入 + `max_tokens=1`」的探测请求，逼出 `maximum context length is N` 之类的报错并提取上下文，**无需手动再触发**；输入按 token 估算取足够大（随机串防 BPE 压缩），遇 413/请求体过大自动缩小重试。
- **输出上限（max_tokens）**：不再主动探测。仅当接口**明确返回**时才写回 `payload.override` 的 `max_tokens`——来源为 `/models` 元数据，或 probe1 报错文本中明确给出的合法上限；接口不报错/不给值时不写入（避免写入虚高假值）。
- 探测只读，写回仅改动 `max-context-length` 与 `payload.override` 的 `max_tokens`（后者仅在明确值时）。
