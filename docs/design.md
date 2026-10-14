# Bootstrap (`bs`) 整体功能与架构设计

> 版本：v0.3（设计评审收敛稿，已并入前两轮反馈）
> 目标读者：仓库维护者本人 + 后续接手开发的 AI Agent / 工程师
> 关联文档：[`AGENTS.md`](../AGENTS.md)（待生成）、[`README.md`](../README.md)（待生成）
>
> **修订记录**
> - v0.1：首轮整体功能与架构设计，需求 1–9 全覆盖。
> - v0.2：确立**管理器优先/薄实现**原则（Go 用 `g`，Node 用 `fnm`/`nvm`，Python 用 `uv`）；新增 `pkg/` 准入规则与 `internal/platform`、`internal/execx` 的取消；清单新增 `kind: versionmgr`、`depends_on`、`via: manager`、`enabled`；方法链新增 `--no-fallback` 与系统包管理器护栏；新增 §8.4 Shell 集成子系统（同名函数包装实现 `bs proxy on` 当前 shell 立即生效 + rc 受管块 + 用户片段目录）；取消 deb/rpm/brew/scoop/winget 分发；自更新仅 sha256；路线图改为"先打通 CLI 主干，再持续补清单"。
> - v0.3：shell 片段**主要随仓库内置**发布（ADR-0012）；fallback 默认值改为"交互开 / 非交互关"（ADR-0013）；首批清单收窄到 linux + darwin，Windows 延后（ADR-0014）。**设计评审收敛，可进入 M0。**

---

## 1. 定位与范围

`Bootstrap`（命令行名 `bs`）是一个**面向个人开发环境的一次性构建与长期可重复构建工具**。

一句话定义：

> 给定一台全新的（或半旧的）Windows / Linux / macOS 机器，`bs` 能把"我的开发环境"一键恢复到熟悉的状态，并在之后长期承担**工具安装/升级、代码仓库拉取/更新、代理切换、环境变量装配**这四件事。

### 1.1 必须解决的问题（对应原始需求 1–9）

| 编号 | 需求 | 归属子系统 |
|---|---|---|
| 1 | 输入 GitHub 仓库任意形态的 URL/简写，自动 clone 到 `<repo_root>/<host>/<owner>/<repo>`；目标已存在则转为 `git pull` | `gitrepo` + `bs repo` |
| 2 | 自动探测 OS/Arch（含 Linux 发行版、Shell），加载对应工具清单 | `platform` + `manifest` |
| 3 | 初始化时列出"已安装 / 未安装"，可交互勾选安装 | `bs init`：环境概览 + huh 多选（缺失项默认勾选）+ plan → apply（`bs tool list --installed/--missing` + `bs tool install` 仍是脚本化的等价入口） |
| 4 | 纯 CLI（无 GUI、无常驻服务） | 全局约束 |
| 5 | 执行下载 / clone 等网络操作时自动识别并注入代理 | `proxy` + `pkg/cmdx`/`httpx` |
| 6 | 集成 startproxy / chaitinproxy 能力，按平台生成可生效的代理环境变量 | `proxy` + `bs proxy` + `bs shell init` |
| 7 | 工具按**技术栈**（golang/python/node/rust/c/cpp…）与**功能类型**（包管理器/运行时/基础 CLI/系统开发依赖）分类组织 | `manifest` 分类模型 |
| 8 | 同一工具支持多种安装方式并有序降级：第三方包管理器（bun/npm/uv/cargo…）→ 官方脚本（curl \| bash）→ 系统包管理器（apt/brew/winget）→ 源码编译 | `install` 方法链 |
| 9 | 各 OS 的工具集合与安装方式独立可配，同时保留跨平台公共集合 | `manifest` 分层文件 |

### 1.2 明确不做（Non-Goals）

- 不做 GUI / 全屏常驻 TUI 应用；交互仅限于 `huh` 驱动的一次性向导（表单/多选），且在任何非交互场景必须可旁路。
- 不实现远程清单仓库与清单服务（ADR-0002）；清单随二进制发布 + 本地覆盖。
- 不替代 dotfiles 管理（`.bashrc`、`.gitconfig` 内容仍由用户/另一仓库管理）；`bs` 只负责**往里追加一小段受管片段**或输出可 `eval` 的文本。
- 不做系统级配置管理（Ansible/Nix 的领域），不接管服务编排、容器编排。
- 不内嵌 git 实现（不用 go-git），**直接调用系统 `git`**：可复用用户的 SSH key、credential helper、`.gitconfig`、代理配置，语义与手动操作完全一致。

---

## 2. 设计原则

1. **声明式优先，命令式兜底**：工具清单是数据（YAML），不是代码。新增一个工具 = 改一份 YAML，不写 Go 代码。只有当出现 YAML 表达不了的新**安装机制**时，才新增一个 Go `Installer`。
2. **管理器优先，委托优于重实现**（ADR-0005）：语言运行时/版本敏感的 SDK **一律优先交给成熟的版本管理器**——Go 用 `g`，Node 用 `fnm`/`nvm`，Python 用 `uv`，Rust 用 `rustup`，通用场景用 `mise`。`bs` 的职责是"把管理器装好，然后委托它"，而不是自己实现 `下载 → 校验 → 解压 → 切换 PATH → 多版本共存`。理由：这些管理器已经解决了版本切换、校验、镜像、卸载，自己重写只会又重又容易坏。
3. **薄实现**：每个 `Installer` 本质是"**命令模板映射器**"（清单参数 → 一条或多条命令），目标 ≤150 行、无状态、可被 Fake 完全测试。任何超过这个量级的逻辑都说明选错了抽象层（应该换用管理器、或者把它下沉到 `pkg/`）。
4. **Plan / Apply 两阶段**：任何有副作用的操作都要能先 `plan`（dry-run）后 `apply`，输出"将执行的确切命令"。
5. **幂等**：重复执行结果一致。`install` 对已安装且版本满足的工具是 no-op 或纯升级。
6. **探测优于记忆**：以实时探测（`which` + `--version` 解析）为唯一事实来源；状态文件只做缓存与审计。
7. **分层配置、就近覆盖**：内置默认 → 系统级 → 用户级 → 项目级 → 环境变量 → 命令行 flag，后者覆盖前者。
8. **面向 Agent 友好**：所有面向人输出的命令都提供 `--json`；退出码语义稳定；错误信息包含可执行的修复建议。
9. **可测试性内建**：所有外部副作用（执行命令、HTTP 下载、文件系统、时钟）都经接口注入，核心逻辑可在无网络、无特权的单元测试中完整覆盖。
10. **最小依赖**：核心依赖控制在 cobra / viper / yaml.v3 等少数几个；交互增强库属于"可选 UI 层"，不得渗入领域层。
11. **不静默提权**：需要 root 时明确告知并请求确认，绝不后台偷偷 `sudo`。
12. **纯逻辑下沉 `pkg/`**（见 §4.1）：与业务无关、可脱离本项目使用的纯逻辑（URL 解析、版本比较、shell env 渲染、命令执行）放 `pkg/`，让 `gitrepo`/`proxy`/`install`/`cli` 等模块共享同一份实现，避免各写一套。

---

## 3. 总体架构

### 3.1 分层视图

```mermaid
flowchart TB
    subgraph L1["① 命令层 internal/cli (cobra)"]
        C1[repo] --- C2[tool] --- C3[proxy] --- C4[config] --- C5[init/doctor/env/shell/self]
    end

    subgraph L2["② 应用层 internal/app (用例编排)"]
        A1[RepoService] --- A2[ToolService] --- A3[ProxyService] --- A4[EnvService] --- A5[BootstrapService]
    end

    subgraph L3["③ 领域层 internal/domain (模型 + 端口/接口)"]
        D1[Tool/Manifest] --- D2[Platform] --- D3[InstallMethod/Installer] --- D4[ProxyProfile] --- D5[RepoRef]
    end

    subgraph L4["④ 基础设施层 internal/{install,gitrepo,proxy,shellsnip,config,manifest,detect,httpx,ui} + pkg/*（纯逻辑）"]
        I1[CmdExecutor] --- I2[Downloader] --- I3[GitClient] --- I4[StateStore] --- I5[ConfigLoader] --- I6[Reporter]
    end

    L1 --> L2 --> L3 --> L4
```

**依赖方向单向向下**：`cli → app → domain ← infra`（infra 实现 domain 定义的接口），`pkg/*` 是可被任意层使用的纯逻辑底座。`domain` 不 import 任何 `internal/*`；`cli` 不含业务逻辑（只做参数绑定、调用 service、渲染结果）。

### 3.2 进程内的关键数据流

```
读取配置(viper) + 平台探测 + 清单加载(embed/用户覆盖)
                │
                ▼
        ┌───────────────┐
        │  Planner      │  生成 []Action{Target, Method, Command, Reason}
        └───────┬───────┘
                ▼
        ┌───────────────┐   人: 表格 + 交互勾选/确认
        │  Presenter    │   Agent: --json
        └───────┬───────┘
                ▼
        ┌───────────────┐   注入 proxy env / PATH / sudo 策略 / 超时 / dry-run
        │  Executor     │  逐 Action 执行 → Verify → 记录 State
        └───────┬───────┘
                ▼
          Report（成功/跳过/失败 + 建议）
```

---

## 4. 目录结构

```
Bootstrap/
├── cmd/bs/main.go                 # 仅：Runtime.Run(os.Args) → os.Exit(code)
├── internal/
│   ├── cli/                       # cobra 命令层（每域一文件，薄：解析参数 + 调 app + 渲染）
│   │   ├── root.go                # 全局 flag、Runtime 装配、表格/JSON 输出出口（emit/newTable）
│   │   ├── exit.go                # 错误 → 退出码（0..5）的唯一映射点
│   │   ├── config.go  doctor.go  version.go  env.go  proxy.go  repo.go
│   │   └── shell.go  tool.go  manifest.go
│   ├── app/                       # 用例编排（无 cobra、不直接打印，输出走注入的 writer）
│   │   ├── app.go                 # App 装配：paths/config/platform/runner/logger/Manifests
│   │   ├── tool.go                # list/info/plan/install/upgrade/uninstall/validate/diff
│   │   ├── repo.go  proxy.go  shell.go  subshell.go  env.go  doctor.go  helpers.go
│   ├── manifest/                  # 清单模型 + 分层装载合并 + 校验 + 依赖图（"domain"层）
│   │   ├── types.go               # Tool/Method/Verify/Params/Profile/When/Origin
│   │   ├── load.go                # embed 内置 + 用户覆盖 + 合并（KnownFields 严格模式）
│   │   ├── validate.go            # 全量校验（一次报全）+ 系统包管理器护栏
│   │   └── graph.go               # TopoOrder / ExpandDeps / Problems 聚合
│   ├── install/                   # 安装引擎 + 全部 Installer 实现 + 注册表
│   │   ├── ports.go               # Op/Action/Download/ExecEnv/Installer/Registry/错误类型
│   │   ├── chain.go               # 方法链选择、--via 强制、降级候选
│   │   ├── engine.go              # PlanTools/Apply 流水线、Step/Report、Recorder、LogPath
│   │   ├── managers.go            # 版本管理器表：g/fnm/nvm/uv/rustup/mise/pyenv
│   │   ├── pkgmanagers.go         # bun/npm/pnpm/pipx/uvpm/cargo/goinstall/brew
│   │   ├── system.go              # apt/dnf/yum/pacman/zypper/apk/winget/scoop/choco（一份实现多 via）
│   │   ├── artifacts.go           # script/release/source（下载、校验、解压、软链、克隆）
│   │   ├── render.go              # {{...}} 模板渲染（变量/过滤器）
│   │   ├── exec.go  files.go      # argv 拆分与 sudo 前缀、文件落盘辅助
│   │   └── osenv.go               # ExecEnv 的唯一真实实现（所有系统接触点）
│   ├── detect/                    # 四态探测 + 5s 超时 + 并发 + TTL 缓存（cache.go）
│   ├── gitrepo/                   # clone/pull/sync 编排（用 pkg/giturl + 系统 git）
│   ├── proxy/                     # profile 解析、state 读写、系统代理探测、连通性测试
│   ├── shellsnip/                 # 内置/用户片段装载、排序、受管块拼装（rc.go/fs.go）
│   ├── config/                    # paths.go 目录策略 + load.go viper 装载 + types.go + validate.go
│   ├── state/                     # state.yaml：活跃 proxy profile、安装记录路径等
│   └── buildinfo/                 # Version/Commit/Date（ldflags 注入，version 命令读取）
├── pkg/                           # 纯逻辑库：无业务语义、不 import internal、可脱离本项目复用
│   ├── giturl/                    # 仓库 URL → Ref{Host,Owner,Repo,Ref,SubPath} + layout 渲染
│   ├── version/                   # 版本抽取(命名组正则)/比较/约束
│   ├── proxyenv/                  # Profile → bash/zsh/fish/pwsh/cmd/dotenv/json 的 env 文本
│   ├── hostinfo/                  # OS/Arch/Distro/Shell/包管理器探测 → Platform
│   ├── cmdx/                      # 命令执行：dry-run/超时/env 注入/日志脱敏
│   ├── httpx/                     # 带代理与超时的下载、sha256 校验
│   ├── ghrelease/                 # GitHub Release 元数据（最新 tag、asset、校验和文件名候选）
│   ├── archive/                   # tar/zip/gz 解压与"从归档里找出可执行文件"
│   └── fsx/                       # 原子写、~ 展开、目录确保、内容不变不写盘、备份
├── tools/                         # 内置工具清单 + 内置 shell 片段（go:embed 进二进制）
│   ├── embed.go                   # //go:embed 清单与 snippets
│   ├── common.yaml                # 跨平台公共工具（38 条）
│   ├── linux.yaml  darwin.yaml  windows.yaml   # 平台专属工具与平台专属 profile（windows 目前是占位）
│   ├── profiles.yaml              # base/golang/nodejs/python/rust/rust-dev/java/editor/infra/agent/managers
│   └── snippets/{posix,fish,pwsh}/…# 10-proxy.* / 20-path.* / 90-aliases.sh
├── docs/design.md  tool-schema.md  configuration.md   # 决策记录（ADR）以 §18 的形式包含在 design.md 里
├── scripts/                       # devenv.sh smoke.sh smoke.ps1 install.sh install.ps1 e2e-install.sh
├── .github/workflows/{ci.yaml,release.yaml}
├── .goreleaser.yaml  Makefile  go.mod
├── README.md  AGENTS.md
└── bin/  dist/                    # 本地构建产物（bin/bs、交叉编译归档），不入库
```

> 与早期设计稿的差异：原计划里的 `internal/domain/`、`internal/install/manager/`、
> `internal/install/{bun,npm,apt,…}.go` 单文件、`internal/ui/`、`internal/httpx/` 都被合并或上移了——
> 模型进了 `internal/manifest`，管理器与包管理器改成**表驱动的一份实现**，
> `httpx` 与新增的 `ghrelease`/`archive` 落在 `pkg/`；`internal/ui` 在 M3 建起来了，
> huh 只被它 import（见 §12）。
> 依据是 §4.1 的准入规则：能表驱动的就不要拆文件，能脱离业务复用的就上移 `pkg/`。

### 4.1 `pkg/` 准入规则（回答"常见方法是否抽成 pkg"）

**结论：是，但要克制。** 只把满足以下三条的代码放 `pkg/`，其余一律留在 `internal/`：

1. **纯逻辑**：不读全局状态、不 import `internal/*`、输入输出都是值对象；
2. **有独立意义**：脱离 `bs` 这个程序仍然成立（甚至可被别的仓库 import）；
3. **无业务词汇**：代码里不出现 `Tool`/`Manifest`/`Profile`/`Bootstrap` 这类概念。

按此规则，**上移 `pkg/` 的是**：`giturl`（URL 归一化，最典型的共用逻辑）、`version`（版本抽取与比较，被 detect/upgrade/清单校验共用）、`proxyenv`（被 `proxy`、`env`、`shell` 三处共用）、`hostinfo`（平台探测，被 config/manifest/install/doctor 共用）、`cmdx`（命令执行 + dry-run + 脱敏，被所有 installer 共用）、`fsx`（原子写与路径展开，被 config/state/shell 共用）。

**留在 `internal/` 的是**：清单语义、方法链决策、state 记录、rc 受管块拼装——它们都带业务含义。若将来某模块被三个以上模块需要且仍然纯粹，再上移，成本很低（只是改 import 路径）。

> 因此原先设计里的 `internal/platform` 与 `internal/execx` 被取消，分别由 `pkg/hostinfo`、`pkg/cmdx` 承担；`internal/gitrepo` 保留为"调用系统 git 的编排者"，URL 解析委托 `pkg/giturl`。这直接回应"不要在 bootstrap 里堆沉重实现"。

---

## 5. 配置体系

### 5.1 配置目录策略（跨平台）

优先级（高 → 低），第一个可用者为主目录：

| 平台 | 主配置目录 | 兼容读取（只读） |
|---|---|---|
| Linux | `$XDG_CONFIG_HOME/bootstrap` → 缺省 `~/.config/bootstrap` | `/etc/bootstrap` |
| macOS | `$XDG_CONFIG_HOME/bootstrap` → 缺省 `~/.config/bootstrap`（**与 Linux 统一**） | `/Library/Application Support/Bootstrap`（仅系统级） |
| Windows | `%AppData%\bootstrap` | `%ProgramData%\Bootstrap` |

覆盖规则：环境变量 `BS_CONFIG_HOME` > `BOOTSTRAP_CONFIG_HOME` > 平台默认；不引入其他私有约定，便于用户脚本与 Agent 定位。

> **ADR-0001（已决策）**：macOS **不**使用 `~/Library/Application Support/Bootstrap` 作为主目录，而是与 Linux 统一到 `~/.config/bootstrap`。理由：与 `gh`/`docker`/`kubectl`/`cargo` 等现代 CLI 惯例一致，跨机搬运/备份配置只有一条路径，文档与 Agent 无需分叉；代价是不符合 Apple 官方 Human Interface Guidelines——本项目是开发者 CLI，明确选择前者的惯例一致性。

配套目录：

| 用途 | Linux/macOS | Windows |
|---|---|---|
| config | 见上表 | `%AppData%\bootstrap` |
| shell 片段（受管） | `<config>/shell/` | `<config>\shell\` |
| state（活跃 profile、安装记录） | `$XDG_STATE_HOME/bootstrap` → `~/.local/state/bootstrap` | `%LocalAppData%\Bootstrap\state` |
| cache（下载缓存、探测缓存） | `$XDG_CACHE_HOME/bootstrap` → `~/.cache/bootstrap`（`downloads/` 可随时删除） | `%LocalAppData%\Bootstrap\cache` |

`bs config path` 打印全部实际路径（含来源层级），`bs config path --json` 供脚本消费。

### 5.2 分层合并

```
内置默认值(代码内 defaultConfig)
  → 系统级  <system>/config.yaml
  → 用户级  <config>/config.yaml
  → 项目级  $PWD/.bootstrap.yaml（仅当存在于当前目录树，用于按项目覆盖 repo_root）
  → 环境变量：**逐个显式绑定**的表（`internal/config/load.go` 的 `envBindings`），不是 AutomaticEnv
  → 命令行 flag（viper.BindPFlag）
```

实现：**viper 只负责"配置"这一件事**。工具清单是数据而非配置，用 `gopkg.in/yaml.v3` 直接解析（避免 viper 的大小写折叠、类型猜测带来的坑）。viper 装载后用 `Unmarshal(&Config{})` 落到**强类型结构体**，随后走 `Validate()`；业务代码永远只读结构体，不读 `viper.Get("x.y")`。

环境变量之所以用固定映射表而不是 `AutomaticEnv`/`SetEnvKeyReplacer`：后两者在
`Unmarshal` 路径上行为不可靠（大小写与键名折叠的坑），而显式绑定让"哪些变量被支持"
一眼可查、可测试（`config.ResolvePathsWith` + 注入的 `EnvFunc`）。变量清单见
[configuration.md](./configuration.md) §1。

### 5.3 命令行改配置（`config get|set|unset|edit|keys`）

目标：日常调参不必"打开编辑器 + 猜键名 + 担心写坏"。几条定死的规矩：

- **只改用户级配置文件**。系统级要 root、项目级属于某个仓库、环境变量是临时覆盖——
  在这三个地方"帮忙"写值，等于制造一份下次找不到原因的状态。
- **只改那一行**。用 `yaml.Node` 原地改而不是"反序列化成 map 再序列化"：后者会把用户的
  注释、键顺序、空行全丢掉；用命令改一个值却重写整份配置，是最容易被抱怨的行为。
- **类型由内置默认配置推断**。`ui.confirm` 只收 true/false、`repo.clone.depth` 只收整数；
  写错类型在**落盘之前**就报错。模板里没有的键（用户自建的 `proxy.profiles.<名字>`）
  按 YAML 标量规则自动判断，列表/映射则要求写 YAML/JSON。
- **不留下坏配置**。写前 `fsx.BackupFile`，写后立刻用**真实装载路径**（含派生字段与
  `Validate()`）复核一遍；不合法就把原内容放回去并报错。绝不能出现"改完 bs 起不来"。
- **`get` 读生效值**，`-v` 时说明"这个值来自哪个文件"；没有配置文件提供时回落到内置默认值。
  少数键的生效值是运行时算出来的（`repo.root` 会在留空时按平台约定自动挑一个），
  `get` 会把它标成 derived 而不是显示一个空串。
- **`edit` 需要真终端**（退出码 5），退出编辑器后立刻校验：**不回滚**用户手改的内容（那是
  用户自己的决定），只把问题全列出来，并把改前备份路径打给他。
- `--dry-run` 依然零写入：连"顺手生成一份默认配置"都不做。

### 5.4 配置示例

```yaml
# ~/.config/bootstrap/config.yaml
version: 1

log:
  level: info          # debug|info|warn|error
  color: auto          # auto|always|never

repo:
  root: /data/src       # linux/mac；windows 例：D:/src
  layout: "{{.Host}}/{{.Owner}}/{{.Repo}}"   # 目录布局模板（可覆盖）
  default_host: github.com
  hosts: [github.com, gitlab.com, gitee.com, codeberg.org, bitbucket.org]
  clone:
    protocol: https     # https|ssh
    depth: 0            # 0 = 完整历史
    extra_args: []
  pull:
    strategy: ff-only   # ff-only|rebase|merge
    autostash: false
    prune: true
  sync:
    jobs: 4

proxy:
  default_profile: local
  auto_apply: true      # bs 自身发起的网络操作自动注入代理
  override_existing: false
  profiles:
    local:
      http_proxy:  http://127.0.0.1:7890
      https_proxy: http://127.0.0.1:7890
      no_proxy: "127.0.0.1,localhost,10.2.0.0/16,192.168.0.0/16,chaitin.net,chaitin.cn,.chaitin.net,.chaitin.cn,.baizhi.cloud"
    chaitin:
      http_proxy:  http://proxy.in.chaitin.net:8123
      https_proxy: http://proxy.in.chaitin.net:8123
      no_proxy: "127.0.0.1,localhost,10.2.0.0/16,192.168.0.0/16,chaitin.net,chaitin.cn,.chaitin.net,.chaitin.cn,.baizhi.cloud"

tool:
  manifest:
    builtin: true                 # 使用二进制内置清单（go:embed）
    user_dirs: ["~/.config/bootstrap/tools"]   # 用户扩展/覆盖清单目录
  default_profile: base
  default_via: []                 # 全局优先的安装方式，如 [manager, bun]（留空 = 尊重清单顺序）
  allow_system_for: [devdep, service, cli]   # 允许"用 apt/brew 装"的 kind（不鼓励用系统包管理器装 runtime）
  managers:                       # 每个技术栈首选版本管理器（决定 via: manager 落到哪个管理器）
    golang: g
    nodejs: fnm                   # 备选 nvm / volta
    python: uv                    # 备选 pyenv
    rust: rustup
    universal: mise
  versions:                       # 期望版本（清单里用 {{versions.golang}} 引用；latest = 交给管理器选最新）
    golang: "1.24.0"
    nodejs: lts
    python: "3.12"
  extra_path:                     # 追加到 bs env 的 PATH（可留空，工具 provides 会自动补）
    - ~/.local/bin
    - ~/go/bin

shell:
  snippets_dir: ~/.config/bootstrap/shell   # 用户片段（内置片段 embed，此目录同名文件覆盖内置）
  rc_files:                       # 留空 = 按平台自动探测
    bash: ~/.bashrc
    zsh: ~/.zshrc
    fish: ~/.config/fish/conf.d/bootstrap.fish
    pwsh: ""                      # 例：~/Documents/PowerShell/Microsoft.PowerShell_profile.ps1
  auto_path: true                 # 受管块中包含 PATH 装配（等价 eval "$(bs env --path)"）
  auto_proxy: false               # 受管块是否在启动时自动应用活跃 proxy profile（默认关：不污染新 shell）

ui:
  confirm: true
  progress: auto       # auto|plain|off
```

**校验**：`bs config validate` 检查 schema 版本、路径可写、profile 定义完整、manifest 覆盖合法、`managers` 指向已注册管理器；`bs init` 结束时自动跑一次。

---

## 6. 工具清单模型（核心数据结构）

### 6.1 分类体系（需求 7）

两个正交维度：

- **Stack（技术栈 / 归属领域）**：`golang` `python` `nodejs` `rust` `cpp` `java` `shell` `containers` `cloud` `infra` `editor` `base`
- **Kind（功能类型）**：
  | Kind | 含义 | 例子 |
  |---|---|---|
  | `runtime` | 语言运行时/SDK | go, node, python, rust |
  | `versionmgr` | **语言版本管理器**（管理器优先策略的载体） | g, fnm, nvm, uv, rustup, mise, pyenv |
  | `pkgmgr` | 包管理器（装 CLI 工具用） | bun, pnpm, npm, pipx, cargo |
  | `cli` | 常用命令行工具 | rg, fd, jq, yq, gh, btop, delta |
  | `devdep` | 系统开发依赖（编译头文件等） | build-essential, python3-dev, libssl-dev |
  | `service` | 本地服务/守护进程 | docker, postgres, redis |
  | `gui` | 带界面的开发辅助 | vscode, wezterm |
  | `agent` | AI Agent / 编码助手 | codex, claude-code, opencode, gemini-cli, qwen-code, crush, aider, copilot-cli |

以此支持：`bs tool list --stack golang`、`bs tool install --kind devdep`、`bs tool install --profile golang`。

### 6.2 工具条目 Schema

清单是**声明式**的：条目只说"要什么、怎么判断装没装、按什么顺序委托谁装"，
不说"怎么解压、怎么切 PATH"（ADR-0005/0006）。下面摘自当前内置清单 `tools/common.yaml`：

```yaml
version: 1

tools:
  # ── ① 版本管理器：它自己用 release/brew/goinstall 装，属于"一次性"成本 ──
  - id: g
    name: g (Go version manager)
    stack: golang
    kind: versionmgr
    description: Go 多版本管理器（golang 条目由它落地）
    homepage: https://github.com/voidint/g
    when: {os: [linux, darwin]}
    path_dirs: ["~/.local/bin"]
    verify: {command: g, version_args: [--version], version_regex: '(?P<v>\d+\.\d+\.\d+)'}
    install:
      # 官方 release 用 sha256sum.txt（不是 goreleaser 默认的 checksums.txt）
      - via: release
        when: {os: [linux, darwin]}
        params:
          repo: voidint/g
          asset: "g{{version}}.{{os}}-{{arch}}.tar.gz"
          strip_components: 1
          binary: g
          dest: "~/.local/bin"
      - via: brew
        when: {os: [darwin]}
        params: {formula: g}
      - via: goinstall
        when: {has: [go]}
        params: {module: github.com/voidint/g@latest}

  # ── ② 运行时：只声明"交给哪个管理器装哪个版本"，bs 不碰解压与 PATH ──
  - id: golang
    name: Go
    stack: golang
    kind: runtime
    description: Go 工具链（由 g 管理）
    homepage: https://go.dev/dl/
    depends_on: [g]
    when: {os: [linux, darwin]}
    verify:
      command: go
      version_args: [version]
      version_regex: 'go version go(?P<v>\d+\.\d+(?:\.\d+)?)'
      min_version: "1.22"
    install:
      - via: manager
        params: {action: install, version: "{{versions.golang}}"}
      - via: manager
        when: {has: [mise]}
        params: {manager: mise, action: use, version: "go@{{versions.golang}}", global: true}
      # 全新机器上连 g 都没有时的引导路径：先装发行版自带的 go
      - via: apt
        when: {os: [linux], distro: [ubuntu, debian]}
        params: {packages: [golang-go], allow_system: true}

  # ── ③ 没有稳定可执行文件的工具：shell 函数型用 verify.file ──
  - id: nvm
    stack: nodejs
    kind: versionmgr
    provides: ["~/.nvm/nvm.sh"]
    verify: {file: "~/.nvm/nvm.sh"}
    install:
      # 仓库即安装物：按 tag 克隆到 ~/.nvm（git 自身保证完整性）
      - via: source
        params: {repo: nvm-sh/nvm, ref: v0.40.8, dest: "~/.nvm"}

  # ── ④ 目录型发布物：extract 整包落盘，symlinks 把可执行文件暴露到 PATH ──
  - id: uv
    stack: python
    kind: versionmgr
    path_dirs: ["~/.local/bin"]
    verify: {command: uv, version_args: [--version], version_regex: 'uv (?P<v>\d+\.\d+\.\d+)'}
    install:
      - via: release
        when: {os: [linux], arch: [amd64, arm64]}
        params:
          repo: astral-sh/uv
          asset: "uv-{{arch|archmap:amd64=x86_64,arm64=aarch64}}-unknown-linux-gnu.tar.gz"
          strip_components: 1
          extract: true
          dest: "~/.local/share/uv"
          symlinks:
            - {from: "~/.local/share/uv/uv", to: "~/.local/bin/uv"}
            - {from: "~/.local/share/uv/uvx", to: "~/.local/bin/uvx"}

  # ── ⑤ 保留但默认禁用的方法：加 enabled: false，用户覆盖时可一键启用 ──
  - id: fnm
    stack: nodejs
    kind: versionmgr
    verify: {command: fnm, version_args: [--version], version_regex: '(?P<v>\d+\.\d+\.\d+)'}
    install:
      - via: brew
        when: {os: [darwin]}
        params: {formula: fnm}
      # fnm 的 release 只有 zip、没有校验和文件，因此默认关闭
      - via: release
        enabled: false
        when: {os: [linux]}
        params: {repo: Schniz/fnm, asset: "fnm-linux.zip", binary: fnm, dest: "~/.local/bin"}
```

**工具级字段**（`internal/manifest/types.go` 的 `Tool`；`yaml.Decoder.KnownFields(true)`，
拼错字段名会直接报错）：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `id` | 字符串 | 是 | 唯一；`^[a-z0-9][a-z0-9._-]*$` |
| `name` | 字符串 | 否 | 展示名；与 id 不同时 `bs tool info` 才额外显示 |
| `stack` | 枚举 | 是 | `base` `golang` `python` `nodejs` `rust` `cpp` `java` `shell` `containers` `cloud` `infra` `editor` `agent` |
| `kind` | 枚举 | 是 | `runtime` `versionmgr` `pkgmgr` `cli` `devdep` `service` `gui` `agent` |
| `description` / `homepage` | 字符串 | 否 | 展示与溯源 |
| `tags` | 数组 | 否 | `--tag` 过滤，多个 tag 需全部命中 |
| `depends_on` | id 数组 | 否 | 安装按拓扑序；`runtime → versionmgr` 不许跨 stack |
| `when` | 条件对象 | 否 | 见下 |
| `provides` | 路径数组 | 否 | 安装后的**产出路径**，可以是文件（如 `~/.nvm/nvm.sh`） |
| `path_dirs` | 路径数组 | 否 | 要加进 PATH 的**目录**（`bs env path` 的两个来源之一，另一个是配置 `tool.extra_path`） |
| `verify` | 对象 | 是 | 见下 |
| `install` | 方法数组 | 是 | **书写顺序即优先级**（§7.2） |
| `update` / `uninstall` | 方法 / 方法数组 | 否 | 不写就复用 `install` 链（installer 按 `op` 换成对应命令） |
| `disabled` | 布尔 | 否 | 整条禁用；禁用条目只校验 id/stack/kind，允许"半成品" |
| `manager_optional` | 布尔 | 否 | 允许 `kind: runtime` 没有 `via: manager`（默认**不允许**） |
| `+install` `+uninstall` `+tags` `+provides` `+path_dirs` | 追加语法 | 否 | 只在用户覆盖文件里有意义（§6.3） |

`provides` 与 `path_dirs` 刻意分开：前者回答"东西装在哪、装没装"，后者回答"PATH 要加哪个目录"。
早期草案拿 `provides` 去拼 PATH，碰到 `~/.nvm/nvm.sh` 这种**文件**就只能猜是文件还是目录；
现在让清单把意图写清楚，代码不再猜。

**条件 `when`**：`os` `os_not` `arch` `distro` `distro_not` `has` `has_not`。
`has`/`has_not` 是**命令存在性**条件（`has: [bun]` 表示"机器上已有 bun 才走这条路"），
需要调用方注入 `LookPath`；无法判断时视为不匹配。工具级与方法级条件都必须满足。

**`verify`（决定"装没装、版本多少"）**：

| 字段 | 说明 |
| --- | --- |
| `command` | 探测用的可执行文件名；与 `file` 互斥（同时写时以 `command` 为准，校验期报错） |
| `file` | 用"文件是否存在"判断（shell 函数型工具，如 nvm 的 `~/.nvm/nvm.sh`） |
| `version_args` | 版本查询参数，如 `[--version]` |
| `version_regex` | **必须含命名组 `(?P<v>...)`**；加载期编译，写错立即报错 |
| `min_version` / `exact_version` | 语义化版本约束；低于 `min_version` 判为 `outdated` |

`kind: versionmgr` 必须声明 `command` 或 `file`——`depends_on` 排序与 `via: manager`
的可用性判断都靠它探测。

**方法级字段**（`Method`）：`via`（必填）、`when`（与方法级条件叠加）、`enabled`（缺省 true）、
`params`。

`params` 是**强类型**结构体而不是 `map[string]any`：字段名拼错在加载期就报错，而不是等安装时
被静默忽略。每个 `via` 只关心自己那几个字段，由各自的 Installer 校验必需项：

| via | 用到的 params |
| --- | --- |
| `manager` | `manager`（缺省取 `tool.managers.<stack>`）、`action`、`version`、`source`、`global`、`command` |
| `bun` `npm` `pnpm` | `package`、`global`（缺省 true）、`prefix`、`version` |
| `pipx` `uvpm` | `package`、`python`、`version` |
| `cargo` | `crate`、`features`、`git`、`version` |
| `goinstall` | `module`（可自带 `@version`）、`version`（缺省 `latest`） |
| `brew` | `formula` 或 `cask`、`tap` |
| `apt` `dnf` `yum` `pacman` `zypper` `apk` | `packages`、`needs_root`、`allow_system` |
| `winget` `scoop` `choco` | `id`（Windows 三件套）或 `packages`、`allow_system` |
| `script` | `url`、`sha256`（或 `insecure: true`）、`args`、`shell` |
| `release` | `repo`、`asset`、`version_source`、`version`、`dest`、`binary`、`strip_components`、`extract`、`symlink(s)`、`checksum`、`checksum_file`、`sha256` |
| `source` | `repo` 或 `url`、`ref`、`dest`、`build`、`install`、`command` |

**`via: manager` 的落地方式**：`internal/install/managers.go` 里每个管理器就是一组命令模板
（`ManagerSpec.Actions`），**没有**任何下载、解压、切 PATH 的逻辑（这才是"薄"）：

| 管理器 | bin | install / use / upgrade / uninstall | 备注 |
| --- | --- | --- | --- |
| `g` | `g` | `g install` / `g use` / `g upgrade` / `g uninstall` | |
| `fnm` | `fnm` | `fnm install` / `fnm use` / `fnm install` / `fnm uninstall` | |
| `nvm` | `nvm` | `nvm install` / `nvm use` / `nvm install` / `nvm uninstall` | shell 函数：先 `. ~/.nvm/nvm.sh` |
| `uv` | `uv` | `uv python install` / `uv python pin` / `uv python install --reinstall` / `uv python uninstall` | |
| `rustup` | `rustup` | `rustup toolchain install` / `rustup default` / `rustup update` / `rustup toolchain uninstall` | |
| `mise` | `mise` | `mise install` / `mise use -g` / `mise upgrade` / `mise uninstall` | |
| `pyenv` | `pyenv` | `pyenv install -s` / `pyenv global` / `pyenv install -s` / `pyenv uninstall -f` | 已注册，内置清单暂未使用 |

- 命令模板统一渲染 `{{version}} {{os}} {{arch}} {{tag}} {{versions.<name>}}` 等变量
  （过滤器与完整变量表见 §7.3）。
- `version: latest`（或 `lts`）只在**真要安装时**用该管理器的 `list-remote` 解析成具体版本；
  `--dry-run` 只留占位符，预览阶段不打网络。
- 清单里写 `via: manager` 而不写 `params.manager` 时，落地目标由配置 `tool.managers.<stack>`
  决定：把 `nodejs: fnm` 改成 `nodejs: nvm` 后，`depends_on: [fnm]` 会被自动改写成 `nvm`，
  **清单无需分叉**（引擎的 `normalize` 步骤，见 §7.5）。
- 需要一条清单表达不了的怪命令时用 `params.command` 逃生口，但仍属于该管理器的语义范围。
- **禁止**在管理器表里实现版本下载、镜像、PATH 切换——那是管理器自己的事；`bs` 只负责
  "确保管理器在、把命令发出去、验证结果"。

### 6.3 清单分层与合并

加载顺序（`internal/manifest/load.go` 的 `Load`，后者覆盖前者）：

```
① 二进制内置（go:embed，只读）         tools/common.yaml
                                      → tools/<GOOS>.yaml（linux/darwin/windows）
                                      → tools/profiles.yaml
② 用户覆盖（可读写，按文件名排序后覆盖） <config>/tools/*.yaml
```

> **ADR-0002（已决策）**：清单来源 = **内置 embed + 用户本地覆盖目录**，不实现远程清单仓库。
> 理由：离线可用、无网络与缓存一致性负担，二进制即"已知良好清单"的快照；新增/修正工具走本仓库 PR
> 或本地覆盖文件。远程清单若未来需要，作为独立增量能力（M5+）评估，不预留半成品开关。

`<config>/tools` 目录默认由 `tool.manifest.user_dirs` 决定（空则该目录即 `<config>/tools`）；
`tool.manifest.builtin: false` 可以只用自己的清单。

**合并规则**（按 `id` 合并；`bs tool manifest path` 能看出实际加载了哪几个文件）：

- **标量字段**（`name`/`stack`/`kind`/`description`/`homepage`）：覆盖文件里**非空**才覆盖，
  空字符串视为"没写"。
- **数组字段**（`tags`/`depends_on`/`provides`/`path_dirs`/`install`/`uninstall`）：
  **整体替换**，不做半合并。要追加就用显式语法 `+install` / `+uninstall` / `+tags` /
  `+provides` / `+path_dirs`；追加统一发生在**所有覆盖文件处理完之后**，语义稳定。
  例如"在内置 git 上再加一条 apt 路径"：

  ```yaml
  tools:
    - id: git
      +install:
        - via: apt
          params: {packages: [git]}
  ```

- **`when`**：按子字段合并（只覆盖写了的那几个维度），不会因为只想加一个 `arch` 条件而丢掉 `os`。
- **`verify`**：同样按子字段合并。
- **`disabled: true`**：一旦被覆盖文件置为 true 就会保持（覆盖只能禁用，不能重新启用）。
- **profile**：同名 profile **整体替换**，需要组合时用 `extends`（避免"半合并"的意外结果）。
- 每个条目记录 `Origin`（最终由哪个文件定义/覆盖），校验报错与 `bs tool manifest diff` 都用它定位。
- 同一个文件里出现重复 `id`、或字段名拼错，都在加载期直接报错（`KnownFields(true)`）。
- `bs tool list` 默认只显示当前平台**可见**的条目（`when` 匹配且未 `disabled`）；
  `--all` 显示全部并标注"不适用于当前平台"/"已禁用"。

`profiles.yaml` 只放**跨平台**套装（`base`/`golang`/`nodejs`/`python`/`rust`/`rust-dev`/`java`/
`editor`/`infra`/`agent`/`managers`）；带平台差异的套装（`cpp`/`containers`/`full`）写在 `tools/<os>.yaml` 里——
"cpp 要装哪些包"在 Linux 与 macOS 上不同，用 `when` 硬凑会把清单写成一团浆糊。
profile 可以写成纯列表简写，也可以是完整形式：

```yaml
profiles:
  base: [git, jq, ripgrep]                  # 简写
  golang:
    description: Go 工具链与开发工具
    extends: [base]                         # 继承（环会被校验拦住）
    tools: [g, golang, gopls, dlv, staticcheck]
```

### 6.4 M5 按 stack 补齐的条目（2026-10-14）

`common.yaml` 一次补进 27 条：java（`openjdk`/`maven`）、cpp（`ccache`/`clang-format`/`clangd`/
`meson`/`mold`/`conan`）、containers（`podman`/`skopeo`/`dive`/`lazydocker`/`hadolint`）、
infra（`kind`/`stern`）、rust（`rust-analyzer`/`sccache`/`cargo-nextest`/`cargo-audit`/`cargo-deny`）、
agent（`gemini-cli`/`qwen-code`/`opencode`/`crush`/`copilot-cli`/`aider`），并新增
`java`/`rust-dev` profile、给 `cpp`/`containers`/`agent`/`python` 补条目。

**每个方式的名字都实测过**（这是清单纪律 §9 的落地，不靠"看起来对"）：
apt 名字用 Ubuntu 24.04 的 `apt-cache policy`；Fedora 名字在 `fedora:41` 容器里
`dnf repoquery`；Arch 名字查 archlinux.org 的包 API；brew 查 formulae.brew.sh；
npm/crates/PyPI 查各自 registry；`release` 类查 GitHub Release 的**资产名与校验和文件**
（并且**下载归档看目录结构**，`strip_components` 只在该归档确实有外层目录时才写）。

`release` 类的实测结论（每一类都真装过一遍：`bs tool install kind lazydocker hadolint stern sccache --yes` 全绿）：

| 工具 | 资产形态 | 校验和 | strip |
|---|---|---|---|
| `kind` | 裸二进制 `kind-<os>-<arch>` | `<资产>.sha256sum`（带文件名） | — |
| `hadolint` | 裸二进制 `hadolint-<os>-<arch>` | `checksums.sha256`（行是 `<hash> *<名字>`） | — |
| `lazydocker` | `lazydocker_<v>_<Os>_<arch>.tar.gz` | `checksums.txt` | 无（根下就是二进制） |
| `stern` | `stern_<v>_<os>_<arch>.tar.gz` | `checksums.txt` | 无 |
| `sccache` | `sccache-v<v>-<triple>.tar.gz` | `<资产>.sha256`（裸哈希） | 1（有外层目录） |
| `dive` | `dive_<v>_<os>_<arch>.tar.gz` | `dive_<v>_checksums.txt`（名字带版本 → 显式 `checksum_file`） | 无 |

**直链模式**：`kubectl`（dl.k8s.io）、`helm`（get.helm.sh；GitHub Release 里只有 `.asc`
签名）、`gradle`（services.gradle.org）的产物都不在 GitHub Release 上。为此 `release`
新增了 `params.url`：产物地址由清单写全（支持模板），产物名从 URL 推出，
校验和按 URL 的**兄弟文件**找（`<url>.sha256` → `<url>.sha256sum`，也可用
`checksum_file` 指定）——**校验和仍然是硬要求**，拿不到就中止（§16）。
版本来源三种都可：固定版本、能读出最新版本的 URL（kubectl 的 `stable.txt`、
gradle 的 `versions/current`）、或 `params.repo` 走 GitHub `releases/latest`（helm）。
干跑一律不联网（`version_source` 是 URL 时也用 `<latest>` 占位）。
三条都真装过：kubectl 1.37.1 / helm 4.3.0 / gradle 9.8.1（gradle 顺带验证了
`extract` 整包解压 + `symlink.from` 里的 `{{version}}` 渲染）。

剩下的**已知缺口**（写在这里免得有人以为漏了）：

1. `rust-analyzer` 的官方 Release 不发布任何校验和文件（只有 `.gz`），按硬纪律不能用
   `via: release`；Ubuntu/Debian 也没有包，那边先用 `rustup component add rust-analyzer`。
2. `kustomize` 的 tag 带前缀（`kustomize/v5.8.3`）且校验和放在共享的 `checksums.txt` 里，
   现有模板过滤器表达不了"从 tag 里去掉前缀"；先用 brew/发行版包，等真需要时再加
   一个 `trimprefix` 过滤器 + "同目录校验和文件"支持。

### 6.5 探测缓存与"装完立刻可见"

探测结果缓存在 `<cache>/probe.json`（TTL 见 `detect.CacheTTL`），`tool list` 默认读缓存。
由此带来一个必须显式处理的坑：**计划阶段的探测会把"还没装"缓存下来**，
若应用阶段不更新缓存，接下来一整个 TTL 周期里 `bs tool list --installed` 都会显示没装。
所以 `Engine.Apply` 每执行完一条 install/upgrade 步骤就把刚探测到的结果 Merge 回缓存；
**卸载则无条件按 id 失效**——卸载步骤里带的 `Detection` 是计划阶段（卸载前）的结果，
拿它 Merge 会把"已安装"重新写回缓存，卸载后列表照样显示装着。
`--dry-run` 不碰缓存（一条都不写）。

### 6.6 校验（`bs tool manifest validate`，CI 中必跑）

校验的目的是**一次把错找全**（`manifest.Problems` 聚合，逐条列出），而不是让作者改一个跑一次。
`--os linux|darwin|windows` 可以按指定系统交叉校验（`bs tool manifest validate --os windows`）。

加载期（`Load`/`compileTool`）就会失败的：

- 清单 `version` 不是 `1`；
- YAML 里出现**未知字段**（`KnownFields(true)`，拼错 `vesion` 之类不会被静默忽略）；
- 同一个文件里**重复 `id`**；
- `verify.version_regex` 编译失败，或**没有命名组 `(?P<v>...)`**；
- `verify.min_version` / `exact_version` 不是合法版本号。

校验期逐条检查（`Validate`）：

- `id` 必填且匹配 `^[a-z0-9][a-z0-9._-]*$`；`stack`/`kind` 必填且在枚举内；
- `disabled: true` 的条目只查上面三项，其余允许"半成品"；
- `verify` 至少要有 `command` / `file` / `version_regex` 之一；
  `verify.file` 与 `verify.command` 互斥；`kind: versionmgr` 必须声明 `command` 或 `file`；
- `install` 不能为空；每个方法（含 `update`/`uninstall`）的 `via` 必须在安装器注册表里，
  `params` 必须过该 Installer 的 schema 校验；
- `params` 里所有模板字段都会用真实的 `tool.versions` 预渲染一遍，
  因此 `{{verison}}` 这类拼写错误、不存在的过滤器、未定义的 `{{versions.xxx}}`
  都会在 validate 阶段被拦住，而不是等真正安装时才失败；
- `via: manager`：`params.manager` 必须在管理器表里（不写则取 `tool.managers.<stack>`，
  该配置也没有就报错），`params.action` 必填且该管理器要支持；
- `kind: runtime` 必须有一条启用的 `via: manager` 路径，除非显式 `manager_optional: true`
  ——这条护栏就是"管理器优先"（ADR-0005）不被悄悄绕过的保证；
- **系统包管理器护栏**：`apt`/`dnf`/`yum`/`pacman`/`zypper`/`apk`/`winget`/`choco`
  只允许用于 `tool.allow_system_for` 列出的 `kind`（默认 `devdep`/`service`/`cli`），
  或该方法显式写 `params.allow_system: true`；否则报错。
  `brew`/`scoop` 不在护栏内——它们是用户态、无 sudo 的包管理器；
- `depends_on` 引用的 id 必须存在；`runtime → versionmgr` 的依赖不许跨 stack；
  `depends_on` 必须无环（环会打印出完整路径 `a → b → a`）；
- profile 里引用的 tool id 必须存在，`extends` 必须存在且**无环**。

`bs tool manifest diff <文件>` 用来预览"某份覆盖文件会让当前合并结果发生什么变化"
（新增了哪些工具、哪些字段/参数不同），改清单后先 diff 再 validate 是最省事的顺序。

---

## 7. 安装引擎

### 7.1 端口（domain 层接口）

`internal/install/ports.go` 定义安装引擎的全部对外契约：

```go
type Op string // install | upgrade | uninstall

// Installer 是"一种安装机制"（manager/bun/brew/apt/script/release/source …）。
type Installer interface {
    Name() string                                     // 与清单里的 method.via 对应
    Available(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method) bool
    Plan(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error)
    Execute(ctx context.Context, a Action, e ExecEnv) error
}

// 可选能力：用类型断言探测，避免接口膨胀。
type ValidateParams interface{ Validate(p manifest.Params) error }
// 一个实现覆盖多个 via 时（apt/dnf/pacman…共用一份实现）用这个。
type ValidateParamsFor interface{ ValidateFor(via string, p manifest.Params) error }

type Action struct {
    ToolID    string
    Via       string
    Op        Op
    Reason    string     // 为什么选中它（plan 输出用）
    Summary   string     // 给表格看的一句话
    Commands  []Command  // 可读 + 可执行
    Downloads []Download
    NeedsRoot bool
    Network   bool       // 需要代理注入
    // resolved 保存 Plan 算出来的中间结果（release 的 download/dest/symlink 等），
    // 由 WithResolved/Resolved 存取，避免把 installer 私有结构塞进公共字段。
}

type Download struct {
    URL, SHA256, Dest string
    Mode              os.FileMode
}
```

`Plan` 只做"将要发生什么"的静态计算（可联网解析版本与校验和，但**不落盘**）；
`Execute` 才真正动手。两者分开是 `bs tool plan` 与 `--dry-run` 能成立的前提。

**唯一允许碰系统的出口**是 `ExecEnv`（真实实现 `install.OSEnv`）：

```go
type ExecEnv interface {
    Run(ctx context.Context, c Command) (Result, error)
    Download(ctx context.Context, d Download) (string, error)
    FetchText(ctx context.Context, url string) (string, error) // 只读，干跑也照常
    LatestRelease(ctx context.Context, repo string) (string, error)
    LookPath(name string) (string, bool) // 先看 PATH，再看 extra_path
    Env() map[string]string
    DryRun() bool
    IsRoot() bool
    OS() string
    Arch() string
    Distro() string
    HomeDir() string
    Versions() map[string]string
    WorkDir() string // 只是拼接路径，不创建目录
    Log() *slog.Logger
}
```

`OSEnv` 的三条"不越界"约定（都有单测锁定）：

- `Run`/`Download`/`LatestRelease` 在 `DryRun` 时直接返回 `ErrDryRun`，不产生任何副作用；
  只有 `FetchText` 例外——它是只读请求，plan 需要真实的校验和才能在预览里显示将校验什么；
- `WorkDir()` 只算出 `<cache>/work/<pid>-<纳秒>`，**不建目录**：`bs tool plan` 不该因为要展示一个
  临时路径就在磁盘上留下东西；
- `LookPath` 先查注入的 `PATH`，再查配置 `tool.extra_path` 与 `~/.local/bin`、`~/bin`、
  `~/.g/versions/current/bin`——这些目录不写进 PATH 也能被探测到。

**注册表**（`Registry`）持有全部 Installer 与版本管理器定义：`Get(via)`、`KnownVias()`、
`IsSystemVia(via)`、`Managers()`、`ValidateMethod`、`ValidateManagerAction`。
系统包管理器是"一份实现、多个 via"（apt/dnf/yum/pacman/zypper/apk/winget/scoop/choco 各自
`RegisterAs`），`manifest.ValidateOptions.Vias` 接的就是它。

错误类型都实现了 `errors.As`：`ErrUnsupported`（该 via 不支持这个 op）、
`ErrMethodNotFound`、`ErrViaUnavailable{Declared}`、`ErrNoMethod`（聚合全部候选的失败原因）、
`UnknownViaError`、`UnknownManagerError`、`UnsupportedActionError`——退出码映射见 §10/§11。

### 7.2 方法链选择与降级（需求 8）

选择算法（`internal/install/chain.go` 的 `ResolveChain`）：

1. `--via <name>`（或配置 `tool.default_via`）只作用于**用户显式点名的工具**：自动补进来的依赖
   仍按自己的清单声明安装（否则 `bs tool install golang --via apt` 会把版本管理器也塞给 apt）。
   - 清单里没声明这个 via → `ErrMethodNotFound`（退出码 2，用法错误）；
   - 声明了但当前机器不可用 → `ErrViaUnavailable{Declared: true}`（退出码 2）——
     用户明确指定的方式不可用，必须整体失败而不是悄悄降级。
2. 否则**严格按清单里 `install` 的书写顺序**遍历（顺序即策略，不在代码里硬编码优先级）：
   - `enabled: false` → 跳过（不进候选表）；
   - 方法级 `when` 不匹配 → 跳过，并把**原因**记进 `Candidate.Note`
     （`bs tool plan`/`report.Steps[].skipped` 都能看到"为什么没用 brew"）；
   - Installer 的 `Available()` 为 false（没有 brew、管理器没装、`has: [bun]` 不满足）→ 跳过；
   - 命中第一个可用方法 → 选中，把"为什么是它"写进 `Action.Reason`。
3. 一个候选都没有时得到 `ErrNoMethod`，它**聚合每个候选各自的原因**（而不是只报最后一个），
   例如：`tool fnm: no usable install method (brew (when: os=linux 不在 darwin), cargo (when: 缺少依赖命令 cargo), release (enabled: false))`。
   计划期就有 `failed` 步骤时，`bs tool install/upgrade/uninstall` 以退出码 4 结束
   （前置条件不满足），不会让脚本误以为装好了；`bs tool plan` 仍是 0——它只是预览。
   而"用户点名的方式不存在/不可用"（`ErrMethodNotFound` / `ErrViaUnavailable`）会让整条命令
   直接以退出码 2 失败，绝不悄悄降级。

**降级（ADR-0013）**：选中方法执行失败后，只有 `Fallback` 为真才按顺序试链上其余候选
（`Chain.Fallbacks()` 只含当时 `Available()` 的候选）。降级必须显式留痕：

- 终端：`bs tool install` 的 Report 里标注 `bun 安装失败 → 已改用 npm`；
- `--json`：步骤上带 `fallback_from` / `fallback_reason` / `tried_methods`，
  顶层 `fallbacks: [{tool_id, from, to, reason}]`。

`Fallback` 的默认值随场景变化（`internal/app/tool.go` 的 `fallbackEnabled`）：

| 场景 | 默认 |
| --- | --- |
| `--fallback` / `--no-fallback` | 显式指定优先 |
| `--json`、`--yes`、非交互（stdout 不是终端） | **关**（绝不静默换安装方式） |
| 其它（交互式终端），以及 `--dry-run` | **开**（能自救就自救） |

**推荐书写顺序**（`bs tool manifest validate` 不强制，但内置清单照此写）：

```
① manager        → 版本管理器（g / fnm / nvm / uv / rustup / mise），runtime 的首选
② 专用包管理器   → bun / npm / pnpm / pipx / uvpm / cargo / goinstall（装 CLI 与 agent）
③ brew           → macOS 的主力（用户态、无 sudo、可回退）
④ release        → 官方 release 下二进制（管理器自身、无包管理器的工具）
⑤ 系统包管理器   → apt / dnf / pacman / winget（只用于不常升级的基础依赖，需要显式放行）
⑥ source         → 源码编译 / "仓库即安装物"（最后兜底）
```

> 内置清单里**不使用** `via: script`：官方脚本没有稳定哈希，固化 sha256 无从谈起，
> 用 `insecure: true` 等于放弃校验（AGENTS.md §8.3）。

**系统包管理器护栏**：默认只允许用于 `kind ∈ {devdep, service, cli}`（`tool.allow_system_for`）；
把 `runtime` 交给它们需要该方法的 `params.allow_system: true`（内置清单里 `golang` 的
`apt` 引导路径就是这么放行的），否则 validate 报错。`brew`/`scoop` 不在护栏内——用户态包管理器
用它们装 versionmgr（如 macOS 上的 `fnm`）是正常做法。

**依赖顺序**：`depends_on` 构成 DAG，`selectTools` 用 `manifest.ExpandDeps` 做拓扑排序
（同层保持清单书写顺序），例：`g → golang → gopls`。环在加载期就被 `manifest.TopoOrder`
拦截并打印完整路径。

### 7.3 Installer 实现与参数

`internal/install` 里的实现分成三族，共 14 个 via（`NewRegistry` 注册；系统包管理器一份实现
覆盖 9 个 via）。整包（引擎 + 链 + 注册表 + 全部 installer）约 2950 行，含注释，最大单文件
654 行——"薄"靠的是**共用实现 + 表驱动**，而不是每个 via 一个文件。

| via | 平台 | params | 实际行为 |
| --- | --- | --- | --- |
| `manager` | 全 | `manager` `action` `version` `source` `global` `command` | 查管理器命令模板表，渲染后执行；`latest`/`lts` 用 `list-remote` 解析（只在真要装时） |
| `bun` `npm` `pnpm` | 全 | `package` `global` `prefix` `version` | `global` 缺省 true → 加 `-g`；`prefix` 用于非 root 全局安装 |
| `pipx` `uvpm` | 全 | `package` `python` `version` | `pipx install` / `uv tool install`（天生用户级，不加 `-g`） |
| `cargo` | 全 | `crate` `features` `git` `version` | `--features` 逗号拼接；升级用 `install --force` |
| `goinstall` | 全 | `module` `version` | `module@version`；`version` 缺省或升级时用 `latest`；不支持卸载 |
| `brew` | mac/linux | `formula` `cask` `tap` | `tap` 先于 `install`；`cask` 时加 `--cask` |
| `apt` `dnf` `yum` `pacman` `zypper` `apk` | linux | `packages` `needs_root` `allow_system` | apt 先 `update`；需要 root 时自动加 `sudo` 前缀 |
| `winget` `scoop` `choco` | windows | `id` `packages` `allow_system` | winget 用 `--id`，其余用包名；升级一律不支持 |
| `release` | 全 | `repo` `asset` `version_source` `version` `dest` `binary` `strip_components` `extract` `symlink(s)` `checksum` `checksum_file` `sha256` | GitHub Release 下载 → 校验 sha256 → 解压/取二进制 → 落盘 + 软链 |
| `source` | 全 | `repo` 或 `url` `ref` `dest` `build` `install` `command` | `git clone --depth 1 [--branch ref]`；`dest` 直指目标目录（nvm 这类"仓库即安装物"） |
| `script` | 全 | `url` `sha256` `args` `shell` `insecure` | 下载固化哈希的官方脚本再执行；**必须**有 `sha256` 或显式 `insecure: true` |

几处刻意的取舍：

- **`release` 拿不到校验和就中止**（AGENTS.md §8.3）。查找顺序：`params.sha256` 固化 →
  `params.checksum_file` 指定的文件名 → 常见文件名 `checksums.txt` / `sha256sum.txt` /
  `checksums.sha256` / `SHA256SUMS` → `<asset>.sha256`（Rust 生态习惯，文件里通常只有一个裸哈希）。
  `params.checksum: none` 是唯一的显式放弃开关。实测各家习惯不同（g 用 `sha256sum.txt`、
  k9s 用 `checksums.sha256`、uv/ripgrep 每个产物配 `<asset>.sha256`），逐个试比强制清单作者
  记住某个项目的习惯更可靠。
  这些"元数据查询"（最新 tag、校验和文件、版本文本）共用 `install.LookupTimeout = 60s`
  的单次上限，而真正的下载用 `httpx.DefaultDownloadTimeout = 15min`（通用 GET 是 60s）——
  **下载慢是正常的，查元数据慢不是**；把两者绑在同一个 60s 上，会出现"能下完却永远装不上"。
- **下载是"可续、可复用"的**（M4）：归档/脚本落在 `<cache>/downloads`（跨次运行稳定，
  与每次运行都不同的 `WorkDir` 刻意分开），半成品命名为 `.<名字>.<url 指纹>.part`——
  带 URL 指纹就不会出现"v1.2 的半成品被 v1.3 续上"；下次下载带 `Range` 接着来，
  目标文件已存在且 sha256 一致时直接用缓存。两条防线保证"续传不破坏完整性"：
  服务端忽略 `Range`（回 200）时整体重写、校验失败时删掉半成品。缓存条目 30 天未用
  在下次下载前清理（`--dry-run` 不清理）。**`<cache>/downloads` 可以随时整个删掉。**
- **`extract` 决定"目录型"还是"单文件型"**：默认从归档里挑出 `binary`（缺省是 repo 名）
  复制到 `dest`；`extract: true` 则整棵解压到 `dest`（Go 工具链、uv 这类目录型发布物），
  再用 `symlinks` 把要暴露的可执行文件链到 PATH 目录。
- **`script` 与 `release` 都不写系统目录**：只写 `dest`/`symlink` 指定的用户目录，绝不静默 `sudo`。
- **`source` 固定 `ref`** 比跟 HEAD 可控；`dest` 为空时克隆到 `<cache>/work/src-<id>`。

**模板渲染**（`internal/install/render.go`，与 Go 的 `text/template` 无关，只是 `{{...}}` 占位符）：

| 变量 | 说明 |
| --- | --- |
| `{{version}}` / `{{tag}}` | 解析出的版本 / tag（`v` 前缀保留在 `tag` 里） |
| `{{os}}` `{{arch}}` | 目标系统与架构（`--dry-run` 时也按本机算） |
| `{{package}}` `{{crate}}` `{{module}}` `{{formula}}` `{{python}}` `{{manager}}` `{{action}}` | 各 via 的语义参数 |
| `{{versions.<name>}}` | 配置 `tool.versions` 里的值（如 `{{versions.golang}}`） |

过滤器：`{{arch|archmap:amd64=x86_64,arm64=aarch64}}`、`{{os|osmap:linux=unknown-linux-musl}}`、
`{{version|trimv}}`、`{{x|default:y}}`、`{{x|lower}}`、`{{x|upper}}`。
**未知变量或未知过滤器直接报错**，不会静默留下 `{{...}}` 字面量；
清单校验期会用 `tool.versions` 的真实值（缺省栈用样例值兜底）把所有模板预渲染一遍，
所以拼写错误在 `bs tool manifest validate` 阶段就能发现。

### 7.4 探测（需求 3 的"已安装/未安装"）

`internal/detect.Detector` 只有一个公开入口 `Detect(ctx, tools, Options{Refresh, UseCache, Only})`
和 `Probe(ctx, tool)`（单工具、不走缓存）。判定顺序：

1. `verify.command` 在 PATH（含 `extra_path`）里 → 执行 `version_args`，
   用带命名组 `(?P<v>...)` 的 `version_regex` 抽版本；
2. `verify.file` 存在 → `installed`（nvm 这类 shell 函数没有可执行文件）；
3. 都没有，但 `provides` 里有可执行文件 → `unknown` + 说明"命令不在 PATH，但 provides 路径存在"
   （这正是 `bs env path` 该被 eval 的信号）；
4. 什么都不在 → `missing`。

状态四态：`installed` / `outdated`（版本不等于 `exact_version`，或低于 `min_version`）/
`missing` / `unknown`（命令在但版本解析不出来——**不阻断安装**，只提示）。
只声明了 `command` 而没有 `version_regex`/`version_args` 时，命令存在即算 `installed`
（detail 写"命令存在（未声明版本探测）"）。
`Result` 带 `tool_id`/`status`/`command`/`version`/`detail`/`checked_at`，还有 `Present()`、
`Installed()`、`Label()` 等便捷方法。

工程约束：

- **并发探测**，默认 `min(8, NumCPU)`（`MaxConcurrency`），单个工具的探测超时 5s
  （`ProbeTimeout`）——某个卡死的 `--version` 不会拖慢整体；
- 结果缓存到 `<cache>/probe.json`（`paths.CacheDir`，TTL 10 分钟）。缓存只服务**显示**：
  10 分钟内重复 `bs tool list` 不必反复起子进程；
- **要据此跳过安装的决定必须强制重探**（ADR-0010/0011）：`PlanTools` 在 `op == install` 时
  传 `Refresh: true`，`bs tool list --refresh` 同理；
- `--dry-run` 时 `UseCache: false`——干跑的语义是"磁盘上什么都不留下"，连缓存也不写；
  缓存写入用 `WriteFileIfChanged`，内容没变就不碰 mtime（"重复 list 不写盘"是可断言的性质）；
- 缓存文件损坏或过期只当作"没有缓存"，不报错；`ErrNoExecutor` 防止"既没有执行器也没有
  PATH 查找"时静默给出满屏 `missing`。
- **探测与安装共用同一套查找规则**（`install.OSEnv.LookPath`：进程 PATH + `tool.extra_path`
  + 清单 `path_dirs` + `~/.local/bin`、`~/bin`、`~/.g/versions/current/bin`）。这不是洁癖：
  新机器上装进 `~/.local/bin` 的工具在重开 shell 之前不在 PATH 里，若两边规则不一致，
  "复核"会把自己刚装好的工具判成 missing。也因此探测**用解析出的完整路径执行** `--version`，
  而不是退回裸命令名。

### 7.5 执行流水线（`bs tool plan` / `bs tool install`）

引擎的 `PlanTools` 与 `Apply` 严格分开（`internal/install/engine.go`），
`bs tool plan` 只跑前三步，`--dry-run` 跑到 Apply 前停下：

```
[1] Preflight   依赖检查：depends_on 指向的工具不在本次选择里 → 记进 plan.Problems
                （"可能已安装"，只提示不阻断）
[2] Select      ExpandDeps 拓扑排序（含依赖自动补全）→ 过滤 disabled 与 when 不匹配的工具
[3] Plan        逐工具 ResolveChain → 选中的 Installer.Plan()
                · install：已安装（installed）→ skip；uninstall：未安装 → skip；upgrade 从不跳过
                · 方法链全部不可用 → 该步 status=failed 并带上聚合原因
                  （CLI 看到 failed 步骤就以退出码 4 结束，不会静默返回 0）
                · ErrUnsupported（如用 apt 卸载）→ status=unsupported（不算失败）
                · 探测：install 且非 dry-run 时强制重探；dry-run/plan 既不读也不写缓存
[4] Confirm     `bs tool install/upgrade/uninstall` 在**真正动手前**要一句明确同意
                （CLI 层，不在引擎里）：
                · --yes 直接通过；
                · 非交互（stdin/输出不是终端、--json、--non-interactive、CI=1）
                  绝不阻塞等待输入，以退出码 5 结束并提示加 --yes；
                · 交互式：提示打在 stderr，只有 y/yes 继续（回车＝拒绝），
                  拒绝后打印"已取消，未做任何修改。"并以退出码 0 结束。
[5] Apply       按拓扑序**串行**执行（工具之间有依赖，暂不提供 --jobs）
                · dry-run：把 install/upgrade/uninstall 改成 planned 回显，一行都不执行
                · 每个 Action 由 Installer.Execute 执行；失败且有 --fallback 时试链上其余候选
[6] Verify      装完立刻重探（Probe，不走缓存）：仍然 missing → 该步 failed，
                错误是"命令已执行，但验证失败"——不相信命令的退出码
[7] Report      逐步状态 + 耗时 + 降级记录；`installed.yaml` 落盘（audit）；
                每个工具的命令输出落到 `<state>/logs/<时间>-<工具>.log`，失败项在
                Report 里打印日志路径；有失败项 → 退出码 1；既有成功又有失败 → 退出码 3
```

`normalize` 是 Plan 前的一个隐式步骤：把配置 `tool.managers.<stack>` 落到清单上——
`via: manager` 缺 `params.manager` 时补上，`depends_on: [fnm]` 在该 stack 的管理器换成 `nvm` 时
自动改写依赖指向（**清单无需分叉**）。没用到配置映射时不复制清单。

- **`bs tool plan` 是离线预览**：它把引擎切到干跑语义（`App.DryRun = true`），
  因此**不联网解析最新版本**（`release` 的 URL 保留 `<latest>` 占位符）、**不写探测缓存**，
  但仍然做真实探测（这样才能如实回答"哪些已装、哪些要处理"）。新机器上网络还没配好时，
  `bs tool plan` 必须立刻给出结果，而不是每个工具卡 15 秒。
- **`--dry-run`** 是给 `install/upgrade/uninstall` 的同一套演练开关，行为一致。
- 真实的版本号、下载地址与校验和**只在执行安装时解析**（`release` 查最新 tag、
  `manager` 的 `latest` 查 `list-remote`）。预览显示占位符是有意为之：预览的用途是
  "看要做什么、按什么顺序、用哪种方式"，不是"看最终 URL"。

**日志与审计**：`<state>/installed.yaml` 记录 `{version, tools.<id>.{via, version, installed_at}}`，
只写成功（`done`）的步骤，内容不变时不写盘；它**不是**"是否已安装"的依据（ADR-0010/0011），
只用于 `bs tool info` 展示历史与将来的升级提示。命令输出走两处：`slog`（`-v` 可见完整
argv 与退出码，实时）与文件日志 `<state>/logs/<时间>-<工具>.log`（`install.LogPath`，
含每条命令的 stdout/stderr 与退出码）。文件日志由引擎在每个步骤外面套一层
`logEnv` 装饰器实现——所有 `via` 共用同一条执行路径，拦在这里就覆盖全部安装方式，
且每个工具一个实例，不存在并发写同一文件。`--dry-run` 不写日志（零副作用）。

### 7.6 卸载与升级

- **upgrade 不做"只有 outdated 才动"的闸门**：探测只用于展示，实际的"要不要装/要不要升"
  交给各自的包管理器（`cargo install --force`、`go install x@latest`、`brew upgrade`、
  `g upgrade`、`rustup update`…）。`tool.update` 没写就复用 `install` 链，installer 按 `op`
  换成对应的升级命令；`release` 的"升级"就是重新取最新 release 并覆盖。
- **系统包管理器的升级一律 `unsupported`**（"不需要及时升级"的定位），要用系统工具自己升。
- **`upgrade --all` 是批量入口**：候选=清单里**所有已安装**的工具（`installed` / `outdated` /
  `unknown` 都算"已安装"——用 `installed` 当判据会把最该升级的 outdated 漏掉）。
  批量命令必须能跑完能跑的部分，所以它有两条与点名单个工具不同的规则：
  1. `unsupported`（系统包管理器升级）与"没有任何可用方式"（`Step.NoMethod`）的步骤
     被摘出来列为**跳过**，不计失败；点名单个工具时这些仍然照旧报错/提示——
     "`bs tool upgrade htop` 不支持"本身就是用户要的答案。
  2. 一个可升级的都没有（乃至一台机器什么都没装）时**退出码 0**：
     "本来就是干净的/最新的"不是错误，脚本里 `bs tool upgrade --all` 不该因此中断。
  真正的问题（缺校验和、`--via` 写错）依然整体失败。
- `--all` 与 `-i` 一样不能和显式工具名同时给（退出码 2）：否则"我点名的那个被忽略"很难查。
- **uninstall 只对"用户态装法"提供**：`bun/npm/pnpm/pipx/uvpm/cargo/manager/brew` 可以；
  `goinstall`/`release`/`script`/`source` 与 `pacman`/`winget` 返回 `unsupported`，
  提示交给原生包管理器。`unsupported` 只是"这条不做"，不算失败（不影响退出码）。
- `bs tool uninstall` 会跳过本来就没装的工具（探测到 `missing` → `skip`），
  所以重复执行是安全的。

---

## 8. 代理子系统（需求 5、6）

### 8.1 四条使用路径（含"当前 shell 立即生效"）

| 路径 | 谁受益 | 机制 | 是否需要改 rc |
|---|---|---|---|
| **A. Shell 函数包装**（主用法） | 你自己在终端里敲 `bs proxy on` | `bs shell install` 往 rc 的**受管块**里注入一个与二进制同名的 `bs` shell 函数：拦截 `bs proxy on/off/use` → 先执行真二进制（持久化活跃 profile）→ 再 `eval "$(bs proxy env --apply)"` 注入**当前 shell** | 需要一次，且由 `bs` 自动写入 |
| **B. 显式 eval** | 已有自己的 rc 习惯、或用别的 shell | `eval "$(bs proxy env local)"` / `--shell fish` 等 | 需要 |
| **C. 零 rc 改动** | 临时机器、CI、不想碰 rc | `bs proxy exec -- <cmd>`（一次性）；`bs shell [--profile P]`（开一个环境已装配的子 shell） | 不需要 |
| **D. 自动注入**（需求 5） | `bs` 自己发起的 clone / 下载 / 包管理器命令 | 执行前把活跃 profile 的 env 合并进 `ExecEnv.Env()`；`proxy.override_existing=false` 时**不覆盖**用户已有的 `HTTP_PROXY` | 不需要 |

> **必须说清楚的技术事实**：任何子进程都**无法**修改父 shell 的环境变量（POSIX 硬约束）。所以"敲 `bs proxy on` 当前 shell 就生效"只有一种诚实实现——**rc 里有一个同名 shell 函数做中转**，这正是 `nvm`、`direnv`、`zoxide`、`conda` 的通行做法。区别在于：`bs` 的这一行由 `bs shell install` **自动写入且可自动升级**，你永远不会手写、也不会在换电脑时漏掉。

**路径 A 的实际效果**（rc 里只有一行 `eval "$(bs shell init bash)"`，内容是动态渲染的）：

```bash
bs() {                                   # 与二进制同名的包装函数（由 bs shell init 输出）
  case "${1:-}.${2:-}" in
    proxy.on|proxy.off|proxy.use)
      command bs "$@" || return $?        # ① 真二进制：持久化活跃 profile
      eval "$(command bs proxy env --apply)"   # ② 注入当前 shell（off 时输出 unset）
      ;;
    *) command bs "$@" ;;                 # 其余命令直通
  esac
}
# 你原来的手感原样保留：
startproxy()   { command bs proxy use local   ; eval "$(command bs proxy env --apply)"; }
chaitinproxy() { command bs proxy use chaitin ; eval "$(command bs proxy env --apply)"; }
stopproxy()    { command bs proxy off         ; eval "$(command bs proxy env --apply)"; }
```

于是：**改代理站点 = 改配置里的 profile**，不用再动系统里那份脚本；换电脑 = `bs shell install` 一条命令。

### 8.2 命令

```
bs proxy list                      # 列出所有 profile 及默认者
bs proxy show [profile]            # 展示将导出的具体变量
bs proxy on [profile]              # 设为活跃 profile（写 state）；有包装函数时当前 shell 立即生效
bs proxy off                       # 清空活跃 profile（当前 shell 立即 unset）
bs proxy use <profile>             # on 的别名（贴近你原有 startproxy 的语义）
bs proxy status                    # 活跃 profile + 当前 shell 实际 env + 来源 + 可达性
bs proxy env [profile]             # 输出可 eval 的环境变量族（按 --shell 渲染）
      --shell bash|zsh|fish|pwsh|cmd|dotenv|json
      --apply                      # 按"当前活跃 profile"渲染；无活跃 profile 时输出 unset/清除语句
      --print                      # 只打印，不做任何 state 变更
bs proxy exec [profile] -- <cmd...># 临时带代理执行命令（无需 eval）
bs proxy test [--url https://github.com]   # 分别测直连与代理连通性/延迟
bs proxy add <name> --http ... --no-proxy ...   # 快速新增 profile（写用户配置）
```

`bs proxy on` **只改 state 文件**（持久化默认 profile），绝不修改 rc；rc 的生效由 §8.1 的包装函数或 `eval` 完成。

### 8.3 按平台渲染

| Shell | 输出 |
|---|---|
| bash/zsh | `export HTTP_PROXY=... HTTPS_PROXY=... http_proxy=... https_proxy=... NO_PROXY=... no_proxy=...` |
| fish | `set -gx HTTP_PROXY ...` |
| PowerShell | `$env:HTTP_PROXY = "..."` |
| cmd | `set HTTP_PROXY=...` |
| dotenv | `HTTP_PROXY=...`（供 `docker --env-file`、其他脚本 source） |

- 同时设置**大写与小写**两套变量，`no_proxy`/`NO_PROXY` 同值——完全复刻你现有 rc 脚本的行为。
- `--apply` 在"无活跃 profile"时输出**清除语句**（`unset HTTP_PROXY ...` / `set -e HTTP_PROXY` / `Remove-Item Env:HTTP_PROXY`），保证 `bs proxy off` 真的能关掉，而不只是留着旧值。
- 渲染逻辑全部在 `pkg/proxyenv`（纯函数：`Render(profile, shell) (string, error)`），被 `proxy`、`env`、`shellsnip` 三处共用——这就是 §4.1 里"上移 `pkg/`"的典型收益。

### 8.4 Shell 集成子系统（把你喜欢的脚本搬进仓库，换机器不再 copy）

**目标**：`bs shell init` 不只是输出代理函数，而是把"我习惯的那一套 shell 环境"统一受管；**这些片段的主要归宿是 Bootstrap 仓库自己**（ADR-0012），换机器无需任何额外同步。

**片段（snippet）来源与优先级**：

```
① 仓库内置片段（主载体，go:embed，随二进制发布）
   tools/snippets/posix/{10-proxy.sh, 20-path.sh, 30-tools.sh, 90-aliases.sh}
   tools/snippets/fish/*.fish
   tools/snippets/pwsh/*.ps1
② 用户片段（可选覆盖，机器特定/临时内容）
   <config>/shell/{posix,fish,pwsh}/*.sh
```

- **日常新增别名/函数直接提 PR 或直接改 `tools/snippets/`**：升级 `bs` 即自动带上，不存在"这份脚本在哪台机器上"的问题。代价是片段变更要走提交（个人仓库，成本可接受）。
- `<config>/shell/` 只用于**机器特定或敏感**内容（某台机器独有的 export、路径、私密 alias），不进仓库。
- 文件名 `NN-name.*` 决定加载顺序（10 → 20 → 90），同序号时用户片段优先；同名文件**整体覆盖**内置（便于临时替换而不改仓库）。
- `bs shell snippet list|new <name>|path` 管理用户片段（`new` 只创建空文件，编辑用 `$EDITOR` 或直接改，
  这里刻意不内置编辑器交互）；`bs shell init` 会把这些片段内容**内联渲染**进输出（所以 rc 里永远只有一行）。

**命令**（均为已实现）：

```
bs shell init [bash|zsh|fish|pwsh|cmd]     # 渲染完整受管内容（包装函数 + 片段 + PATH/代理装配）
bs shell install [--rc <file>] [--shell S] # 幂等写入 rc 的受管块；写前自动备份（--dry-run 见全局 flag）
bs shell uninstall [--rc <file>] [--shell S]  # 精确移除受管块（块外内容零改动）
bs shell status [--rc <file>] [--shell S]  # 受管块是否存在、内容是否与当前 bs 一致
bs shell snippet list|new <name>|path
bs shell [--profile P]                   # 不改 rc：开一个已装配环境 + PATH 的交互式子 shell
```

**受管块格式**（幂等替换的锚点，块外字节级不动）：

```bash
# >>> bootstrap (managed by 'bs shell install'; edit snippets in ~/.config/bootstrap/shell) >>>
eval "$(bs shell init bash)"
# <<< bootstrap <<<
```

- `bs shell install` 的行为：定位/探测 rc → 若存在受管块则**原地替换**（升级即生效）→ 否则追加 → 写前备份 `<rc>.bs-backup.<ts>` → 原子写入。
- `bs shell status` 对比"rc 中的块内容"与"当前 `bs shell init` 的输出"，提示是否需要重跑
  `bs shell install`（升级 `bs` 之后重跑一次即可；**没有**单独的 `shell update` 子命令）。
- **安全边界**：只识别并替换上述标记之间的内容；不解析、不重排、不删除块外的任何行；`uninstall` 只删块。这与 §16 的安全约定一致。

### 8.5 探测与安全

- **proxy env 自动识别**（需求 5）：执行网络操作前，按 `bs 配置 profile > 环境变量 HTTP(S)_PROXY > 系统代理（macOS `scutil --proxy`、Windows 注册表 `ProxyServer`、Linux `gsettings`/env）》的顺序解析；`bs proxy status` 展示每一项来源。
- **连通性测试**：先直连，失败再用代理；`--check-network` 时给 3s 超时，避免离线环境下卡住。
- **凭据**：profile 支持 `http_proxy: http://user:${BS_PROXY_PASS}@host:port`，密码可来自环境变量或系统 keyring（后续增强），**不落明文**（至少支持 env 引用语法 `${VAR}`）。

---

## 9. 仓库管理子系统（需求 1）

### 9.1 URL 归一化（`pkg/giturl.Parse`）

输入 → `RepoRef{Host, Owner, Repo, Ref, SubPath, Raw}`，规则：

| 输入示例 | 归一化结果 |
|---|---|
| `kingfs/Bootstrap` | host=`github.com`（`repo.default_host`）, owner=`kingfs`, repo=`Bootstrap` |
| `https://github.com/kingfs/Bootstrap` | 同上 |
| `https://github.com/kingfs/Bootstrap.git` | 去掉 `.git` |
| `git@github.com:kingfs/Bootstrap.git` | scp-like 语法解析 |
| `ssh://git@gitlab.com/group/sub/proj.git` | GitLab 多级 namespace：`owner=group/sub` |
| `https://github.com/kingfs/Bootstrap/tree/feat/x/internal/foo` | repo=`Bootstrap`, ref=`feat`, subpath=`x/internal/foo`（⚠️ 见下方歧义说明） |
| `https://github.com/kingfs/Bootstrap/blob/main/README.md` | repo 同上, ref=`main`, subpath=`README.md` |
| `https://github.com/kingfs/Bootstrap/releases/tag/v0.3.0` | ref=`v0.3.0`（tag） |
| `https://github.com/kingfs/Bootstrap/pull/12` | repo + `pull/12` 元信息（提示可 `--fetch pr/12`） |
| `https://github.com/kingfs/Bootstrap?tab=readme` | 忽略 query |
| `github.com/kingfs/Bootstrap`（无 scheme） | 识别首段含 `.` → 视为 host |

算法：去空白 → 去 query/fragment（`#` 可作 ref 分隔符）→ 去尾部 `/` → 去 `.git` → 处理 scp-like → 切分路径 → 首段含 `.` 或等于已配置 host 视为 host，否则用 `default_host` → 取 `owner`（GitLab 支持多段，以最后一段为 repo）→ 余下段匹配 `tree|blob|commit|releases/tag|pull|compare` 提取 ref/subpath → 校验 owner/repo 字符集。

**分支名含 `/` 的歧义（实现约束）**：`/tree/<a>/<b>/...` 里无法判断 `<a>` 是完整分支名还是
分支名的第一段，除非查询远端。M1 的实现选择可预期的规则：**紧跟关键字的第一段就是 ref，其余是 subpath**。
需要精确指向含 `/` 的分支时用 `bs repo get <url> --ref feat/x` 显式覆盖。
（备选方案"探测远端分支列表"会引入网络请求，破坏 `Plan` 阶段的无副作用特性，故不采用。）

**未知 host 策略**：形状为 `<host>/<owner>/<repo>` 且 host 含 `.` 时宽松接受（写成 `<user_host>/...` 目录）；`repo.hosts` 白名单只用于"简写歧义"和 clone 协议选择。

### 9.2 路径布局

```
<repo.root>/{{.Host}}/{{.Owner}}/{{.Repo}}       # 默认模板，与你现有 /data/src/github.com/kingfs/... 习惯一致
```

- `repo.root` 缺省探测顺序：配置值 → `<repo.root>` 存在则用 → Linux/macOS `~/src`；Windows `D:/src`（存在则用，否则 `%USERPROFILE%\src`）。
- `bs repo root` 打印实际根目录与来源；`bs repo get --print-path` 只打印目标路径（便于 `cd "$(bs repo get x/y --print-path)"`）。
- 已存在同名目录但**不是** git 仓库 → 默认报错并提示 `--force` / `--dir`；`--force` 时**不删除**已有内容，只在空目录或显式 `--adopt` 下继续。

### 9.3 clone / pull 语义

```
目标不存在 → git clone [--depth N] [--branch REF] <url-or-ssh> <path>
目标已存在且有 .git →
    git -C path remote get-url origin     # 校验 origin 是否同一仓库，不一致 → 警告（--repair-origin 修正）
    git -C path fetch --all --prune
    git -C path status --porcelain        # 脏工作区？
        ├─ 干净 → git -C path pull --ff-only|--rebase|--merge
        └─ 脏   → 默认跳过并提示（--autostash 则 stash→pull→stash pop）
    非默认分支：只 fetch + 报告落后/领先提交数，不自动切换
```

- 全程注入代理（见 §8），走 HTTPS 时复用系统 git 的 credential helper（用户已经配好的 PAT 继续有效）。
- `--ssh` 或 `repo.clone.protocol: ssh` 时把 URL 规范化为 `git@<host>:<owner>/<repo>.git`。

**M1 实现现状**（与上面伪代码的差异，均已实测通过）：

- 已实现：不存在→clone（含 `--depth`/`--branch`/`extra_args`）、存在且是 git 仓库→`fetch --all [--prune]` +
  `status --porcelain` + `pull --ff-only|--rebase|--merge`、脏工作区默认跳过、`--autostash` 走 stash→pull→pop、
  `bs repo list`（分支/远端/脏状态）、`bs repo sync`（并发，单仓库失败不中断整体）、`--dry-run` 完全不执行命令。
- 已强化的安全语义：目标目录**非空且不是 git 仓库**时一律 `skip`（不删除、不覆盖任何已有文件），
  只有目录为空时才 clone 进去——这一条由单测锁定（`TestPlanSkipProtectsPreciousDirectory`）。
- 推迟：`--print-path`、`--adopt`、`origin` 不一致的校验与 `--repair-origin`、非默认分支的落后/领先计数。
- `--depth` 支持浅克隆，`--sparse` 后续增强。

### 9.4 批量能力

```
bs repo get kingfs/Bootstrap other/repo https://github.com/x/y   # 一次给多个 URL 即批量
bs repo list [--root <path>] [--json]                            # 枚举 root 下所有仓库
bs repo sync [--jobs N] [--root <path>] [--autostash]            # 批量 pull，汇总表格
```

> 从文件读取 URL 列表（`-f repos.txt`）、`repo list --dirty`、`sync --only-dirty` 都**还没实现**；
> 需要批量时把 URL 写在命令行或 `xargs`（`bs repo get` 接受任意多个位置参数）。

`bs repo sync` 是"换电脑/定期更新"的主力命令：并发 fetch+pull，输出 `仓库 | 分支 | 动作 | 结果 | 领先/落后`。

---

## 10. 平台探测（需求 2）

```go
type Platform struct {
    OS        string   // linux | darwin | windows   (runtime.GOOS)
    Arch      string   // amd64 | arm64
    Distro    string   // ubuntu/debian/fedora/arch/alpine/... (解析 /etc/os-release 的 ID)
    DistroVer string
    Shell     string   // bash | zsh | fish | pwsh | cmd
    IsWSL     bool
    IsRoot    bool
    HasSudo   bool     // 免密或可交互
    PackageManagers []string  // apt/dnf/pacman/brew/winget/scoop/choco 探测结果
    HomeDir   string
    Env       map[string]string
}
```

- Linux：`/etc/os-release`（ID/ID_LIKE/VERSION_ID）；容器与 WSL 识别（`/.dockerenv`、`WSL_DISTRO_NAME`）。
- macOS：`sw_vers -productVersion`（仅信息展示）；芯片经 `GOARCH`。
- Windows：`runtime.GOOS` + PATH 中探测 winget/scoop/choco；不依赖注册表即可（增强才会读）。
- 该结构在进程启动时就绪，作为参数显式传给下游（**不鼓励全局单例**，便于测试注入）。

---

## 11. CLI 命令全集

> 这一节是**已实现**的命令表面（`bs <cmd> --help` 即权威参考）。规划中但尚未实现的命令
> 集中列在末尾的"尚未实现"，避免把设计意图误读成既有能力。

```
bs [--config FILE] [--repo-root DIR] [--dry-run] [--yes] [--json] [--no-color]
   [--interactive|--non-interactive] [--plain-ui] [--timeout DURATION] [-v|-vv] [--log-level LEVEL] <command>

version              版本信息（--json：version/commit/date/go_version/os/arch）
doctor [--fix]       环境自检（只读）；--fix 顺手修"结论明确、修法唯一"的问题（缺配置/仓库根/受管块）

init                 新机器一把梭：环境概览 → 选工具 → plan → apply（可选 shell install）
                     [--profile P] [--tools a,b] [--missing-only] [--with-shell|--no-shell]
self-update          把 bs 自己更新到最新发布版（sha256 校验 + 自检 + 备份 + 原子替换）
                     [--check] [--version vX.Y.Z] [--force] [--repo owner/repo]

config path          打印各配置/状态/缓存目录的实际路径及来源
config show          打印合并后的生效配置
config validate      校验配置文件合法性
config init          生成默认配置文件（已存在时不覆盖）
config get <键>      打印某个键的生效值（点分路径；-v 说明来源；列表/映射输出 YAML）
config set <键> <值> 只改用户级配置里的那一行（注释与顺序保留，写前备份、写后校验、不合法还原）
                     [--unset]   删除该键（回到由默认值/上层配置决定）
config keys          列出可用键路径（get/set 的拼写参考）
config edit          用 $EDITOR 打开用户级配置（需要终端；退出后立刻校验）

proxy  list | status | on | use [名字] | off
proxy  env [--apply|--off] [--profile 名字] [--shell bash|zsh|fish|pwsh|cmd|dotenv|json]
proxy  exec [--profile P] -- <命令...>   在带代理的子进程里执行（`--` 之后原样传给子进程）
proxy  test                分别测直连与走代理（**唯一会发真实网络请求的命令**）
proxy  add                 新增 profile（默认只打印 YAML 片段，不写文件）

repo   get <url...> [--root DIR] [--ref R] [--depth N] [--protocol https|ssh] [--autostash] [--force]
repo   list | root
repo   sync [--root DIR] [-j/--jobs N] [--autostash]

tool   list  [--stack S] [--kind K] [--profile P] [--tag T]... [--installed|--missing|--outdated] [--all]
             [--refresh] [--no-probe]
tool   search <关键词>      （别名 find；与 list 同一套过滤参数）
tool   info <工具> [--refresh]
tool   plan  [工具...] [--profile P] [--stack S] [--kind K] [--tag T]... [--missing]
             [--via V] [--fallback|--no-fallback]      # 离线预览：不联网解析版本、不写缓存
tool   install   [同上] [-i/--interactive]     # -i：先探测，再在交互界面里勾选要处理的工具
tool   upgrade   [同上] [-i/--interactive] [--all]   # --all：升级所有已安装的（系统包管理器按 §7.6 列为跳过）
tool   uninstall [同上] [-i/--interactive]
tool   manifest path | validate [--os linux|darwin|windows] | show [工具...] | diff <文件>

env    show           显示将被装配的 PATH 条目（含来源与是否存在）与代理状态
env    path [--shell S]     输出把工具目录加入 PATH 的 shell 语句（供 eval，幂等）
env    export [--shell S]   输出 PATH + 代理的完整 shell 语句（供 eval）

shell  init <bash|zsh|fish|pwsh|cmd>   输出受管块内容（片段 + bs 包装函数），供 rc 里 eval
shell  install                          把受管块幂等写入 rc（写前备份）
shell  uninstall                        移除受管块（块外内容字节级不变）
shell  status                           显示受管块安装状态
shell  snippet list|new|path            查看/创建 shell 片段
shell  [--profile P]                    开一个已装配环境的交互式子 shell（不改 rc；不接受命令参数）

completion <shell>   由 cobra 提供（bash/zsh/fish/powershell）
help [命令]          由 cobra 提供
```

**退出码约定**（Agent 与 CI 友好，映射集中在 `internal/cli/exit.go`）：

| 码 | 含义 | 例子 |
| --- | --- | --- |
| `0` | 成功（含"没东西要做"与用户主动取消） | `bs tool install` 全部 skip |
| `1` | 一般失败 | 所有步骤都失败、未知错误 |
| `2` | 用法/参数错误 | 工具名不存在、`--via` 写了一个清单里没有的方式、`--fallback` 与 `--no-fallback` 同时给 |
| `3` | 部分成功 | 3 个工具装了 2 个 |
| `4` | 前置条件不满足 | `--via` 指向本机不可用的方式（`ErrViaUnavailable`）、仓库根未初始化（`ErrNoRoot`）、需要执行器却没有可用的执行环境 |
| `5` | 需要交互但处于非交互模式 | 破坏性操作没给 `--yes` |

**非交互约定**：输出不是终端、`--json`、`--non-interactive` 或 `CI=1` 时不提问、不阻塞；
破坏性操作改为要求 `--yes`，否则以退出码 5 结束。`bs tool install/upgrade/uninstall`
在交互终端会问一句（默认拒绝），拒绝后打印"已取消，未做任何修改。"并以 0 结束。

**尚未实现**（设计意图，不在当前二进制里；写在这里以免误读）：
`tool uninstall --allow-system`、`proxy remove`、`repo info`、
`shell snippet edit|show|remove`、`shell update`、
跨工具并行的 `--jobs`/`--fail-fast`/`--keep-going`、
Windows 工具清单与 `repo.root` 的 Windows 约定（ADR-0014）。

---

## 12. 输出与人机交互

**人类可读输出**：

- 列表类用 `text/tabwriter` 对齐的表格（`tool list`：工具/技术栈/类型/状态/方式/说明），
  没有"进度条动画"这类花活；
- `bs tool plan` 首行固定提示"（只展示计划，不会执行任何操作）"，`--dry-run` 提示
  "（dry-run：以下操作都不会被执行）"——**先说清会不会动手，再展示要做什么**；
- 计划与执行结果都逐工具一行（`工具 / 动作 / 方式 / 说明`），执行完再打一行汇总
  `共 N 个工具，其中 X 装好、Y 跳过…（耗时 …）`；
- 有工具被真正改动时补一句"提示：新装的工具可能需要重开 shell 或执行
  `eval "$(bs env path)"` 才能进入 PATH"——把"命令成功"和"当前 shell 可用"这两件事分开说；
- 错误与提示写 stderr（含确认提示），数据/表格写 stdout，所以 `bs tool list | grep x` 是干净的。

**机器可读输出（`--json`）**：

- **没有信封**。`--json` 直接输出该命令的数据结构本身（`bs tool list --json` 是数组，
  `bs tool plan --json` 是 `Plan` 对象，`bs tool install --json` 是 `Report` 对象），
  不套 `{"version":1,"command":"...","data":{...}}`。理由有二：
  1. 少一层解包——调用方（脚本、Agent）本就按命令知道自己在请求什么；
  2. `bs env path` / `bs shell init` 这类命令的**正常输出就是给 `eval` 用的 shell 代码**，
     信封会直接破坏 `eval "$(bs env path)"` 的组合能力。约定比"统一形状"更重要。
- 成功/失败不写在 JSON 里，而是由**退出码**表达（§11）。需要细节时看 `report.steps[].status`
  与 `report.steps[].error`；部分成功是退出码 3 而不是某个布尔字段。
- 字段命名以 snake_case 为主（`ToolRow`/`Plan`/`Step`/`Report`/`EnvReport` 都有显式 `json` tag），
  但 `bs tool manifest show --json` 直接序列化 `manifest.Tool`，用的是 Go 字段名
  （`ID`/`Name`/`Stack`/`Install`…）——那是"给人对比合并结果"的调试视图，不承诺稳定 schema。
  需要稳定消费面的用 `tool list` / `tool info` / `tool plan`。
- `--json` 同时隐式进入非交互模式：不提问、不阻塞，`fallback` 默认关闭（ADR-0013）。

**交互（ADR-0003）**：所有提问都经由 `internal/ui.Prompter`。今天有三处会提问：
`bs init` 的"要不要生成配置 / 勾哪些工具 / 是否装 shell 块"、`bs tool install|upgrade|uninstall -i`
的勾选、以及破坏性操作前的一句确认。，`charmbracelet/huh` 是它唯一的
落点（`go.mod` 里 huh 只被这个包 import）。选择顺序：

| 环境 | 实现 | 行为 |
|---|---|---|
| 非交互（管道/`CI`/`--json`/`--non-interactive`） | `ui.NonInteractive` | 提问即返回 `ErrNotInteractive` → 退出码 5，绝不阻塞 |
| 真终端 + `--plain-ui`，或不是真终端 | `ui.Text` | 纯文本问答（编号/名字、`a` 全选、`q` 取消），零依赖 |
| 真终端 | `ui.huhPrompter` | 交互式界面：空格勾选、`/` 过滤、←/→ 切换、回车提交 |

约定与红线：

- 破坏性操作前问一句，按钮写"继续 / 取消"，**回车＝默认拒绝**；拒绝不是错误（退出码 0，
  提示"未做任何修改"）；`Ctrl-C` 同样按取消处理（`huh.ErrUserAborted` → `ui.ErrCancelled`）；
- `--yes` 是脚本与 CI 的正规授权方式；非交互环境没有 `--yes` 就以退出码 5 明确拒绝
  （AGENTS.md §8.6），而且**先判同意再联网**（`bs self-update` 在没授权时不发任何请求）；
- `ui.confirm=false` 只让交互会话不再确认，**不**放宽非交互环境的 `--yes` 要求；
- 纯文本路径永远可用，也是测试里唯一能跑的实现；`internal/ui` 不允许把 huh 类型泄漏到接口上。

**怎么验证交互式界面**：huh 只在真终端启动，而"假终端会吞掉按键"——bubbletea 启动时会往终端写
OSC 11（查背景色）与 DSR（查光标位置），然后从 stdin 读回应；`script` 这类假终端没人回答，
于是把用户按键当回应吃掉，看起来像"按键失灵"。所以真界面用 `make pty` 验证：
`scripts/pty-check.py` 用 `pty.fork` 起一个真 PTY、**替终端回答查询**、再按时间表送按键，
并断言退出码与关键输出（例：勾选后干跑落地、`Ctrl-C` 取消退出码 0、确认框回车＝拒绝）。

**日志**：`log/slog` 结构化日志，默认 stderr，级别取 `log.level`（`-v` debug、`-vv` trace）。
外部命令的完整 argv 与退出码在 debug 级可见；`*_PROXY` 值一律脱敏（保留 host:port，
userinfo 换成 `***`）。文件日志（`<state>/logs/<时间>-<工具>.log`）由引擎按步骤写入
（§7.5 的 `logEnv`），日志写不出来只降级为警告，不让安装失败。

---

## 13. 工程化约定

| 关注点 | 方案（当前实现） |
|---|---|
| 错误处理 | `fmt.Errorf("...: %w", err)` 逐层包装；领域错误用 Sentinel + `errors.Is/As`；`internal/cli/exit.go` 是**唯一**把 error 映射成退出码的地方（`codedError` + cobra 文案识别） |
| 上下文 | 所有 IO 首参 `context.Context`；超时集中在四处：外部命令默认 10min（`cmdx.DefaultCommandTimeout`）、版本探测 5s（`detect.ProbeTimeout`）、元数据查询 60s（`install.LookupTimeout`，只用于"最新 tag / 校验和文件 / 版本文本"）、通用 GET 60s 与下载 15min（`httpx.DefaultTimeout` / `httpx.DefaultDownloadTimeout`）、单条连接 10s（`httpx.dialTimeout`）。这五个数都能被配置覆盖（`timeouts.{command,probe,lookup,download}`），`--timeout`/`timeouts.global` 再给整条命令加一层总时限；取消时**杀整棵进程组**并配 `cmd.WaitDelay`，否则孤儿子进程会抱着管道把 `Wait` 拖住，超时形同虚设 |
| 并发 | `errgroup` 只用在两处：工具探测（`min(8, NumCPU)`）与 `repo sync`（`repo.sync.jobs`）；安装执行是**串行**的（依赖顺序优先，暂不并行） |
| 配置注入 | 依赖显式构造（`app.New` 装配 `Runtime`/`App`），**没有 `init()` 副作用**——安装器注册表也由 `install.NewRegistry()` 显式构建 |
| 平台分支 | 平台差异收敛在 `pkg/hostinfo`（探测）与 `internal/config/paths.go`（目录策略）；业务代码里不散落 `runtime.GOOS` |
| 外部命令 | 一律走 `pkg/cmdx`（超时、干跑、脱敏、日志都在那里），业务代码不直接 `os/exec` |
| 测试 | 表驱动单测；安装引擎用 `internal/install/fake_test.go` 的 `fakeEnv`/`fakeDetector`（记录命令、可编排返回值），探测用注入的 `Executor`/`LookPath`；CLI 层用进程内 `Runtime.Run` + 临时 `BS_CONFIG_HOME`/`BS_STATE_HOME`/`BS_CACHE_HOME` + 缓冲区输出；端到端用 `scripts/smoke.sh`（POSIX）与 `scripts/smoke.ps1`（Windows），两者都不发网络请求；另有**需要网络的**可选 `scripts/e2e-install.sh`（真下载真校验，产物全部落在 `.cache/e2e/`，不属于 CI 必跑项） |
| 无副作用测试（硬要求） | 全部读写都在 `t.TempDir()`；`--dry-run` 断言"目标路径不存在/文件数不变"；幂等断言"第二次执行 mtime 不变"（`fsx.WriteFileIfChanged`/`state`）；涉及用户文件的逻辑断言"内容字节级未变"（`shellsnip`）；单测与 smoke **都不发网络请求** |
| 静态检查 | `make check` = `gofmt -l` + `go vet ./...` + `go test ./...` + `go test -race ./...`；CI 在 ubuntu/macos/windows 三个 runner 上跑同样的四件事 + 冒烟。另有 **`make lint`**（golangci-lint，`.golangci.yml` 里只留"真错误"类 linter）与 **`make vuln`**（govulncheck）——两者**刻意不进 `check`**：一个慢、一个要联网，不该拦住本地提交 |
| 依赖策略 | 直接依赖 5 个：`spf13/cobra`、`spf13/viper`、`gopkg.in/yaml.v3`、`golang.org/x/sync`、`charmbracelet/huh`；huh 只允许出现在 `internal/ui`（其余包通过 `Prompter` 提问）；`pkg/*` 不 import `internal/*` |
| 提交规范 | Conventional Commits（`feat(tool): ...`），正文写动机与影响；每个提交都要"可编译、可发布、可用" |
| 版本管理 | `internal/buildinfo.{Version,Commit,Date}`，由 `-ldflags -X` 注入（`Makefile` 与 `.goreleaser.yaml` 各一份）；`bs version --json` 暴露 |
| 幂等与安全 | 文件写入用 `pkg/fsx` 的 `AtomicWriteFile`（tmp+rename）与 `WriteFileIfChanged`（内容不变不碰 mtime）；改用户 rc 前先 `fsx.BackupFile`；下载必须校验 sha256（拿不到就中止） |

**没有的东西**（设计稿里出现过，别照抄）：`cmdx.FakeExecEnv`（假实现统一放在各包的
`*_test.go` 里）、`testdata/golden` 快照、`//go:build integration` 标签。

**干净的机器上才暴露的问题**由 `make docker-matrix` 覆盖（`scripts/docker-check.sh`
在 ubuntu/debian 最小镜像里以 `--network none` 跑新机器流程）：smoke 跑在开发机上，
"缺 git/curl 会怎样""有没有偷偷依赖宿主环境"在那里永远测不出来。它不进 `make check`
（要 Docker、要拉镜像、慢），沿用 e2e 的定位。

**干净机器 + 真网络**才暴露的问题由 `make docker-integration` 覆盖
（`scripts/docker-integration.sh` → 容器内 `scripts/docker-integration-check.sh`，
默认 `ubuntu:24.04`）：apt 系统包路径、GitHub Release 下载 + sha256、`params.url`
直链、装/卸闭环与探测缓存，以及**代理全链路**。代理那段的证据是反向对照——把代理指向
黑洞（`http://127.0.0.1:9`）后下载**必须**失败且错误里出现该地址，切回真代理后同一件事
**必须**成功；"下载成功"本身不作为"代理生效"的证据。它同样不进 `make check`
（要 Docker、要外网、会真下几十 MB）；代理地址用 `BS_PROXY_URL` 覆盖，给空串则跳过
代理阶段。第一次跑就抓到两个问题：`ubuntu:24.04` 镜像没有系统 CA 证书导致所有 HTTPS
失败而错误里毫无指向；`via: release` 装的工具根本卸不掉。两者都已修（§18.3）。

---
## 14. 构建与发布

> **ADR-0008（已决策）**：**唯一分发渠道是 GitHub Release 的二进制包**，不做 deb/rpm、
> 不做 Homebrew tap、不做 scoop/winget 清单发布。理由：流程是"Release 下二进制 → 跑 bootstrap"，
> 中间商只会增加维护面。

- **目标产物**：`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、
  `windows/amd64`、`windows/arm64`（6 个组合，`Makefile` 的 `PLATFORMS` 与 goreleaser 的
  `goos`×`goarch` 矩阵一致）。`CGO_ENABLED=0` 纯静态、交叉编译零负担；统一带 `-trimpath`。
- **版本注入**：`-X github.com/kingfs/Bootstrap/internal/buildinfo.{Version,Commit,Date}`，
  `Makefile` 用 `git describe`/`git rev-parse`/当前时间，goreleaser 用 `{{.Version}}`/
  `{{.Commit}}`/`{{.CommitDate}}`。
- **goreleaser**（`.goreleaser.yaml`，`version: 2`，`project_name: bootstrap`）：
  - `archives`：`bootstrap_<version>_<os>_<arch>.tar.gz`（Windows 覆盖为 `.zip`），
    内含 `bs`(`.exe`) 以及 `README.md`、`docs/design.md`、`scripts/install.sh`、
    `scripts/install.ps1`（**没有 LICENSE 文件**——仓库里就没有，所以别写进产物清单）；
  - `checksum`：`checksums.txt`（sha256），供引导脚本校验；
  - `changelog`：来自 GitHub，按提交信息排序，排除 `docs:`/`test:`/`chore:`/`style:`；
  - **不配置** `nfpm`/`brews`/`scoop`/`winget`。
- **GitHub Actions**：
  - `.github/workflows/ci.yaml`：在 `ubuntu-latest`/`macos-latest`/`windows-latest` 三个原生
    runner 上跑 `gofmt -l`（仅 ubuntu）、`go vet`、`go test -race`、冒烟
    （POSIX 跑 `scripts/smoke.sh`，Windows 跑 `scripts/smoke.ps1`）；另有一个 job 跑
    `make cross` 并上传 `dist/**` 作为产物——**这保证 Windows/macOS 代码路径真的被编译与测试**；
  - `.github/workflows/release.yaml`：`v*` tag 触发，先跑单测与冒烟，再 `goreleaser release --clean`。
- **Makefile**：`build` `install` `fmt` `fmt-check` `vet` `test` `race` `cover` `smoke`
  `check` `cross` `snapshot` `release-check` `clean`。所有 Go 命令都经 `scripts/devenv.sh` 包裹
  （把 `GOPATH`/`GOMODCACHE`/`GOCACHE` 指到仓库内 `.cache/`，受限环境也能构建）。
  `make snapshot` / `make release-check` 需要本机装 goreleaser（`make tools` 里固定了版本），
  日常 CI 用不到。
- **发布彩排（`make release-check`）**：发版那天最容易翻车的不是编译，而是"包发出去装不上"。
  所以有一个不碰 GitHub 的彩排：`make snapshot` 打出 6 个平台的产物后，
  `scripts/release-check.sh` 把它摆成 `<base>/<tag>/…` 的目录、用本地 HTTP 服务冒充下载站，
  再让 `scripts/install.sh` **真的去装一遍**。检查：产物齐全/命名/`checksums.txt` 逐条对得上、
  归档里有 README+设计文档+两个安装脚本（且与仓库里的**字节一致**）、本机那份二进制是静态链接、
  `bs version` 带得出快照版本号与本次提交、内置清单（go:embed）可用、覆盖安装不报错；
  以及三条**必须失败**的路径——归档被篡改（校验拦住）、站点上没有 `checksums.txt`（按红线中止）、
  版本不存在（明确报下载失败）——且失败后不能留下半个可执行文件。
  接着用同一套办法覆盖**自更新**（最危险的一步：替换正在运行的二进制）：把站点摆成 GitHub 的
  路径形状，用 `BS_GITHUB_API`/`BS_GITHUB_ASSET_BASE` 指过去，现场编译一个"更新的版本"发上去，
  验证 `--check` 只查不下载、真更新后版本变了且旧二进制留了备份（备份里确实是旧版本），
  以及"产物被篡改""站点没有 checksums.txt"两条路都必须中止、**当前二进制一动不动、也不多留备份**。
  全程只用 `curl`+`tar`+`sha256sum`+`python3` 起本地服务，不碰 GitHub。
- **引导安装**（鸡生蛋闭环）：
  - `scripts/install.sh`：`curl -fsSL .../install.sh | sh` —— 探测 OS/Arch、
    解析版本（`--version vX.Y.Z` 或 `BS_VERSION`，缺省跟随 `releases/latest`）、
    `--base-url`/`BS_BASE_URL` 可把下载前缀换成镜像或加速站（此时必须显式给版本）、下载归档、
    **必须**从 `checksums.txt` 取到 sha256 并校验（拿不到就中止）、解压后装到
    `~/.local/bin`（`--dir` / `BS_INSTALL_DIR` 可覆盖），最后提示 `eval "$(bs env path)"`；
  - `scripts/install.ps1`：Windows 侧同款（下载 zip、校验、装到用户目录并写 PATH，
    `-BaseUrl`/`BS_BASE_URL` 同样可换镜像）；
  - 两个脚本都可重复执行（重跑即升级）。
- **镜像/加速站**：GitHub 在很多网络环境下慢或不通，所以 `BS_GITHUB_API` 与
  `BS_GITHUB_ASSET_BASE` 可以换掉 API 与下载前缀（作用于按清单装 release 工具与自更新两条路径），
  `BS_REPO` 换发布仓库。三者都只认环境变量、不落配置文件：换机器/进内网时临时给一下，
  比把镜像固化进配置更不容易忘；`scripts/release-check.sh` 也正是靠它们把自更新
  指向本地站点来彩排。
- **`bs self-update`**（ADR-0009：只校验 sha256）：解析目标版本（`--version` 或 latest）→
  取 `checksums.txt` 里的 sha256（**拿不到就中止**，不提供"跳过校验"开关）→ 下载校验 →
  解压出 `bs` → **跑一遍 `<新二进制> version` 自检** → 备份旧文件 → 原子替换 → 清理自己产生的
  `*.bak-*`/`*.tmp-*`。Windows 上正在运行的 exe 不能被覆盖、但能被改名，因此"改名让位 → 放新的"，
  失败时把原件改回去。`--check` 只查版本，`--dry-run` 连校验和都算给你看但什么都不下载。

---

## 15. 文档体系

- **`README.md`（给人看，简洁）**：一段话定位 → 安装（三平台各一行）→ **5 分钟上手**（`bs init` 一把梭，再是 `bs shell install` + `bs tool plan/install`）→ 常用命令速查表（init/self-update/repo/proxy/shell/tool/env）→ 配置示例 → 卸载。
- **`AGENTS.md`（给 AI Agent 看）** 必含：项目定位与不变量、**构建/测试/校验命令**、目录地图与分层依赖规则、**如何新增一个工具（只改 YAML 的三步流程）**、如何新增一种安装机制（实现 Installer + 注册 + 测试）、代码风格与提交规范、Definition of Done 清单、禁止事项（如禁止在 `cli` 层写业务逻辑、禁止无 sha256 的脚本安装）。
- **`docs/`**：`tool-schema.md`（清单字段全参考 + 每个 via 的 params）、`configuration.md`（配置项全参考 + 示例）、`design.md`（本文，**ADR 以 §18 的形式内联在这里，没有单独的 `docs/adr/` 目录**）。
- **命令内建文档**：每个 cobra 命令有 `Long`/`Example`；`bs completion <shell>` 由 cobra 自动提供；`bs <cmd> --help` 即权威参考。

---

## 16. 安全边界

1. **下载物必须校验 sha256**：`via: release` 固化了 `params.sha256`、指定了 `params.checksum_file`，
   或能从常见校验和文件里查到才继续，查不到就中止（`params.checksum: none` 是唯一的显式放弃开关）；
   `via: script` 必须有 `params.sha256`，除非清单显式写 `params.insecure: true`。
   内置清单里**不使用** `via: script`（AGENTS.md §9 的清单纪律）。没有 `--insecure-script` 这类 CLI 开关——
   放弃校验必须写进清单并被 review。
2. **不静默提权**：系统包管理器的使用受 `tool.allow_system_for` 护栏限制（§7.2），
   需要 root 时命令自动加 `sudo` 前缀，`Action.NeedsRoot` 在 `--json` 的
   `plan.steps[].action.needs_root` 里可见，具体命令用 `-v` 或 `--dry-run -v` 能看到。
   人工确认是**整批一次**的"即将安装/升级/卸载 N 个工具，继续？"（§7.5 的 [4]），
   不是逐命令二次确认；`--yes` 表示调用方已明确授权。
3. 代理凭据不落明文日志；日志中 `*_PROXY` 值做掩码（保留 host:port，隐去 userinfo）。
4. `bs self-update` **只校验 sha256**（ADR-0009），签名校验明确不做；校验和拿不到就中止，
   没有"跳过校验"的开关。替换前先跑一遍新二进制的 `version` 自检，跑不起来就保留旧的。
5. rc 文件只允许通过 `bs shell install` 的**受管块**修改：块由固定标记包围、可幂等替换、可精确移除，块外字节级不动；写前自动备份。其余场景一律只**输出**片段，不写文件。
6. **破坏性操作必须先拿到明确同意**（`confirmDestructive`）：交互终端默认拒绝，
   非交互环境绝不阻塞，没有 `--yes` 就以退出码 5 结束（`--dry-run` 不需要同意，因为它什么都不做）。
7. **镜像前缀是"把信任转交出去"**：`BS_GITHUB_ASSET_BASE` 同时决定产物与 `checksums.txt`
   的来源，所以镜像能换掉两者而校验依然"通过"——sha256 防的是传输损坏和单边篡改，
   防不了"你自己指过去的那台服务器"。因此这两个变量只认环境变量、不落配置文件（不会
   在换机器后悄悄生效），并且在 `bs self-update --help`、`docs/configuration.md` 与这里
   三处都写明这层含义。默认值永远是官方地址，不设变量就等于没有镜像。

---

## 17. 演进路线

策略：**先把整条 CLI 主干打通并能自举，再持续往清单里补工具**。每个阶段结束都必须"可编译、可发布、可用"。

| 阶段 | 目标 | 交付 | 状态 |
|---|---|---|---|
| **M0 骨架** | 能发布一个"什么都不能干但装得上"的 `bs` | go.mod 定版、目录结构、cobra root、`config path/show/validate/init`、`version`、goreleaser + CI、`install.sh`/`install.ps1` | ✅ 完成 |
| **M1 CLI 主干跑通** | 高频能力先可用 | `pkg/{giturl,version,proxyenv,hostinfo,cmdx,fsx}`；`proxy` 全套；`shellsnip` + `shell init/install/uninstall/status/snippet`；`repo get/list/root/sync`；`env show/path/export`；`doctor`（只读） | ✅ 完成 |
| **M2 工具引擎** | 清单驱动安装 | 清单 schema + 分层加载/合并/校验；`detect`（四态 + TTL 缓存 + 强制重探）；`plan`/`install`/`upgrade`/`uninstall`；14 类 via（`manager` 覆盖 g/fnm/nvm/uv/rustup/mise/pyenv，`brew`，apt/dnf/yum/pacman/zypper/apk/winget/scoop/choco，bun/npm/pnpm/pipx/uvpm/cargo/goinstall，`release`/`script`/`source`）；真实内置清单 `tools/{common,linux,darwin,profiles}.yaml`；`tool list/info/search/plan/install/upgrade/uninstall` + `tool manifest path/validate/show/diff`；`--dry-run` 与"破坏性操作先要一句同意"（退出码 5） | ✅ 完成（判据见下） |
| **M3 初始闭环** | 新机器一把梭（**linux + darwin**） | `bs init` 向导（huh：环境概览 + 已装/未装 + 勾选）→ plan → apply → 可选 `bs shell install`；`self-update`（sha256 + 自检 + 备份 + 原子替换）；`internal/ui` 的 `Prompter` 抽象（纯文本永久可用）；接线 `ui.confirm`/`ui.progress`/`shell.auto_path`/`shell.auto_proxy`/`--interactive`/`--plain-ui` | ✅ 完成 |
| **M4 打磨** | 长期好用 | ✅ 全局 `--timeout` + `timeouts.*` 配置（取消时杀整棵进程组）、✅ `tool install -i` 交互勾选、✅ `bs doctor --fix`、✅ 下载断点续传 + 下载缓存复用、✅ `config get\|set\|unset\|edit\|keys`、✅ `tool upgrade --all` + `tool list --outdated`、✅ `golangci-lint`/`govulncheck`（`make lint`/`make vuln`，刻意不进 `make check`）、✅ shell completion 覆盖（smoke 断言四种 shell）、✅ Docker 集成矩阵（`make docker-matrix`：ubuntu/debian 裸镜像 + `--network none`）、✅ README/AGENTS/design/configuration 定稿（判据见下）、✅ 干净镜像真网络集成测试（`make docker-integration`，含代理全链路反向对照） | ✅ 完成 |
| **M5+ 持续补全** | 不断把喜欢的工具链补进来 | ① Windows 清单（scoop/winget + pwsh 片段 + `repo.root` 的 `D:/src` 约定）；② ✅ 按 stack 补齐 java/cpp/containers/rust/agent（2026-10-14：+27 条，含 `release` 类资产的归档结构与校验和实测）；③ ✅ 直链类工具（kubectl/helm/gradle）——`release` 已支持 `params.url`；④ ✅ 发布流程彩排 `make release-check`（本地站点真装 + 自更新整条链路，不碰 GitHub）；⑤ ✅ GitHub 镜像前缀 `BS_GITHUB_API`/`BS_GITHUB_ASSET_BASE`（按清单装 release 工具与自更新同一条路径） | 🔄 进行中 |

**M2 完成判据的自查**：

- `bs tool list` 能看到三平台差异 ✅（`when` 过滤 + `--all` 标注"不适用于当前平台"）；
  `bs tool manifest validate --os linux|darwin|windows` 三种系统都能通过 ✅；
- `bs tool install golang` 是否"走 g 装好并可用" ✅——曾经的拦路虎（`params.version` 里的
  `{{versions.golang}}` 从不渲染，见 §18.4 缺陷 #1/#2）已修：引擎在选完工具、`Installer.Plan`
  之前统一展开"名字类"参数。复核方式（离线、可复查字面量）：
  `bs tool install golang --dry-run` 在已装 `g` 的机器上显示 `g install latest`；
  `bs config set tool.versions.golang 1.26.9` 之后显示 `g install 1.26.9`。
  任何一步若出现字面量 `{{...}}`，就说明渲染链又断了。

**M3 完成判据的自查**（全部在无副作用沙箱里验证；`make pty` 需要 `/dev/ptmx`）：

| 判据 | 验证方式 | 结果 |
|---|---|---|
| `bs init` 能一把梭 | `bs init --dry-run --yes --tools ripgrep` 零写入；`make pty` 里真勾选后落地 | ✅ |
| 非交互不阻塞 | `init` / `self-update` 无 `--yes` → 退出码 5；`--json` 也走这条路 | ✅ |
| 参数互斥/未知目标 | `--missing-only --profile`、`--with-shell --no-shell`、未知 profile/tool → 退出码 2 | ✅ |
| 交互式勾选（huh） | `make pty`：多选勾选后干跑落地；勾选数回填正确（缺失项默认勾选） | ✅ |
| 取消语义 | `make pty`：Ctrl-C → 退出码 0 + "未做任何修改"；确认框回车＝拒绝 | ✅ |
| `bs self-update` | 20+ 条离线单测（假 GitHub）：缺校验和/坏校验和/自检失败都中止并保留旧文件；Windows 让位失败回滚 | ✅ |
| 未发布 Release 时的提示 | `bs self-update --check --repo kingfs/Bootstrap` → 退出码 1 + "还没有发布任何 Release" | ✅ |
| 配置项接线 | `ui.confirm` / `ui.progress` / `shell.auto_path` / `shell.auto_proxy` 都有单测与冒烟断言 | ✅ |
| 无副作用 | `make check`（fmt+vet+test+race）、`make smoke`（90 条断言，沙箱 + 不联网）、`make cross` 全绿 | ✅ |

**M4 完成判据的自查**（同样全部无副作用；"真终端"那一格靠 `make pty`）：

| 判据 | 验证方式 | 结果 |
|---|---|---|
| 全局 `--timeout` | 优先级 flag > `timeouts.global` > 内置默认；超时以退出码 1 结束并提示"进程树已停掉、已完成步骤不回滚"；取消时杀整棵进程组由 `pkg/cmdx` 单测锁定；`--timeout 0` = 不限（与"没给"同义，文档写明） | ✅ |
| `tool install -i` 勾选 | `internal/cli` 注入假 Prompter 的单测：勾选结果决定计划、非交互 → 退出码 5、与显式工具名互斥 → 退出码 2；真终端行为由 `make pty` 覆盖（勾选/取消/默认勾选） | ✅ |
| 下载断点续传 | `pkg/httpx` 单测：206 接着下、服务端忽略 `Range` 回 200 时必须**整体重下**、416 重下、**校验失败删半成品**、`.part` 名字含 URL 指纹（同 URL 才复用）、缓存 30 天未用才清理且 `--dry-run` 不清理 | ✅ |
| `bs doctor --fix` | 默认只读（有"零写入"断言）；`--fix` 只修"生成配置 / 建目录 / 装 shell 受管块"这三类结论唯一的问题；`--dry-run` 预览不落盘；非交互无 `--yes` → 退出码 5；`internal/app/doctorfix_test.go` 逐步断言写入内容 | ✅ |
| `make lint` / `make vuln` | 两者都**不在** `make check` 里（一个慢一个要联网）；工具版本钉在 Makefile，缺失时给出"先跑 make tools"的提示；`make lint` 0 issues、`make vuln` 无已知漏洞 | ✅ |
| 文档定稿 | README（安装/镜像/进度）、AGENTS（施工图 + 已知未完成项）、design（本文）、configuration（环境变量表）、tool-schema（清单字段）与实现逐条对齐；M2/M3/M4 三份判据自查各自留档 | ✅ |
| 无副作用总验收 | `make check`、`make smoke`（193 条断言）、`make pty`、`make cross`（6 个平台组合）、`make docker-matrix`（ubuntu/debian 裸镜像 + `--network none`）、`make docker-integration`（ubuntu:24.04 + 真网络 + 代理，64 条断言）、`make e2e`（真实下载）、`make release-check`（61 条断言）全绿 | ✅ |

---

## 18. 决策记录

### 18.1 已确认（评审结论，2026-10-13）

| # | 决策 | 结论 | 影响 |
|---|---|---|---|
| ADR-0001 | macOS 配置目录 | 与 Linux 统一为 `~/.config/bootstrap` | 路径解析只有一条 unix 分支；文档单一 |
| ADR-0002 | 工具清单来源 | 内置 `go:embed` + 用户本地覆盖目录 | 无远程清单、无缓存一致性逻辑；新增工具走 PR 或本地覆盖 |
| ADR-0003 | 交互依赖 | 采用 `charmbracelet/huh`，仅限 `internal/ui` 且必须可降级 | 多一组依赖树；换来 init 向导体验 |
| ADR-0004 | 首轮交付范围 | 只评审设计，不写实现代码 | 评审通过后进入 M0 |
| ADR-0005 | **运行时安装策略** | **管理器优先**：runtime 一律委托版本管理器（Go→`g`，Node→`fnm`/`nvm`，Python→`uv`，Rust→`rustup`），`bs` 只装管理器、不自己下载解压切换 | 清单里 runtime 首选 `via: manager`；release 兜底默认 `enabled: false` |
| ADR-0006 | **代码厚度** | 每个 Installer 是薄命令映射器（体量预算见 §7.3），通用纯逻辑下沉 `pkg/`（准入三条见 §4.1） | 取消 `internal/platform`、`internal/execx`，改由 `pkg/hostinfo`、`pkg/cmdx` 承担 |
| ADR-0007 | **Shell 集成形态** | 采用"同名 shell 函数包装 + rc 受管块"实现 `bs proxy on` 当前 shell 立即生效；`bs shell install` 幂等写入并可自动升级；用户 shell 片段统一放 `<config>/shell/` 随 config 一起备份 | rc 里只有一行受管内容；换机器不再 copy 脚本 |
| ADR-0008 | 分发形态 | **只发 GitHub Release 二进制**（tar.gz/zip + checksums），不做 deb/rpm/brew/scoop/winget | goreleaser 配置大幅简化 |
| ADR-0009 | 自更新校验 | 仅 sha256，不引入 cosign/sigstore | 少一层依赖与密钥管理 |
| ADR-0010 | 工具状态权威源 | **实时探测为准** + 短期缓存（TTL 10 分钟，影响动作前强制重探）；`installed.yaml` 仅作审计 | 不会出现"记录说装了其实没有" |
| ADR-0011 | `repo.root` 缺省 | 缺省 `~/src`（Windows `D:/src`，不存在则 `%USERPROFILE%\src`），`bs init` 里可改，也可随时手改配置文件 | 与你现有 `/data/src` 习惯兼容（显式配置即可） |
| ADR-0012 | **个人 shell 片段归宿** | **主要放仓库内置 `tools/snippets/`**（随二进制发布，换机器零同步，升级即带上）；`<config>/shell/` 仅作机器特定/临时覆盖 | 片段变更需提交；不再依赖 dotfiles 同步 |
| ADR-0013 | **fallback 默认值** | 交互式终端默认**开**；非交互（管道/CI/`--json`/`--non-interactive`）默认**关**；降级必须在 Report 与 `--json` 中显式标注 | 自动化场景绝不静默换安装方式 |
| ADR-0014 | **首批平台范围** | 首批清单（`base/golang/nodejs/python`）只覆盖 **linux + darwin**；Windows 首批仅保证 `bs` 本体 + `repo`/`proxy`/`shell(pwsh)` 可用，工具清单在 M5+ 单独补 | Go 用 `g` 管理的前提是其在各平台可用；Windows 不冒险 |

### 18.2 已闭环

先前列出的待确认问题（状态权威源、`repo.root` 缺省、是否发 deb/rpm/brew、自更新签名、首批清单范围、shell 片段归宿、fallback 默认、Windows 首批范围）已全部拍板，见 ADR-0008 ~ ADR-0014。设计评审到此收敛，可进入 M0。

### 18.3 后续可评估（非阻塞）

| 议题 | 说明 | 何时再看 |
|---|---|---|
| dotfiles 集成 | 是否让 `bs` 顺带 symlink/拉取 dotfiles 仓库（目前明确不做，只受管 shell 片段，且片段主要随仓库走） | M4 之后 |
| 远程清单 | ADR-0002 明确不做；若将来多机同步清单痛点明显再评估 | M5+ |
| 系统代理自动跟随 | 读取 macOS `scutil --proxy` / Windows 注册表 / Linux gsettings，让 `bs proxy` 支持 `--follow-system` | M4 |
| `mise` 作为统一兜底 | 管理器表里已注册 `mise`（`mise install/use -g`），清单里 `golang` 也已有 `has: [mise]` 的等价路径；要不要让它接管更多语言再看 | 补清单时按需 |
| Windows 工具清单与 `repo.root` | 首批不含（ADR-0014）：`tools/windows.yaml` 目前是占位文件，只保证能加载不报错；`D:/src` 约定与 scoop/winget 清单待单独设计 | M5+ |
| ~~`bs doctor --fix`~~ | ✅ 已实现：只修"结论明确、修法唯一"的问题（生成配置、建仓库根、装受管块）；补 PATH、清缓存这类仍然只提示——它们要么有副作用、要么该由安装动作顺带完成 | 已完成 |
| 跨工具并行安装 | `bs tool install` 目前严格串行（依赖顺序优先）；同层无依赖的工具并行需要先解决日志与确认交互的交错问题 | 安装变慢时 |
| ~~全局 `--timeout`~~ | ✅ 已实现：`--timeout` 给整条命令设总时限，`timeouts.*` 覆盖各环节（命令/探测/元数据/下载）；取消时杀整棵进程组 | 已完成 |
| ~~`config get\|set\|edit`~~ | ✅ 已实现（§5.3）：只改用户级配置的那一行，写前备份、写后校验、失败还原；`upgrade --all` 一并补上（§7.6） | 已完成 |
| `upgrade --all` 与"管理器还没装" | `upgrade --all` 的候选是**已安装**的工具：`golang` 装了但管理器 `g` 没装时，它的 `manager` 方式不可用（回退到 apt 又被 §7.6 挡住）→ 列为跳过。要不要在批量升级里先"补装它声明的管理器"，等真机上再遇到时定 | 有人在真机上遇到时 |
| `via: release` 的卸载只覆盖「单文件 + 软链」 | ✅ 已实现（2026-10-14，Docker 集成测试发现）：删除 `dest/binary` 与清单里声明的软链；`params.extract: true`（整包解压型，如 uv/gradle）与含 `{{version}}` 的路径**明确报不支持**并给出要手工删的目录。根治办法是安装时把「写了哪些文件」记进 state（卸载即精确回滚），当前先不引入这份记录 | 有整包解压型工具需要卸载时 |
| 系统 CA 证书检查 | ✅ 已实现：`pkg/hostinfo` 探测系统证书库（含 `SSL_CERT_FILE`/`SSL_CERT_DIR`），`bs doctor` 新增 `ca certs` 一项并按包管理器给出安装命令；`httpx` 在 x509 错误上补英文 hint。起因：ubuntu:24.04 最小镜像没有 `/etc/ssl/certs/ca-certificates.crt`，所有 HTTPS 下载都失败，而错误里没有任何指向 | 已完成 |
| `bs tool list` 的探测成本 | 目前每个条目都真起一次 `--version`（并发 8、TTL 10 分钟）；清单长到几百条时需要"只探测相关的"或懒惰探测 | 清单规模上来后 |

### 18.4 缺陷修复记录

2026-10-13 的 M2 收尾 review（用真实二进制逐条复现）发现并**已修复**下列 7 个问题：

| # | 位置 | 现象 | 修复 |
|---|---|---|---|
| 1 | `internal/install/engine.go`（`resolveParams`） | `params.version` 里的模板（`"{{versions.golang}}"`）从未渲染，命令里出现字面量 `g install {{versions.golang}}` | 引擎在选工具后、`Installer.Plan` 前统一展开"名字类"参数（`version/package/formula/cask/crate/module/id/python/version_source/source/repo`），见 `internal/install/render.go` 的 `nameParamFields` |
| 2 | `internal/install/osenv.go`（`NewOSEnv`） | `OSEnvOptions.Versions` 没赋给 `versions` 字段，`ExecEnv.Versions()` 恒为 nil | 补上赋值；`{{versions.*}}` 因此才真正可用 |
| 3 | `internal/manifest/load.go`（`mergeVerify`） | 合并 `verify` 时忽略 `over.File` | 补上 `File` 覆盖（`disabled` 仍只能置 true） |
| 4 | `internal/cli/manifest.go` | `bs tool manifest show` 没有非 JSON 分支，不打印却返回 0 | 默认输出规范化 YAML，`--json` 仍出 JSON |
| 5 | `internal/install/engine.go` | `LogPath` 无调用点，§7.5 承诺的日志没兑现 | 新增 `logEnv` 装饰器，每个工具写 `<state>/logs/<时间>-<工具>.log`（**惰性创建**：纯下载型安装没有命令，就不留空文件），失败时在 Report 里打印路径，`--dry-run` 不写 |
| 6 | `internal/install/pkgmanagers.go` | 摘要重复动词（`cargo install install fnm`） | 摘要直接用真实命令（`bin + args`） |
| 7 | `internal/cli/tool.go`（`planHasWork`） | 计划期 `failed` 不算"有活干"，`bs tool install fnm --yes` 打印 failed 后返回 0 | 新增 `failedSteps`：安装/升级/卸载遇到计划期 failed 步骤以退出码 4 结束 |

第二轮（同上，由 `internal/install/artifacts_e2e_test.go` 的离线端到端测试发现）：

| # | 现象 | 修复 |
|---|---|---|
| 8 | 探测用 runner 的 PATH 查找、安装用 `extra_path`/`path_dirs`，两边不一致 → 刚装进 `~/.local/bin` 的工具被复核判为 missing | 探测改用 `install.OSEnv` 的查找规则；`extraPathDirs` 与 `bs env path` 同一份来源 |
| 9 | 探测用 `LookPath` 找到完整路径，执行时却退回裸命令名 → "找得到却执行不了" | 用解析出的路径执行版本探测 |

第三轮（真实下载的 `make e2e` 抓到，前两轮都以为"是网络慢"）：

| # | 现象 | 修复 |
|---|---|---|
| 10 | `proxy.override_existing=false`（默认）时，`NetworkEnv()` 把 shell 里**已存在**的那组代理变量直接 `delete` 掉。`httpx` 把注入的 env 当权威（不再回退进程环境），于是**用户 shell 里明明配了代理，`bs` 自己下载却全部直连**——直连 github.com 的 release 资产既慢又常超时，而子进程（继承进程环境）却正常 | 改为"同一组变量用 shell 里那份值"（大小写一组，避免 curl 认小写、Go 认大写导致两边走不同代理）。修复后同一台机器上元数据请求从"20s 超时"变成 1.3s，两个 release 工具 9s 装完 |
| 11 | `tools/common.yaml` 里 `g`/`lazygit`/`k9s` 都误写了 `strip_components: 1`，而这三个归档**没有**外层目录，结果把二进制本身剥没了（只有真下载才暴露） | 删掉这三处（uv/ripgrep/ruff 确实有外层目录，保留）；`archive` 解出 0 个文件时立即报"nothing left after stripping"，找不到 binary 时把归档顶层条目列出来 |
| 12 | `make e2e` 不依赖 `build`，脚本用的是**过期的 `bin/bs`**——"测试通过/失败"都可能是上一版代码的结论 | `e2e: build`；脚本头部写明"自己不做构建"；PATH 收敛为沙箱 bin + `/usr/bin:/bin`（宿主已装的同名工具会让安装被跳过，e2e 就什么都验证不到），并在计划阶段就断言不是 skip |

M3 已把上面这批接线补齐：`ui.confirm`（交互会话里可关掉确认）、`ui.progress`（逐步骤进度）、
`shell.auto_path` / `shell.auto_proxy`（`bs shell init` 渲染时生效，不写 rc）、
`--interactive` / `--plain-ui`、`huh` 交互式勾选、`bs init`、`bs self-update`。
M4 又补上了全局 `--timeout`（含 `timeouts.*` 配置与"取消时杀整棵进程组"）。
`make lint`（golangci-lint，配置在 `.golangci.yml`：只保留真问题，关掉只影响风格的 `shadow`/`ST*`/`QF*`）
与 `make vuln`（govulncheck，当前 0 个可达漏洞）已接入，但**刻意不进 `make check`**：
一个慢一个要联网。

### 18.5 缺陷清单的维护方式

**这一节不再列"待修缺陷"**：截至 M4 收尾，§18.4 记录表里的 7 条（模板参数未渲染、
`Versions()` 恒为 nil、`verify.file` 无法覆盖、`manifest show` 静默返回 0、日志文件没接线、
摘要重复动词、计划期 `failed` 返回 0）与第二轮/第三轮的 5 条**都已修复**，
每一条在 §18.4 表里都能找到对应的修复说明；真正"已知且未修"的东西（Windows 清单、
`fd`/`bat` 的名字差异、交互取消只认 Ctrl-C 等）记在 §18.3 与 AGENTS.md §10，
不属于"代码与设计不一致"。

以后再往里加条目时请遵守两条：

1. **必须能用真实二进制复现**（写清命令与观察到的输出），而不是"读代码觉得不对"——
   §18.4 的第一轮 7 条就是这么筛出来的；
2. **修完就地更新同一条**：写清根因与修复，别留着一条过期描述。
   文档比代码乐观比文档滞后更糟：`config get repo.root` 的派生值、`upgrade --all` 的
   跳过语义都是这一轮才发现"文档得跟着行为改"的地方。
