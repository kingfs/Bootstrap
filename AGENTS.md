# AGENTS.md —— 给 AI/协作者的工程约定

本文件是这份仓库的"施工图"。改代码前先读完本页与 [docs/design.md](docs/design.md) 的相关章节。

## 1. 这个项目是什么

`bs`（仓库名 Bootstrap）是维护者个人的开发环境重建工具：新机器上用它拉回仓库、
恢复代理与 shell 环境、按清单安装/更新工具链（M1–M3 已完成：`bs init` 一把梭 + 自更新）。

- 语言：Go（版本见 `go.mod`），命令框架 cobra + 配置 viper
- 支持平台：Linux / macOS / Windows；Linux 与 macOS 的 `amd64`、`arm64` 都必须可用
- 发布方式：只有 GitHub Release 二进制归档（`tar.gz` / Windows `zip`）+ `checksums.txt`，
  不做 deb/rpm/brew/scoop/winget（见设计文档 ADR-0008）

## 2. 分层与依赖方向

```
cmd/bs/main.go
   └── internal/cli        （只做参数解析、调用 app、渲染输出）
          └── internal/app （用例编排；不依赖 cobra，不直接打印）
                 └── internal/<domain>（config / state / proxy / shellsnip / gitrepo …）
                        └── pkg/*  （纯逻辑，可被任意层引用）
```

**硬性规则**

1. `internal/cli` 里不得出现业务逻辑、不得直接读写配置文件；需要新能力时先在 `internal/app` 加方法。
2. `internal/app` 不得导入 `cobra`、不得 `fmt.Println` 到终端；输出通过 `App.Out/Err` 注入的 writer。
3. `pkg/*` 必须满足三条才允许新增：**纯逻辑**、**可独立复用**、**不含本项目的业务词汇**
   （出现 `tool`/`manifest`/`proxy profile` 这类概念就说明它该放在 `internal/`）。
4. 依赖只能自上而下；同层可以互相引用，但要避免环。
5. 外部命令一律通过 `pkg/cmdx` 执行（自动获得超时、干跑、脱敏、日志、取消时杀掉整棵进程组）。

## 3. 常用命令

```sh
make build     # bin/bs
make test      # 单元测试
make race      # -race
make smoke     # 无副作用端到端冒烟（关键回归保护）
make pty       # 真 PTY 里验证交互式界面（勾选/取消/破坏性操作默认拒绝）
make check     # gofmt + vet + test + race（提交前必跑）
make cross     # 交叉编译 6 个平台组合
```

受限环境（`$HOME` 只读）下，先 `. scripts/devenv.sh`：它把 `GOPATH`/`GOCACHE` 指到仓库内 `.cache/`。

## 4. 测试要求（重要）

0. **交互式界面（huh）不能只靠单测**：它只在真终端里启动，而假终端会吞掉按键
   （bubbletea 会往终端发色彩/光标查询并等回应，`script` 这类假终端没人回应）。
   因此 `internal/ui` 的单测覆盖纯文本路径与选项映射，真界面用
   `make pty`（`scripts/pty-check.py`：扮演会回答查询的终端 + 按时间表送按键）验证。
1. **所有测试必须无副作用**：
   - 文件读写只用 `t.TempDir()`，绝不写真实 `$HOME`、`/etc`、注册表；
   - 不改真实 rc 文件（用 `--rc` / 注入路径）；
   - 单元测试与 `scripts/smoke.sh` 都不发网络请求（网络路径用 `--dry-run` 或假 Runner 覆盖）。
   - "FakeExecEnv"在本仓库的落地方式：
     `hostinfo.Env` / `config.EnvFunc`（注入环境）、`config.ResolvePathsWith`（注入 goos/home）、
     `gitrepo.Runner` 接口（假命令执行器）、`internal/cli` 进程内测试（`Runtime.Run` +
     临时 `BS_*_HOME` + 缓冲区输出）、`scripts/smoke.sh`（真实二进制 + 沙箱目录）。
2. **`--dry-run` 必须真的什么都不做**：任何新增的破坏性操作都要先过 `Runner.DryRun`，
   并补一条"执行后目标路径不存在"的断言（参考 `internal/gitrepo/manager_test.go`、
   `pkg/cmdx/cmdx_test.go`）。
3. 幂等操作要断言"第二次执行不修改文件"（比较 mtime），参考 `internal/state/state_test.go`、
   `pkg/fsx/fsx_test.go`。
4. 涉及删改用户文件的逻辑，必须有一条"用户文件内容未被破坏"的测试。
5. 提交前跑 `make check` 与 `make smoke`；两者都绿才算完成。

## 5. 代码风格

- 注释用中文，解释**为什么**这么做，而不是复述代码；对外可见的类型/函数都要有注释。
- 错误消息用英文短句（便于搜索），面向用户的提示可以用中文。
- 错误要带上下文，返回时用 `fmt.Errorf("...: %w", err)`；退出码映射集中在 `internal/cli/exit.go`。
- 平台相关分支尽量收敛到 `pkg/hostinfo`（探测）或 `internal/config/paths.go`（目录策略），
  不要在业务代码里散落 `runtime.GOOS` 判断。
- 新增外部依赖前先看有没有标准库/已有依赖能覆盖；引入后跑 `go mod tidy` 并确认 `// indirect` 标记正确。

## 6. 新增能力的落点

| 要加的东西 | 放哪里 |
| --- | --- |
| 新的 CLI 子命令 | `internal/cli/<域>.go` + `internal/app/<域>.go` |
| 新的纯算法（解析/比较/渲染） | `pkg/<名字>`，附表驱动测试 |
| 新的外部工具调用 | 通过 `pkg/cmdx`；只读探测走 `LookPath`/`RunCapture` |
| 新的持久化状态字段 | `internal/state`，并保证内容不变时不写盘 |
| 新的平台差异 | `pkg/hostinfo` 或 `internal/config/paths.go` |
| 新的交互式提问（勾选/确认/输入） | `internal/ui` 的 `Prompter`（**只有这个包能 import huh**，纯文本实现必须同步可用） |
| 新的内置 shell 片段 | `tools/snippets/<posix|fish|pwsh>/NN-名字.{sh,fish,ps1}` |
| 新的内置工具清单条目 | `tools/{common,linux,darwin}.yaml`（字段说明见 `docs/tool-schema.md`） |
| 新的工具套装（profile） | 跨平台的写 `tools/profiles.yaml`，有平台差异的写 `tools/<os>.yaml` |
| 新的安装方式（`via`） | `internal/install/` 里新增 Installer 并注册到 `NewRegistry`，同时补 `docs/tool-schema.md §7` |

### 内置 shell 片段约定

- 文件名前缀是两位数字，决定加载顺序（`10-`、`20-`、`90-`）。
- 用户片段放在 `<config>/shell/<family>/`，**同名文件整体覆盖内置片段**。
- 片段会被 `bs shell init <shell>` 内联进 rc 的受管块，因此：
  - 不要依赖其它片段的执行顺序以外的假设；
  - 保持幂等（每次开 shell 都会执行）；
  - 只读探测要 `command -v` 判空，别让 shell 报错。

## 7. 代理与 shell 集成的既有决策（不要推翻）

- 子进程**不能**修改父 shell 的环境变量。所以"当前 shell 立即生效"用**同名 shell 函数包装**
  实现（`tools/snippets/*/10-proxy.*`）：只拦截 `bs proxy on|off|use`，其余子命令直通。
- rc 里只允许出现一个受管块，标记固定为：
  ```
  # >>> bootstrap (managed by 'bs shell install') >>>
  ...
  # <<< bootstrap <<<
  ```
  块外内容必须字节级不变；`Upsert`/`Remove` 的行为由 `internal/shellsnip` 的单测锁定。
- `bs proxy off` 是**显式关闭**（state 里记 `mode=off`）：此后不再回退到环境变量，
  直到用户重新 `bs proxy use <name>`。
- 凭据只能在日志/输出里以 `***` 出现（`proxyenv.MaskProxyURL`、`cmdx.Runner.Redact`）。

## 8. 安全红线

1. **绝不删除或覆盖用户已有内容**：目标目录非空且不是 git 仓库时，`bs repo get` 只 `skip` 并提示。
2. 修改用户文件（rc、配置文件）之前先备份（`fsx.BackupFile`），并优先用 `AtomicWriteFile`。
3. 下载内容必须校验 sha256；校验文件拿不到就中止（见 `scripts/install.sh`）。
4. `bs doctor` **默认只读**：不创建目录、不写配置、不发网络请求；`--fix` 才动手，而且只修
   "结论明确、修法唯一"的问题（生成配置、建仓库根、装 shell 受管块），走 `confirmDestructive`
   那套同意规则并支持 `--dry-run`。**只读**那部分的细则：网络检查只放在 `bs proxy test`。
   唯一的写动作是"目录可写性探测"——在最近的已存在祖先目录里创建并立刻删除一个临时文件。
   另外它**不执行** `go version`：Go 工具链会借此往 `~/.config/go/telemetry` 写计数文件
   （`GOTELEMETRY=off` / `GOTELEMETRYDIR` 都拦不住），这与"只读"冲突。
5. 新增破坏性命令时，必须在 `--dry-run` 下走通，并在 README/`--help` 里说明。
6. **破坏性操作必须先拿到明确同意**（`internal/cli` 的 `confirmDestructive`）：
   交互终端问一句（默认拒绝），非交互（管道/CI/`--json`/`--non-interactive`）**绝不阻塞**，
   没有 `--yes` 就以退出码 5 结束。新增这类命令时照抄 `bs tool install` 的做法。

## 9. 提交与文档

- 提交信息用 `feat:` / `fix:` / `docs:` / `build:` / `test:` / `refactor:` 前缀，正文写清动机与影响。
- 改动了 CLI 表面（命令、参数、退出码）→ 同步更新 `README.md`。
- 改动了架构、分层或关键决策 → 同步更新 `docs/design.md`（必要时追加 ADR）。
- 新增/修改配置字段 → 同步更新 `docs/configuration.md`。
- 改动清单字段或 `via` 参数 → 同步更新 `docs/tool-schema.md`（清单作者看的唯一权威说明）。

### 写工具清单的硬纪律（M2 起）

1. **优先交给已有的包管理器**：能 `brew`/`apt`/`npm`/`cargo`/`uv` 装，就别下官方二进制。
2. **`via: release` 只在仓库确实发布校验和时使用**（实测：voidint/g 用 `sha256sum.txt`、
   lazygit 用 `checksums.txt`、k9s 用 `checksums.sha256`、uv/ruff/ripgrep 每个产物配 `<asset>.sha256`；
   fnm/bat/fzf 的 tar 包**没有**校验和）。拿不到校验和就中止，因此这类工具走管理器。
   内置清单里**不使用** `via: script`（官方脚本没有稳定哈希，用 `insecure` 等于放弃校验）。
3. **不静默提权**：系统包管理器会被护栏拦住（默认只允许 `devdep`/`service`/`cli`），
   确实需要时显式写 `params.allow_system: true` 并在注释里说明理由。
4. **平台差异用 `when` 表达，不要复制 id**；只有"整个工具只属于某个系统"才写进 `<os>.yaml`。
5. **改完必须校验**：`bs tool manifest validate`（含 `--os linux|darwin|windows` 交叉校验），
   再 `bs tool plan` 看实际会选哪个方式。清单字段见 `docs/tool-schema.md`。
6. **"名字类"参数可以引用 `{{versions.*}}`**，但只限 `version`/`package`/`formula`/`cask`/
   `crate`/`module`/`id`/`python`/`version_source`/`source`/`repo`（引擎会提前展开）；
   `asset`/`url`/`sha256`/`dest`/`build` 里的 `{{version}}` 由 installer 在版本确定后渲染，
   这两类不能混用——写错了 `bs tool plan` 里会直接看到字面量 `{{...}}`。

## 10. 已知未完成项

- **Windows 工具清单尚未编写**（`tools/windows.yaml` 为空，ADR-0014）：只保证能加载、不报错。
- Windows 的 `repo.root` 默认值仍待细化（设计文档 §18.3）。
- 内置清单未覆盖 `fd`/`bat`：Ubuntu 上二进制名是 `fdfind`/`batcat`，与其它平台不一致，
  等"同一 id 在不同平台暴露不同命令名"的需求明确后再处理。
- 交互式取消只认 **Ctrl-C**：huh 的 ESC 在可过滤列表里用于"退出过滤/清空输入"，
  不会取消整个表单（纯文本路径按 `q` 取消）。非 TTY（管道/CI）一律走纯文本或直接拒绝。
- 交互式界面的按键行为**无法在单测里验证**（需要真 PTY，见 §4 第 0 条），
  改动 `internal/ui` 后除了 `make check` 还要跑 `make pty`。
- 真实下载/安装的验证脚本是 `scripts/e2e-install.sh`（`make e2e`，需要网络，不进 `make smoke`）；
  它的离线替身是 `internal/install/artifacts_e2e_test.go`（httptest 冒充 GitHub +
  真 httpx + 真解压），改 release 路径时优先扩充这条测试。
- 下载**没有断点续传**：中途断线要从头再来（`httpx.do` 只重试建连阶段），
  慢网络上限速代理下体验不好。
- 代理注入的语义见 `internal/proxy.Service.NetworkEnv`：`override_existing=false` 时
  **同一组变量用 shell 里的值**，绝不能"这一组就不注入"——`httpx` 把注入的 env 当权威，
  缺键即等于直连。改动这块务必跑 `internal/proxy` 的单测与 `make e2e`。
- `make e2e` 走网络，**默认依赖 `build`**（脚本自己不做构建）；它会在沙箱里真下载
  release 归档并校验 sha256，因此既能验证网络路径，也最容易暴露清单里的
  `strip_components`/asset 名写错。
