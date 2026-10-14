# AGENTS.md —— 给 AI/协作者的工程约定

本文件是这份仓库的"施工图"。改代码前先读完本页与 [docs/design.md](docs/design.md) 的相关章节。

## 1. 这个项目是什么

`bs`（仓库名 Bootstrap）是维护者个人的开发环境重建工具：新机器上用它拉回仓库、
恢复代理与 shell 环境、按清单安装/更新工具链。
M1–M4 已完成（`bs init` 一把梭 + 自更新；以及 `--timeout`、`-i` 勾选、下载断点续传、
`bs doctor --fix`、`config get|set|edit`、`tool upgrade --all`、`make lint`/`make vuln`、
`make docker-matrix` 裸机矩阵、`make release-check` 发布彩排、GitHub 镜像前缀）。
现在处于 M5：按 stack 持续补清单，尚未做的主要是 Windows 清单（见 §10）。

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
make build         # bin/bs
make test          # 单元测试
make race          # -race
make smoke         # 无副作用端到端冒烟（关键回归保护）
make pty           # 真 PTY 里验证交互式界面（勾选/取消/破坏性操作默认拒绝）
make check         # gofmt + vet + test + race（提交前必跑）
make lint          # golangci-lint 静态检查（需先 `make tools-lint`；刻意不进 check，但进 CI）
make vuln          # 依赖已知漏洞扫描（需联网查 vuln.go.dev；改动依赖后跑，也进 CI）
make coverage-check # 单元测试 + 覆盖率门禁（COVER_MIN，默认 70；进 CI）
make tools         # 装齐 lint/vuln/release 的二进制（版本固定在 Makefile 里）
make tools-lint / tools-vuln / tools-release   # 只装其中一组（CI 各门禁只装自己那份）
make cross         # 交叉编译 6 个平台组合
make e2e           # 真实下载安装（需要网络，不进 check）
make docker-matrix # 裸机行为矩阵：静态二进制丢进 ubuntu/debian 镜像跑（需 Docker + 拉镜像）
make docker-integration # 干净镜像 + 真网络：apt 路径、Release 下载+sha256、直链、代理全链路（需 Docker + 外网）
make snapshot      # goreleaser 本地打包快照（产物在 dist/）
make release-check # 发布流程彩排：打包后用 install.sh 从本地站点真装一遍（不碰 GitHub）
```

`make snapshot` / `make release-check` 需要 goreleaser（版本固定在 `make tools` 里），
它得 `go install` 编译一大棵依赖，装一次即可。

`make lint` / `make vuln` / `make coverage-check` **不进本地 `make check`**：前两个慢/要联网，
覆盖率门禁则希望"改代码时不必先补测试"——都不该拦住"本地能跑 check 就能提交"。
但它们在 **CI 里都是门禁**（`.github/workflows/ci.yaml` 的 `lint` / `vuln` / `coverage` 三个 job）：
推送或 PR 时必须绿；工具版本只有 Makefile 一处定义，CI 用 `make tools-lint` / `make tools-vuln`
装同一版。覆盖率阈值 `COVER_MIN` 只许往上抬（写这份时总覆盖率 75.4%）。

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
   另有一条**只有干净系统才暴露**的测试轴：`scripts/docker-check.sh` 在 ubuntu/debian 最小镜像里
   以 `--network none` 跑一遍新机器流程（没有 git/curl/go，PATH 与环境变量全清）。
   改动"缺命令时怎么办""是否偷偷依赖宿主环境"这类逻辑后跑 `make docker-matrix`。
   另一条只有**真网络**才暴露的测试轴是 `scripts/docker-integration.sh`（`make docker-integration`）：
   在干净镜像里真装（apt 系统包、GitHub Release + sha256、`params.url` 直链）并验证**代理全链路**——
   用"把代理指向黑洞（`http://127.0.0.1:9`）必须失败"做反向对照，证明 bs 自己发起的下载真的走了代理，
   而不是拿"下载成功"当证据。容器里 github.com 直连不通，所以走代理的下载都排在代理配置之后。
   它慢、要外网、会真下几十 MB，因此**不进 `make check`**；代理地址用 `BS_PROXY_URL` 覆盖
   （**必须自己给**，公开仓库不内置任何内网地址；给空串则跳过代理阶段）。
4. 涉及删改用户文件的逻辑，必须有一条"用户文件内容未被破坏"的测试。
5. 提交前跑 `make check` 与 `make smoke`；两者都绿才算完成。
   动了 `go.mod` / 依赖，或准备发版时，再跑一遍 `make vuln`（需联网）。

## 5. 代码风格

- 注释用中文，解释**为什么**这么做，而不是复述代码；对外可见的类型/函数都要有注释。
- 错误消息用英文短句（便于搜索），面向用户的提示可以用中文。
- 错误要带上下文，返回时用 `fmt.Errorf("...: %w", err)`；退出码映射集中在 `internal/cli/exit.go`。
- 平台相关分支尽量收敛到 `pkg/hostinfo`（探测）或 `internal/config/paths.go`（目录策略），
  不要在业务代码里散落 `runtime.GOOS` 判断。
- **显式接收 `goos` 的函数，路径拼接必须用 `fsx.JoinFor(goos, …)`**，不能用 `filepath.Join`：
  后者按**当前宿主**选分隔符，于是"算 Windows 的 rc 文件"在 Linux 上会得到 `\home\k\.bashrc`
  这种只在维护者机器上"看起来对"的结果（CI 第一次跑就抓住了一批）。同理，
  判断归档条目/远端路径是否绝对要用 `path.IsAbs` 而不是 `filepath.IsAbs`
  （Windows 上 `filepath.IsAbs("/abs/x")` 是 false，恶意压缩包会被放行）。
- 测试里断言路径、权限位、假命令时，先问一句"这条断言在另一个平台上还成立吗"：
  宿主语义（`ExpandPath`、权限位）按宿主算，目标平台语义（配置目录、rc 文件）按目标算，
  只有 POSIX 才有前提的（符号链接需特权、无可执行位的文件不算命令）就 `t.Skip` 并写清理由。
- 新增外部依赖前先看有没有标准库/已有依赖能覆盖；引入后跑 `go mod tidy` 并确认 `// indirect` 标记正确。

## 6. 新增能力的落点

| 要加的东西 | 放哪里 |
| --- | --- |
| 新的 CLI 子命令 | `internal/cli/<域>.go` + `internal/app/<域>.go` |
| 配置文件的"按行修改"（get/set 一类） | `internal/config/edit.go` 的 `yaml.Node` 路径操作 + `internal/app/configedit.go` 的用例（写前备份、写后校验、失败还原） |
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
- 补全**复用这同一个受管块**（ADR-0016）：`bs completion install [shell]` 只做"把 `shell.completion`
  置真 + 确保受管块最新"两件事，补全片段由 `bs shell init` 现算，rc 里不会出现第二组标记。
  开关默认关（与 `auto_proxy` 同理：不擅自改变每个新 shell 的行为）。
- `bs proxy off` 是**显式关闭**（state 里记 `mode=off`）：此后不再回退到环境变量，
  直到用户重新 `bs proxy use <name>`。
- 凭据只能在日志/输出里以 `***` 出现（`proxyenv.MaskProxyURL`、`cmdx.Runner.Redact`）。

## 8. 安全红线

1. **绝不删除或覆盖用户已有内容**：目标目录非空且不是 git 仓库时，`bs repo get` 只 `skip` 并提示。
2. 修改用户文件（rc、配置文件）之前先备份（`fsx.BackupFile`），并优先用 `AtomicWriteFile`。
3. 下载内容必须校验 sha256；校验文件拿不到就中止（见 `scripts/install.sh`）。
   校验**永远覆盖整份文件**（续传时也一样），因此"续传"不会成为绕过校验的后门。
   归档/脚本下载到 `<cache>/downloads`（跨次运行稳定，这样才能续传与复用），
   断线时半成品 `.<名字>.<url 指纹>.part` 留在原处供下次带 `Range` 续下；
   两条硬规矩：**服务端忽略 Range（回 200）必须整体重下**（否则文件里会混两段内容）、
   **校验失败必须删掉半成品**（否则下次续传永远对不上）。缓存条目 30 天未用会在
   下次下载前清掉，`--dry-run` 不清理任何东西。
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
   产物不在 GitHub Release 上时用**直链模式**（`params.url` + `<url>.sha256`/`.sha256sum`），
   校验和同样是硬要求；细节见 `docs/tool-schema.md` 的"直链模式"。
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
- `tools/common.yaml` 里两个**故意留着的缺口**（别当成漏写，见 design §6.4）：
  `rust-analyzer` 在 Ubuntu/Debian 上暂无方式（官方 Release 不发布校验和，也不能用
  `via: release`；那边先用 `rustup component add rust-analyzer`）；`kustomize` 的 tag 带前缀
  （`kustomize/v5.8.3`）且校验和是共享的 `checksums.txt`，得先有 `trimprefix` 过滤器与
  "同目录校验和"支持。`kubectl`/`helm`/`gradle` 已用直链模式（`params.url`）解决。
- 新增/修改清单条目时，**包名与资产名必须实测**（apt/Arch/Fedora 的包名、brew formula、
  npm/crates/PyPI 包名、GitHub Release 资产名与校验和文件、归档有没有外层目录），
  不要照抄记忆里的名字——design §6.4 记了这一轮用到的查询手段。
- Windows 的 `repo.root` 默认值仍待细化（设计文档 §18.3）。
- 内置清单未覆盖 `fd`/`bat`：Ubuntu 上二进制名是 `fdfind`/`batcat`，与其它平台不一致，
  等"同一 id 在不同平台暴露不同命令名"的需求明确后再处理。
- `via: release` 的卸载只覆盖"单个可执行文件 + 清单里的软链"：`params.extract: true`（整包解压型，
  如 uv/gradle）与含 `{{version}}` 的 dest/binary/软链路径会明确报「不支持」并给出要手工删的目录。
  根治要记录"安装时写了哪些文件"（design §18.3）；在那之前**不要**为了让某条清单能卸载而递归删目录。
- 全新最小镜像（`ubuntu:24.04` docker 镜像）**没有系统 CA 证书**：所有 HTTPS 都会失败在 x509 上。
  `bs doctor` 因此有一条 `ca certs`（`pkg/hostinfo` 探测，含 `SSL_CERT_FILE`/`SSL_CERT_DIR`），
  `httpx` 也会在 x509 错误上补一句英文 hint；但**装系统包不是 bs 的事**（doctor 只给命令）。
  改动下载错误处理时别把这层提示弄丢。
- 交互式取消只认 **Ctrl-C**：huh 的 ESC 在可过滤列表里用于"退出过滤/清空输入"，
  不会取消整个表单（纯文本路径按 `q` 取消）。非 TTY（管道/CI）一律走纯文本或直接拒绝。
- 交互式界面**统一走备用屏幕**（huh 的 `tea.WithAltScreen()`，见 `internal/ui/huh.go` 的 `form()`）：
  表单铺满整屏、退出后终端恢复原样。注意 `WithProgramOptions` 是**替换**而非追加，
  必须排在 `WithInput`/`WithOutput` 之前。
- **huh 的 `Height(n)` 是整段字段的高度**，不是"列表能看到几行"：
  `viewport = n − 标题行数 − 说明行数`（见 huh 的 `updateViewportHeight`）。
  给 4 个选项写 `Height(4)`，第 4 个就被挤出可视区、要滚动才够得着（用户实测反馈）。
  `internal/ui` 的 `listHeight` 因此分两种：列表放得下就**不设 Height**（huh 按内容精确撑开，
  一行不多一行不少），放不下才 `终端高 − 标题/说明折行 − 4`（`chromeRows` 按显示宽度算，
  CJK 用 `pkg/textwidth`；留 4 行给 huh 多画的那行空行、帮助行和一点余量）。
- **列表高度必须跟着终端窗口走**：huh 自己会按 `WindowSizeMsg` 重排宽度与分组高度
  （它从不清 `f.width`/`f.height`，`Form.Update` 里那两个 `if == 0` 分支其实每次都进），
  但 `Select/MultiSelect.Height()` 是我们在**表单启动之前**按当时终端算死的常量——
  于是用户把窗口拉大后依然只看到原来那么多行。所以 `huhPrompter.form()` 挂了
  `tea.WithFilter`，在消息进 `Form.Update` 之前用新尺寸重算（`listField.resize`）；
  过滤器的返回值不变，huh 的语义一点没动。改这块必须跑 `make pty`，
  里面有一条 `--resize` 回归断言（24 行终端看不见、拉到 50 行才露出来的那一条）。
- `scripts/pty-check.py` 会先把伪终端设成 `--cols × --rows`（默认 100×30）再 exec：
  0×0 的终端里 huh 会把整段表单渲染成空、只剩帮助行，于是"选项看不看得见"这类回归
  根本测不出来（改窗口尺寸的坑就是这么发现的）。它还支持 `--resize COLSxROWS`
  （配合 `--resize-after`）在运行中改窗口尺寸并送 SIGWINCH，用来验证"跟着终端重排"。
- 容器里常常没有 `$SHELL`（`docker run -it ubuntu bash` 就是）：`hostinfo.detectShell`
  依次退回 shell 自身环境变量、`/etc/passwd` 的登录 shell、父进程名（`/proc`）；
  **都拿不到就返回空串**，调用方必须当成"没有 shell 可集成"处理（`bs init` 会跳过并提示），
  绝不猜一个再去写人家的 rc。改动这段除了单测还要跑 `make docker-matrix`。
- `bs init` 的向导只问三件事（仓库根目录/代理地址/装哪些工具，见 ADR-0015），
  配置文件缺失时**直接生成、不再询问**；`--yes` 是"全自动"（连选什么都不问）。
  改这条链路时注意：`Auto` 与 `AssumeYes` 是两件事——前者管"问不问选择"，
  后者管"要不要确认"，非交互环境仍然必须 `--yes`（AGENTS §8.6）。
- 交互式界面的按键行为**无法在单测里验证**（需要真 PTY，见 §4 第 0 条），
  改动 `internal/ui` 后除了 `make check` 还要跑 `make pty`。
- 探测（`tool list`/`plan`/`init`）会执行清单里的 `verify.command`，而**工具自己的命令可能有副作用**：
  Go 1.26 上 `go version` 会往 `~/.config/go/telemetry` 写计数文件，`GOTELEMETRY=off` /
  `GOTELEMETRYDIR` 实测都拦不住（`go env GOTELEMETRY` 仍报 `local`）。所以：`bs doctor` 干脆
  不跑 `go version`（§8.4），`tool plan` 的提示语说的是"不安装或升级"，而不是"绝对不执行任何东西"；
  探测结果有 TTL 缓存，正常使用不会反复触发。
- 探测缓存（`<cache>/probe.json`）：计划阶段的探测会写缓存，因此 `Engine.Apply` 每执行完一步
  都要刷新缓存（install/upgrade 用刚探测到的结果 Merge，uninstall 无条件按 id 失效，
  见 design §6.5）。改"装完/卸完立刻可见"这类行为时，注意 `Step.Detection` 里可能还是
  **计划阶段**的结果，别直接 Merge。
- **Windows 的覆盖边界**（CI 的 windows-latest 已跑通单测与 `scripts/smoke.ps1`，但仍有洞）：
  - `internal/cli` 里需要"假管理器把新命令装出来并 chmod +x"的那组 `bs init` 用例在 Windows 上
    `t.Skip`（假管理器是 POSIX 脚本）——真机上的 `bs init` 行为还没被验证过。
  - `internal/detect` 的目录 provides 回退在 Windows 上只补 `.exe`，npm 那种 `.cmd` shim
    识别不了（可能把已装的工具报成 missing）。
  - 假命令在 Windows 上只能写成 `.cmd`（`writeFakeCmd` / `pkg/cmdx` 的测试已这么做）；
    但"裸命令名 + 设置了 `cmd.Dir`"时 Go 的解析路径与 PATH 查找不同，
    需要真机调试，别在本地靠猜改这部分断言。
  - `scripts/smoke.ps1` 的断言数远少于 `smoke.sh`（Windows 清单本身也没写，见上）。
- **macOS 的 `/bin/bash` 是 3.2**：脚本里变量展开必须写 `${var}`，不能 `$var` 紧跟非 ASCII 字符
  （`$code）` 会被当成变量名，报 `unbound variable`）。CI 的 ubuntu 用 bash 5 看不出来。
- **发布前跑 `make release-check`**（`scripts/release-check.sh`）：它不碰 GitHub，把 `dist/` 摆成
  `<base>/<tag>/…` 用本地 HTTP 服务冒充下载站，让 `scripts/install.sh` 真装一遍；再用
  `BS_GITHUB_API`/`BS_GITHUB_ASSET_BASE` 把**自更新**指向同一套本地站点（现场编译一个更高版本
  发上去），验"更新成功 + 旧版本留备份"与"篡改归档 / 缺 checksums.txt / 版本不存在"三条失败路径
  必须中止、且当前二进制一动不动、不多留备份。
  产物比源码旧时脚本会 `warn`（改了代码忘了重打包会给出假结论）；`dist/` 里归档带的
  `scripts/install.sh` 必须与仓库字节一致，否则直接判失败。
  改 `scripts/install.sh`、`.goreleaser.yaml`（产物名/归档内容）或安装目录策略后必须跑它。
  注意 `make snapshot` 会 `--clean` 整个 `dist/`，`make cross` 的产物随之消失
  （`make docker-matrix` 依赖 `cross`，会自动重建）。
- 真实下载/安装的验证脚本是 `scripts/e2e-install.sh`（`make e2e`，需要网络，不进 `make smoke`）；
  它的离线替身是 `internal/install/artifacts_e2e_test.go`（httptest 冒充 GitHub +
  真 httpx + 真解压），改 release 路径时优先扩充这条测试。
- GitHub 镜像前缀（`BS_GITHUB_API` / `BS_GITHUB_ASSET_BASE`，见 `internal/app/applyGitHubMirror`）
  只认环境变量、不落配置，`app.New` 里统一应用一次；它们同时决定产物与 `checksums.txt` 的来源，
  **等于把 GitHub 的信任转交给镜像**（design §16 第 7 条）。改动下载路径时别绕过
  `pkg/ghrelease` 自己拼 github.com，否则镜像和彩排都会漏掉这条路径。
- 代理注入的语义见 `internal/proxy.Service.NetworkEnv`：`override_existing=false` 时
  **同一组变量用 shell 里的值**，绝不能"这一组就不注入"——`httpx` 把注入的 env 当权威，
  缺键即等于直连。改动这块务必跑 `internal/proxy` 的单测与 `make e2e`。
- `make e2e` 走网络，**默认依赖 `build`**（脚本自己不做构建）；它会在沙箱里真下载
  release 归档并校验 sha256，因此既能验证网络路径，也最容易暴露清单里的
  `strip_components`/asset 名写错。
- `make docker-matrix` **依赖 `cross` 而不是 `build`**：本地 build 是动态链接的，
  容器里可能因 libc 版本而失败，而发布产物正是 cross 那一份（`CGO_ENABLED=0`）。
  容器内 `--network none`：所以"在裸机上能跑"这件事不是靠网络蒙混的。
- **cobra 的补全命令在构造时就抓走了 writer**：`InitDefaultCompletionCmd()` 内部有
  `out := c.OutOrStdout()`（闭包捕获），所以 `newRootCmd` 里必须**先** `cmd.SetOut/SetErr`、
  **后**调用它——否则 `bs completion <shell>` 的脚本会写到 `os.Stdout`，
  进程内测试（`Runtime.Run` + 缓冲区）拿到空输出，`--json`/重定向也会失效。
  同理，`__complete` 这条隐藏命令在 `Execute` 时才挂上命令树，拿不到注解，
  它在 `rt.init` 里只能按名字识别为"容忍配置损坏"（否则配置写坏一次，Tab 补全就整个不工作）。
