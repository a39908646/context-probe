# context-probe

> 当前版本：**0.10.0**（唯一手写处为 `main.go` 的 `pluginVersion`；本行由构建脚本 / `go test -run TestReadmeVersionInSync -update` 自动同步。
> 版本规范（宿主校验）：非空、**不以 `v` 开头**、匹配 `^[0-9][0-9A-Za-z.+-]*$`；更新检测按点分整数逐段比较，故推荐纯数字点分 `MAJOR.MINOR.PATCH`）

CLIProxyAPI 标准动态库插件（Management API 能力）。把 `config.yaml` 中 `openai-compatibility`
渠道的模型列出来，支持**手动填写上下文上限**并写回 config，并按 config 里的值**直接测试**渠道是否报错。

> v0.10.0 起按需求重构：删除自动探测 / 报错提取 / 元数据推断 / 超大输入补探等全部猜测逻辑，
> 值完全由用户手动决定，插件只负责展示、编辑、写回、测试。

## 工作流

1. **模型列表**：按供应商分组列出可探测模型，每行显示并编辑：
   - **上下文** → 写回模型项内联的 `max-context-length`；
   - **输出上限** → 写回 `payload.override` 规则的 `max_tokens`（追加规则，last-write-wins）。
2. **筛选**：勾选「仅看上下文为空」只显示还没填值的模型。
3. **编辑**：
   - 单点：直接改某行的输入框；
   - 批量：填写「批量上下文 / 批量输出上限」，点「应用到已选」，会把值填入所有已勾选模型；
   - 点「保存并回写 config」把改动写入 `config.yaml`（写前自动备份 `.bak.plugin.<时间戳>`，宿主热加载）。
4. **测试**：点「▶ 按 config 值测试」后，插件**直接以 config 中填写的上下文值作为 `max_tokens`**
   对每个模型发一次 `/chat/completions` 请求，页面实时显示结果：
   - 可用：接口接受该值；
   - 报错：接口返回非 2xx，详情展示 HTTP 状态与错误信息；
   - 死渠道：401/403/404/余额不足/模型不存在等；
   - 网络错误：连接失败或超时（传输层瞬时错误会自动重试 3 次）；
   - 已跳过：config 里上下文为空，无法测试。

未勾选任何模型时，测试会作用于当前全部可探测模型。

## 目录结构

```
context-probe-plugin/
├── main.go              # 源码（全部逻辑，单文件）
├── go.mod               # Go module 定义（源码）
├── main_test.go         # 单元测试
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
| 源码 | `main.go`、`main_test.go`、`go.mod` | 需要版本管理、可编辑修改 |
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

## 配置项（config.yaml `plugins.configs.context-probe`）

| 键 | 类型 | 默认 | 说明 |
|----|------|------|------|
| `config_path` | string | 自动探测 | config.yaml 路径 |
| `providers` | string | 空 | 供应商过滤（子串，逗号分隔，空 = 全部） |
| `probe_timeout_seconds` | int | 60 | 单请求超时秒数 |
| `probe_delay_seconds` | float | 0.3 | 测试请求间隔秒数 |

## 说明

- 探测请求（`/chat/completions`）会带上供应商 `headers:` 中的静态自定义头，并发送宿主同款
  `User-Agent: cli-proxy-openai-compat`；`$` 前缀的动态值（宿主从下游客户端请求复制）测试时无法还原，自动跳过；
  自定义头可覆盖默认 Authorization/Content-Type/UA（与宿主行为一致）。
- 写回仅改动两处：模型项的 `max-context-length`，以及 `payload.override` 追加的 `max_tokens` 规则。
  不改动既有 override 规则，避免破坏手工特例。
- 已停用（`disabled: true`）或未配置 base-url/api-key 的供应商不展示、不参与测试。
- 裸露的标量模型行（`- model-name`）不支持写回，输入框为禁用状态，请在 config.yaml 中改为
  `- name: model-name` 的键值形式。
- 写回前自动备份：`config.yaml.bak.plugin.<YYYYMMDD-HHMMSS>`。
- JSON 报告：`?op=report` 返回最近一次测试结果的 JSON。
