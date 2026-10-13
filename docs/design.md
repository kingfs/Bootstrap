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
| 3 | 初始化时列出"已安装 / 未安装"，可交互勾选安装 | `install` + `ui` + `bs init` |
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
├── cmd/bs/main.go                 # 仅：构造 root command、执行、os.Exit(code)
├── internal/
│   ├── cli/                       # cobra 命令层（每命令一文件，薄）
│   │   ├── root.go                # 全局 flag：--config --dry-run --yes --json --verbose --no-color
│   │   ├── init.go  doctor.go  version.go  selfupdate.go  env.go
│   │   ├── config.go  proxy.go  repo.go  tool.go  shell.go
│   │   └── render.go              # 表格 / JSON / 颜色统一出口
│   ├── app/                       # 用例编排（无 cobra、无 fmt.Println）
│   │   ├── bootstrap.go           # init 向导：探测→清单→勾选→plan→apply
│   │   ├── tool_service.go        # list/info/plan/install/upgrade/uninstall
│   │   ├── repo_service.go        # get/sync/list/root
│   │   ├── proxy_service.go       # on/off/status/env/exec/test
│   │   ├── shell_service.go       # rc 受管块：渲染/安装/卸载/自检
│   │   └── env_service.go         # 汇总 proxy + PATH 导出
│   ├── domain/                    # 纯模型与接口，零 infra 依赖（可依赖 pkg/*）
│   │   ├── tool.go                # Tool, Stack, Kind, Provides, Verify, DependsOn
│   │   ├── manifest.go            # Manifest, Layer, Selection
│   │   ├── method.go              # InstallMethod 及其参数联合体
│   │   ├── installer.go           # Installer / Detector / Action / ExecEnv 接口
│   │   ├── proxy.go               # ProxyProfile, ProxyEnv
│   │   ├── shell.go               # Snippet, ManagedBlock
│   │   └── state.go               # InstalledRecord
│   ├── manifest/                  # 清单装载：embed 内置 + 用户覆盖 + 校验 + 选择
│   ├── install/                   # 各 Installer 实现 + 注册表（每个都是薄映射器）
│   │   ├── registry.go  chain.go  # 方法链选择与降级
│   │   ├── manager/               # 版本管理器注册表：g / nvm / fnm / uv / rustup / mise ...
│   │   ├── script.go  release.go  source.go
│   │   ├── bun.go  npm.go  pnpm.go  pipx.go  uvpm.go  cargo.go  goinstall.go
│   │   └── brew.go  apt.go  dnf.go  pacman.go  winget.go  scoop.go  choco.go
│   ├── detect/                    # 已安装探测、版本约束判定（用 pkg/version）
│   ├── gitrepo/                   # clone/pull/sync 编排（用 pkg/giturl + 系统 git）
│   ├── proxy/                     # profile 解析、系统代理探测、连通性测试
│   ├── shellsnip/                 # 内置/用户片段装载、排序、受管块拼装
│   ├── config/                    # 路径解析 + viper 装载 + 类型化 Config + 校验
│   ├── httpx/                     # 下载、校验和、解压（用 pkg/fsx）
│   └── ui/                        # 表格、进度、确认、颜色、huh 交互（唯一可 import huh 处）
├── pkg/                           # 纯逻辑库：无业务语义、不 import internal、可脱离本项目复用
│   ├── giturl/                    # 仓库 URL → Ref{Host,Owner,Repo,Ref,SubPath}
│   ├── version/                   # 版本抽取(命名组正则)/比较/约束
│   ├── proxyenv/                  # Profile → bash/zsh/fish/pwsh/cmd/dotenv 的 env 文本
│   ├── hostinfo/                  # OS/Arch/Distro/Shell/包管理器探测 → Platform
│   ├── cmdx/                      # 命令执行：dry-run/超时/env 注入/日志脱敏
│   └── fsx/                       # 原子写、~ 展开、目录确保、文件锁
├── tools/                         # 内置工具清单 + 内置 shell 片段（go:embed 进二进制）
│   ├── common.yaml                # 跨平台公共工具
│   ├── linux.yaml  darwin.yaml  windows.yaml
│   ├── profiles.yaml              # base / golang / python / nodejs / rust / cpp / full
│   └── snippets/{posix,fish,pwsh}/…# 内置 shell 片段（10-proxy.sh / 20-path.sh / 90-aliases.sh）
├── docs/
│   ├── design.md                  # 本文
│   ├── tool-schema.md             # 清单字段完整参考
│   ├── configuration.md           # 配置项参考
│   └── adr/                       # 架构决策记录（0001-*.md ...）
├── scripts/install.sh  install.ps1
├── .github/workflows/{ci.yaml,release.yaml}
├── .goreleaser.yaml
├── Makefile
├── README.md  AGENTS.md  LICENSE
└── go.mod
```

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
| cache（下载缓存、checksum） | `$XDG_CACHE_HOME/bootstrap` → `~/.cache/bootstrap` | `%LocalAppData%\Bootstrap\cache` |

`bs config path` 打印全部实际路径（含来源层级），`bs config path --json` 供脚本消费。

### 5.2 分层合并

```
内置默认值(代码内 defaultConfig)
  → 系统级  <system>/config.yaml
  → 用户级  <config>/config.yaml
  → 项目级  $PWD/.bootstrap.yaml（仅当存在于当前目录树，用于按项目覆盖 repo_root）
  → 环境变量 BS_*（viper AutomaticEnv + KeyReplacer "." → "_"）
  → 命令行 flag（viper.BindPFlag）
```

实现：**viper 只负责"配置"这一件事**。工具清单是数据而非配置，用 `gopkg.in/yaml.v3` 直接解析（避免 viper 的大小写折叠、类型猜测带来的坑）。viper 装载后用 `Unmarshal(&Config{})` 落到**强类型结构体**，随后走 `Validate()`；业务代码永远只读结构体，不读 `viper.Get("x.y")`。

### 5.3 配置示例

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
  | `agent` | AI Agent / 编码助手 | codex, claude-code, dsh, opencode |

以此支持：`bs tool list --stack golang`、`bs tool install --kind devdep`、`bs tool install --profile golang`。

### 6.2 工具条目 Schema（节选）

```yaml
version: 1
tools:
  # ── ① 先装版本管理器：它自己用 release/brew/goinstall 装，属于"一次性"成本 ──
  - id: g                           # 你的习惯：voidint/g
    name: g (Go version manager)
    stack: golang
    kind: versionmgr
    description: Go 多版本管理器
    homepage: https://github.com/voidint/g
    when: {os: [linux, darwin]}     # Windows 暂不纳入首批清单（ADR-0014）
    verify: {command: g, version_args: [--version], version_regex: 'g version (?P<v>[\d.]+)'}
    install:
      - via: release                # 首选：官方 release 下二进制，最可控
        params:
          repo: voidint/g
          asset: "g{{version}}-{{os}}.tar.gz"
          version_source: latest-release
          dest: "~/.local/bin"
          checksum: sha256
      - via: brew
        when: {os: [darwin, linux]}
        params: {formula: g}
      - via: goinstall              # 机器上已有 go 时最省事
        when: {has: [go]}
        params: {module: github.com/voidint/g@latest}

  # ── ② 运行时：只声明"交给哪个管理器装哪个版本"，bootstrap 不碰解压与 PATH ──
  - id: golang
    name: Go
    stack: golang
    kind: runtime
    description: Go 工具链（由 g 管理）
    homepage: https://go.dev/dl/
    depends_on: [g]                 # 依赖声明：先确保 g 就位，再执行本条目
    when: {os: [linux, darwin], arch: [amd64, arm64]}   # Windows 暂缓（ADR-0014）
    provides: ["~/.g/versions"]     # 纳入 bs env 的 PATH
    verify:
      command: go
      version_args: [version]
      version_regex: 'go version go(?P<v>\d+\.\d+(\.\d+)?)'
      min_version: "1.22"           # 可选：低于则视为需升级
    install:
      - via: manager                # 首选且通常唯一：委托 g
        params: {manager: g, action: install, version: "{{versions.golang}}"}
      - via: manager                # 机器上已有 mise 时的等价路径
        when: {has: [mise]}
        params: {manager: mise, action: use, version: "go@{{versions.golang}}", global: true}
      - via: release                # 可选兜底：不想依赖管理器时启用（默认注释掉，保持"薄"）
        enabled: false
        params:
          repo: golang/go
          asset: "go{{version}}.{{os}}-{{arch}}.tar.gz"
          version_source: https://go.dev/VERSION?m=text
          strip_components: 1
          dest: "~/.local/go"
          symlink: {from: "~/.local/go/bin/go", to: "~/.local/bin/go"}
          checksum: sha256
    update:
      via: manager
      params: {manager: g, action: upgrade}
    uninstall:
      - via: manager
        params: {manager: g, action: uninstall, version: "{{versions.golang}}"}

  - id: nodejs                      # 同一套范式：Node 交给 fnm/nvm，而不是自己下载
    name: Node.js
    stack: nodejs
    kind: runtime
    description: Node.js（由 fnm/nvm 管理）
    depends_on: [fnm]               # 若用户 managers.nodejs = nvm，则依赖自动换成 nvm
    when: {os: [linux, darwin]}     # Windows 暂缓（ADR-0014）
    verify: {command: node, version_args: [--version], version_regex: 'v(?P<v>\d+\.\d+\.\d+)'}
    install:
      - via: manager
        params: {manager: fnm, action: install, version: "{{versions.nodejs}}"}
      - via: manager
        when: {has: [nvm]}          # nvm 是 shell 函数而非二进制 → 注册表里带 source 脚本
        params: {manager: nvm, action: install, version: "{{versions.nodejs}}", source: "~/.nvm/nvm.sh"}

  - id: fnm
    name: fnm
    stack: nodejs
    kind: versionmgr
    homepage: https://github.com/Schniz/fnm
    when: {os: [linux, darwin]}
    verify: {command: fnm, version_args: [--version], version_regex: '(?P<v>\d+\.\d+\.\d+)'}
    install:
      - via: brew
        when: {os: [darwin]}
        params: {formula: fnm}
      - via: script
        when: {os: [linux]}
        params: {url: "https://fnm.vercel.app/install", sha256: "<固化>"}
      - via: release
        when: {os: [windows]}
        params: {repo: Schniz/fnm, asset: "fnm-windows.zip", dest: "~/.local/bin", checksum: sha256}

  - id: btop
    name: btop
    stack: base
    kind: cli
    description: 资源监控 TUI
    when: {os: [linux, darwin]}     # windows 不支持 → 自动从清单隐藏
    verify: {command: btop, version_args: [--version], version_regex: 'btop version (?P<v>[\d.]+)'}
    install:
      - via: brew
        when: {os: [darwin]}
        params: {formula: btop}
      - via: apt
        when: {os: [linux], distro: [ubuntu, debian]}
        params: {packages: [btop], needs_root: true}
      - via: dnf
        when: {os: [linux], distro: [fedora, rhel]}
        params: {packages: [btop], needs_root: true}
      - via: source
        when: {os: [linux]}
        params: {repo: aristocratos/btop, build: "make -j$(nproc) && sudo make install"}

  - id: codex
    name: Codex CLI
    stack: agent
    kind: agent
    when: {os: [linux, darwin, windows]}
    verify: {command: codex, version_args: [--version]}
    install:
      - via: bun                    # 首选：已有 bun 就极快
        when: {has: [bun]}
        params: {package: "@openai/codex", global: true}
      - via: npm
        when: {has: [npm]}
        params: {package: "@openai/codex", global: true}
      - via: script                 # 官方脚本兜底
        when: {os: [linux, darwin]}
        params: {url: "https://example.com/install.sh", sha256: "..." }
```

**新增的四个 Schema 元素**（都服务于"薄实现 + 管理器优先"）：

| 元素 | 位置 | 作用 |
|---|---|---|
| `kind: versionmgr` | 工具级 | 标记"这是一个版本管理器"，可被 runtime 条目 `depends_on` |
| `depends_on: [g]` | 工具级 | 显式依赖；安装按 DAG 拓扑排序，`golang` 会先触发 `g` 的安装 |
| `via: manager` | 方法级 | 委托已安装的管理器执行；params 是**语义化的**：`{manager, action: install\|use\|upgrade\|uninstall, version, global}`，不写具体命令行 |
| `enabled: false` | 方法级 | 保留但默认禁用某种方法（如"不想要 release 兜底"），用户覆盖时可一键启用 |

**`via: manager` 的实现方式**：`internal/install/manager/registry.go` 维护一张小表——

```go
// 每个管理器就是一组命令模板，没有任何下载/解压/切换逻辑（这才是"薄"）。
type Manager struct {
    Name        string   // g / fnm / nvm / uv / rustup / mise
    Bin         string   // 探测用的可执行文件名
    NeedsShell  bool     // nvm 是 shell 函数 → 需要 source 脚本后执行
    Source      string   // 例："~/.nvm/nvm.sh"
    Install     string   // "g install {{version}}" / "fnm install {{version}}"
    Use         string   // "fnm use {{version}}" / "mise use -g {{version}}"
    Upgrade     string   // "g upgrade" / "rustup update"
    Uninstall   string   // "g uninstall {{version}}"
    ListRemote  string   // "g ls-remote"（用于 latest 解析）
}
```

- `managers.<stack>`（配置）决定同名工具的 `depends_on` 自动改写与 `via: manager` 的落地目标：写 `nodejs: nvm` 就把 Node 条目的依赖从 `fnm` 换成 `nvm`，**清单无需分叉**。
- `version: latest` 由 `ListRemote` 解析为具体版本，模板再渲染命令。
- 需要一条清单里表达不了的怪命令时，用 `params: {command: "..."}` 逃生口，但仍属于该 `manager` 的语义范围。
- **禁止**在 `manager` 里实现版本下载、镜像、PATH 切换——那是管理器自己的事；`bs` 只负责"确保管理器在、把命令发出去、验证结果"。

### 6.3 清单分层与合并

加载顺序（后者覆盖前者，按 `id` 合并、按字段深合并）：

```
① 二进制内置（go:embed，只读）
   tools/common.yaml         ← 跨平台公共条目（满足"有些工具无系统限制"）
   tools/<GOOS>.yaml         ← 系统专属条目
② 用户覆盖（可读写，按顺序后覆盖前）
   <config>/tools/*.yaml     ← 改参数 / 新增工具 / disabled: true 禁用
```

> **ADR-0002（已决策）**：清单来源 = **内置 embed + 用户本地覆盖目录**，不实现远程清单仓库。
> 理由：离线可用、无网络与缓存一致性负担，二进制即"已知良好清单"的快照；新增/修正工具走本仓库 PR 或本地覆盖文件。远程清单若未来需要，作为独立增量能力（M5+）评估，不预留半成品开关。

- `tools/profiles.yaml` 定义"套装"：`base: [git, curl, jq, rg, fd, ...]`、`golang: [g, golang, gopls, dlv, golangci-lint]`、`nodejs: [fnm, nodejs, pnpm]`、`python: [uv, python3-dev]`、`rust: [rustup, cargo-tools]`……profile 也可由用户覆盖扩展。
- 合并规则明确：**数组类型的字段（`install`、`tags`）整体替换而非追加**，除非用显式 `+install:` 追加语法；这样用户覆盖不会产生"意料之外的半合并"。`disabled: true`（整条禁用）与 `enabled: false`（单个方法禁用）是最常用的覆盖手段。
- `bs tool list` 默认只显示当前平台**可见**的条目；`--all` 显示全部并标注"不适用于当前平台"。

### 6.4 校验（`bs manifest validate`，CI 中必跑）

- `id` 唯一、命名合法；`stack`/`kind` 在枚举内；
- `verify` 必须有 `command` 或 `version_regex` 之一；
- 每个 `via` 有对应 Installer 实现且 `params` 通过该 Installer 的 schema 校验；
- `via: manager` 的 `manager` 必须在注册表中存在，且该管理器支持所声明的 `action`；
- `depends_on` 引用的 id 存在、**无环**（DAG 校验），且版本管理器依赖不会跨 stack 乱指；
- `when` 的 os/arch/distro 值合法；
- 引用的 profile 中的 id 均存在；
- 每个 `runtime` 条目至少有一条 `via: manager` 路径，或显式标注 `manager_optional: true`（防止有人悄悄退回"自己下载解压"的重实现）。

---

## 7. 安装引擎

### 7.1 端口（domain 层接口）

```go
// Installer 是"一种安装机制"的抽象实现（bun / brew / apt / script / release / source ...）。
type Installer interface {
    // Name 与 YAML 中 method.via 对应。
    Name() string
    // Available 判断该机制在当前机器上是否可用（二进制存在、包管理器存在等）。
    Available(ctx context.Context, p Platform) bool
    // Plan 只做"将要发生什么"的静态计算，不产生副作用。
    Plan(ctx context.Context, t Tool, m InstallMethod, p Platform) (Action, error)
    // Execute 执行 Action，返回原始输出与错误。
    Execute(ctx context.Context, a Action, e ExecEnv) error
    // Uninstall / Upgrade 可选实现，通过能力接口探测（见下）。
}

type Action struct {
    ToolID   string
    Via      string
    Commands []Command   // 人类可读 + 可执行
    Downloads []Download
    NeedsRoot bool
    Network   bool       // 是否需要代理注入
    Reason    string     // 为什么选择这个方法（plan 输出用）
}

// 可选能力，用类型断言探测，避免接口膨胀：
type Uninstaller interface { Uninstall(ctx, Tool, InstallMethod, ExecEnv) error }
type Upgrader    interface { Upgrade(ctx, Tool, InstallMethod, ExecEnv) error }
```

**核心执行接口**（唯一允许碰系统的出口）：

```go
type ExecEnv interface {
    Run(ctx context.Context, c Command) (Result, error)
    Download(ctx context.Context, d Download) (string, error)
    FS() fsx.FS
    Env() map[string]string    // 已注入 proxy/PATH
    DryRun() bool
    Log() *slog.Logger
}
```

真实现：`cmdx.OSExecEnv`；测试实现：`cmdx.FakeExecEnv`（记录命令、可编排返回）。所有 installer 的单元测试都用 Fake，无需网络与特权。

### 7.2 方法链选择与降级（需求 8）

选择算法（`install.chain.Resolve`）：

1. 用户显式指定 `--via <name>`（或配置 `tool.default_via`）：只在链中匹配该方法；不存在则报错并列出可用方法。
2. 否则**严格按清单里 `install` 的书写顺序**自上而下遍历（顺序即策略，不在代码里硬编码优先级）：
   - `enabled: false` → 跳过；
   - `when` 条件不匹配 → 跳过并记录原因（`--verbose`/`plan -v` 展示）；
   - `when.has: [bun]` 这类**工具存在性条件**不满足 → 跳过；
   - Installer 的 `Available()` 为 false（如无 brew、`manager` 对应的管理器未安装）→ 跳过；
   - 命中第一个可用方法 → 选中，并把"为什么是它"写进 `Action.Reason`。
3. 若选中方法执行失败：
   - **`--fallback` / `--no-fallback`（ADR-0013）**：默认值随场景变化——**交互式终端默认开**（能自救就自救），**非交互（管道/CI/`--json`/`--non-interactive`）默认关**（绝不静默换安装方式，避免"本想用 bun 装，结果悄悄用 apt 装了个旧版本"）。`--fallback` / `--no-fallback` 可显式覆盖该默认。
   - 降级发生时**必须在 Report 里显著标注**："bun 安装失败 → 已改用 npm"，`--json` 输出含 `fallback_from`/`fallback_reason` 字段。
4. 全部方法失败 → 该工具失败，汇总**每个方法各自的失败原因**（而不是只报最后一个）。

**推荐书写顺序**（`bs manifest validate` 会按此给出 lint 警告，不强制）：

```
① manager        → 版本管理器（g / fnm / nvm / uv / rustup / mise），runtime 的首选
② 专用包管理器   → bun / npm / pnpm / pipx / cargo / goinstall（装 CLI 工具与 agent）
③ script         → 官方安装脚本（必须固化 sha256）
④ release        → 官方 release 下二进制（管理器自身、无包管理器的工具）
⑤ 系统包管理器   → brew / apt / dnf / pacman / winget / scoop（只用于不常升级的基础依赖）
⑥ source         → 源码编译（最后兜底，尽量少用）
```

**系统包管理器的护栏**：默认只允许 `apt`/`dnf`/`winget` 等系统包管理器用于 `kind ∈ {devdep, service, cli}`；若把 `runtime` 交给它们（例如用 apt 装 go，版本往往很旧），validate 会报错，需要 `tool.allow_system_for` 或该方法的 `allow_system: true` 显式放行。这条护栏就是"不期望有沉重实现"的另一面——**也不期望用错机制**。

**依赖顺序**：`depends_on` 构成 DAG，`Apply` 阶段按拓扑序执行（例：`g → golang → gopls`）。DAG 校验在清单加载期完成，环依赖直接报错。


### 7.3 Installer 实现清单（首批）与体量预算

| via | 平台 | 参数要点 | 体量预算 | 备注 |
|---|---|---|---|---|
| `manager` | 全 | `manager`, `action`, `version`, `global`, `source` | ~120 行（含注册表） | **委托版本管理器**，runtime 的首选；不含下载/切换逻辑 |
| `bun` | 全 | `package`, `global` | ~40 行 | 首选安装 agent 类 CLI |
| `npm` / `pnpm` | 全 | `package`, `global`, `prefix` | ~50 行 | 非 root 全局安装走 `prefix` |
| `pipx` / `uvpm` | 全 | `package`, `python` | ~50 行 | Python CLI 隔离安装（`uv tool install`） |
| `cargo` | 全 | `crate`, `features`, `git` | ~40 行 | Rust 工具 |
| `goinstall` | 全 | `module`, `version` | ~40 行 | Go CLI |
| `brew` | mac/linux | `formula`/`cask`, `tap` | ~60 行 | 不用 `sudo` |
| `apt` / `dnf` / `pacman` | linux | `packages`, `repo`, `needs_root` | ~180 行（共享基类） | 基础 devdep、btop 之类 |
| `winget` / `scoop` / `choco` | windows | `id`/`package` | ~120 行（共享基类） | Windows 主路径 |
| `script` | 全 | `url`, `sha256`, `args`, `shell` | ~80 行 | `curl … \| bash` 类官方脚本；**必须固化 sha256** 或显式 `--insecure-script` |
| `release` | 全 | `repo`, `asset` 模板, `version_source`, `dest`, `strip_components`, `symlink`, `checksum` | ~150 行 | 从 GitHub Release 下载解压；管理器自身与无包管理器工具用 |
| `source` | 全 | `repo`/`url`, `build`, `install` | ~60 行 | 最后兜底 |

> 预算合计约 1000–1200 行，这是**整个安装引擎的上限**，也是"薄实现"的可度量约束：任何单个 Installer 明显超出预算，说明该抽象选错了层级（应该改成 `via: manager`，或者把通用部分下沉 `pkg/`）。

模板渲染：`asset: "g{{version}}-{{os}}.tar.gz"` 支持 `{{os}} {{arch}} {{version}} {{tag}}`、配置引用 `{{versions.golang}}`、以及过滤器 `{{arch|archmap:amd64=x86_64,arm64=aarch64}}`（不同项目命名差异巨大，映射能力必须有；`osmap` 同理）。

### 7.4 探测（需求 3 的"已安装/未安装"）

`detect.Detector`：

- **存在性**：`exec.LookPath` 优先；`release`/`source` 类另做"路径存在 + 可执行"检查（`provides` 路径）。
- **版本**：执行 `verify.version_args`，用带命名组的 `version_regex` 抽取版本（正则编译在清单加载期完成）。
- **约束**：`min_version` / `exact_version` 用语义化比较（自实现轻量 semver 比较，不引重依赖）。
- **结果三态**：`Installed` / `Outdated` / `Missing`，外加 `Unknown`（命令存在但版本解析失败，不阻塞安装，只警告）。
- 并发探测（默认 `min(8, NumCPU)`），带 per-command 超时（默认 5s），避免某个卡死的 `--version` 拖慢整体。
- 结果缓存到 state 目录（TTL 短，如 10 分钟），`--refresh` 强制重探；**缓存只加速显示，判定前若结果影响动作（要跳过安装）则强制重探**。

### 7.5 执行流水线（`bs init` / `bs tool install`）

```
[1] Preflight
     · 平台/发行版/Shell 探测                       · 必需基础命令检查（git, curl）
     · 网络与代理可达性（可选 --check-network）      · 权限模式（是否需要 sudo，能否免密）
     · 目标路径可写
[2] Select        按 profile / stack / kind / 交互勾选 → []Tool
[3] Plan          逐工具 Resolve 方法链 → []Action
[4] Confirm       表格：工具 | 状态 | 将用方式 | 命令摘要；支持 a=全选 n=全不选 1,3,5=选择
                  --yes 跳过；--dry-run 到此结束并打印命令
[5] Apply         按 **依赖顺序** 串行执行（工具可声明 depends_on），工具之间默认串行，
                  同层无依赖可并行（--jobs）
[6] Verify        执行后重新探测，失败则该工具标记 FAIL 并给出日志路径
[7] Report        成功/跳过/失败 汇总；失败项给出"下一步建议"；退出码 1
```

失败策略：默认 `--keep-going`（单个工具失败不中断整体），最终汇总非零退出；`--fail-fast` 可选。

**日志与审计**：每条命令的 stdout/stderr 落到 `<state>/logs/<ts>-<toolid>.log`，终端只显示摘要；`bs tool install --verbose` 实时透传。安装成功写 `<state>/installed.yaml`（`id, version, via, at, commands`）作为审计与 `bs doctor` 依据。

### 7.6 卸载与升级

- `upgrade`：先检测，`Outdated` 才动作；按 `tool.update.via`（缺省复用 install 链）执行；对包管理器类走 `brew upgrade` / `bun update -g` / `cargo install --force` 等原生语义；`apt` 类标记 `managed: system`，默认**不主动升级**（符合"不需要及时升级"的定位），除非 `--include-system`。
- `uninstall`：仅对通过非系统包管理器（bun/npm/cargo/goinstall/release/source）安装的工具提供；`apt/brew/winget` 类交给原生包管理器，`bs` 只做提示或 `--allow-system-uninstall` 透传。

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
- `bs shell snippet list|new <name>|edit <name>|show <name>|path` 管理用户片段；`bs shell init` 会把这些片段内容**内联渲染**进输出（所以 rc 里永远只有一行）。

**命令**：

```
bs shell init <bash|zsh|fish|pwsh|cmd>   # 渲染完整受管内容（包装函数 + 片段 + PATH/代理装配）
bs shell install [--rc <file>] [--shell S] [--dry-run]
                                         # 幂等写入 rc 的受管块；自动备份；--dry-run 只显示 diff
bs shell uninstall [--rc <file>]         # 精确移除受管块（块外内容零改动）
bs shell status [--json]                 # 受管块是否存在/是否过期/与当前二进制是否同步
bs shell update                          # 重写受管块（升级 bs 后刷新）
bs shell snippet list|new|edit|show|path|remove
bs shell [--profile P] [--shell S]       # 不改 rc：直接开一个已装配环境 + PATH 的子 shell
```

**受管块格式**（幂等替换的锚点，块外字节级不动）：

```bash
# >>> bootstrap (managed by 'bs shell install'; edit snippets in ~/.config/bootstrap/shell) >>>
eval "$(bs shell init bash)"
# <<< bootstrap <<<
```

- `bs shell install` 的行为：定位/探测 rc → 若存在受管块则**原地替换**（升级即生效）→ 否则追加 → 写前备份 `<rc>.bs-backup.<ts>` → 原子写入。
- `bs shell status` 对比"rc 中的块内容"与"当前版本应输出的锚点行"，提示是否需要 `bs shell update`（例如你换了 shell 或改了 `shell.rc_files`）。
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
| `https://github.com/kingfs/Bootstrap/tree/feat/x/internal/foo` | repo=`Bootstrap`, ref=`feat/x`, subpath=`internal/foo` |
| `https://github.com/kingfs/Bootstrap/blob/main/README.md` | repo 同上, ref=`main`, subpath=`README.md` |
| `https://github.com/kingfs/Bootstrap/releases/tag/v0.3.0` | ref=`v0.3.0`（tag） |
| `https://github.com/kingfs/Bootstrap/pull/12` | repo + `pull/12` 元信息（提示可 `--fetch pr/12`） |
| `https://github.com/kingfs/Bootstrap?tab=readme` | 忽略 query |
| `github.com/kingfs/Bootstrap`（无 scheme） | 识别首段含 `.` → 视为 host |

算法：去空白 → 去 query/fragment（`#` 可作 ref 分隔符）→ 去尾部 `/` → 去 `.git` → 处理 scp-like → 切分路径 → 首段含 `.` 或等于已配置 host 视为 host，否则用 `default_host` → 取 `owner`（GitLab 支持多段，以最后一段为 repo）→ 余下段匹配 `tree|blob|commit|releases/tag|pull|compare` 提取 ref/subpath → 校验 owner/repo 字符集。

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
- `--depth` 支持浅克隆，`--sparse` 后续增强。

### 9.4 批量能力

```
bs repo get kingfs/Bootstrap other/repo https://github.com/x/y   # 批量
bs repo get -f repos.txt                                         # 从文件读（每行一个，支持 # 注释）
bs repo list [--root <path>] [--dirty] [--json]                  # 枚举 root 下所有仓库
bs repo sync [--jobs 4] [--root ...] [--only-dirty]              # 批量 pull，汇总表格
```

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

```
bs [--config FILE] [--dry-run] [--yes] [--json] [-v|-vv] [--no-color] <command>

init                 交互式初始化：探测环境 → 选 profile → 展示工具清单(已装/未装) → 勾选 → plan → 安装
                     （结束时可选执行 bs shell install，把 shell 受管块一次装好）
doctor               环境自检：平台信息、必需命令、包管理器/版本管理器、代理状态、PATH、rc 受管块、配置健康度
version              版本信息（--json），含 commit/date/build tags
self-update          从 GitHub Release 自更新（校验 sha256）

config path          打印所有配置/状态/缓存路径与实际来源
config show|get|set|edit|init|validate

proxy  list|show|on|use|off|status|env[--apply|--print]|exec|test|add|remove

repo   get <url...> | list | sync | root | info <url>
repo   get 别名：clone、pull（`bs clone x/y` 等价 `bs repo get x/y`）

tool   list [--stack S] [--kind K] [--profile P] [--installed|--missing] [--all]
tool   info <id>
tool   search <keyword>
tool   plan [--profile P] [--stack S] [-v]
tool   install [id...] [--profile P] [--stack S] [--kind K] [--via V] [--no-fallback] [--jobs N] [-i]
tool   upgrade [id...|--all] [--include-system]
tool   uninstall <id...> [--allow-system]
tool   manifest path|validate|diff        # 清单已随二进制发布，故无 update

env    show|export|path [--shell ...]     # 汇总 proxy + PATH（含各工具 provides 的 bin 目录）
shell  init <bash|zsh|fish|pwsh|cmd>      # 渲染受管内容（bs 包装函数 + 片段 + PATH/代理装配）
shell  install|uninstall|status|update    # rc 受管块的写入/移除/自检/刷新
shell  snippet list|new|edit|show|path|remove
shell  [--profile P]                      # 不改 rc，直接开一个已装配环境的子 shell
```

**退出码约定**（Agent 与 CI 友好）：`0` 成功；`1` 一般失败；`2` 用法/参数错误（cobra 默认）；`3` 部分成功（keep-going 下有失败项）；`4` 前置条件不满足（如缺 git、目标不可写）；`5` 需要交互但处于非交互模式。

**脚本化约定**：非 TTY 时自动 `--json` + `--yes`（除非显式 `--interactive`），绝不阻塞等待输入。

---

## 12. 输出与人机交互

- **人类**：彩色表格（工具 | 状态 | 版本 | 安装方式 | 备注），进度条式步骤计数，失败时给出可复制粘贴的修复命令。
- **机器**：`--json` 输出稳定 schema（`{"version":1,"command":"tool.plan","data":{...}}`），字段命名 snake_case，方便 Agent 解析。
- **交互多选**（**ADR-0003，已决策**）：采用 `charmbracelet/huh` 实现 `bs init` 向导与 `bs tool install -i` 的搜索/多选/确认（含分组展示、方向键与空格勾选）。约束：
  - 依赖只允许出现在 `internal/ui`，`domain`/`app` 不得 import；UI 通过 `ui.Prompter` 接口注入，`app` 只认接口；
  - 同时保留**纯文本降级模式**：非 TTY、`--json`、`NO_COLOR`、`--non-interactive` 或 `CI=1` 时不用 huh，改为打印计划并要求 `--yes`，绝不在非交互环境阻塞；
  - `--yes` / `--dry-run` 时完全旁路交互层。
- **日志**：`slog` 结构化日志，默认 stderr，`debug` 模式含每条外部命令的完整 argv 与退出码；文件日志始终写全量。

---

## 13. 工程化约定

| 关注点 | 方案 |
|---|---|
| 错误处理 | `fmt.Errorf("...: %w", err)` 包装；领域错误用 Sentinel + `errors.Is/As`；CLI 边界统一转用户可读消息 + 退出码 |
| 上下文 | 所有 IO 函数首参 `context.Context`；全局 `--timeout`（默认命令 10min、探测 5s、HTTP 60s） |
| 并发 | 探测/同步用 `errgroup`，并发度可配；共享状态用 mutex 或 channel，禁用裸 goroutine |
| 配置注入 | 依赖显式构造（构造函数注入），不用 `init()` 副作用；唯一 `init()` 用于 installer 注册表 |
| 测试 | 表驱动单测 + `cmdx.FakeExecEnv`；`testdata/golden` 校验 plan 输出；`pkg/giturl.Parse` 用大表覆盖 URL 变体；集成测试用 `//go:build integration` 在 Docker(ubuntu/debian/fedora/arch) 中跑真实安装 |
| 静态检查 | `golangci-lint`（govet, staticcheck, errcheck, revive, gocritic, gosec, misspell, gofumpt）；`govulncheck`；`go test -race` |
| 依赖策略 | 新增依赖需在 PR 说明理由；优先标准库；UI 类依赖不得进入 `domain`/`app`；`pkg/*` 不得 import `internal/*`（CI 用 `go list` 检查） |
| 提交规范 | Conventional Commits（`feat(proxy): ...`），中文正文允许；每 PR 附"如何验证" |
| 版本管理 | `ldflags -X main.version/commit/date`；`bs version --json` 暴露 |
| 幂等与安全 | 脚本安装必须校验 sha256（清单里固化）；文件写入用原子替换（tmp + rename）；state 写入加文件锁 |

**首批依赖基线**（超出此列表的新增必须在 PR/ADR 说明理由）：

| 依赖 | 用途 |
|---|---|
| `github.com/spf13/cobra` | 命令树、flag、completion、help |
| `github.com/spf13/viper` | 分层配置装载、env/flag 绑定 |
| `gopkg.in/yaml.v3` | 工具清单与 state 文件解析（不经 viper） |
| `golang.org/x/sync` | `errgroup`（并发探测/同步） |
| `github.com/charmbracelet/huh` `lipgloss` | 交互向导与终端样式（仅 `internal/ui`，ADR-0003） |
| `log/slog` | 结构化日志（标准库，非第三方） |

明确**不引入**：`go-git`（用系统 git）、`docker client`、完整 TUI 框架（bubbletea 仅在确有需求时评估）、任何需要 cgo 的库。

---

## 14. 构建与发布

> **ADR-0008（已决策）**：**唯一分发渠道是 GitHub Release 的二进制包**，不做 deb/rpm、不做 Homebrew tap、不做 scoop/winget 清单发布。理由：你的流程是"Release 下载二进制 → 跑 bootstrap"，中间商只会增加维护面。

- **目标产物**：`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`，`windows/arm64` 视需要构建。`CGO_ENABLED=0` 纯静态、体积小、交叉编译零负担。
- **GoReleaser**（`.goreleaser.yaml`）——只保留必要项：
  - `builds`：六目标矩阵 + `ldflags -X main.version/commit/date`；
  - `archives`：`bs_<version>_<os>_<arch>.tar.gz`（unix）/ `.zip`（windows），内含 `bs(.exe)`、`LICENSE`、`README.md`；
  - `checksum`：`checksums.txt`（sha256），供引导脚本与 `bs self-update` 校验；
  - `changelog`：按 Conventional Commits 分组；
  - **不配置** `nfpm`、`brews`、`scoop`、`winget`。
- **GitHub Actions**：
  - `ci.yaml`：lint → unit test(race) → `bs manifest validate` → 三 OS 原生构建（确保 Windows/macOS 代码路径被真正编译与测试）；
  - `release.yaml`：`v*` tag 触发 goreleaser，产出 Release + 附件。
- **引导安装**（鸡生蛋闭环）：
  - `scripts/install.sh`：`curl -fsSL .../install.sh | bash` —— 探测 OS/Arch、下载对应产物、比对 `checksums.txt`、装到 `~/.local/bin`（可 `BS_BIN_DIR` 覆盖）、提示 `eval "$(bs shell init bash)"`；
  - `scripts/install.ps1`：`irm .../install.ps1 | iex` —— 装到 `%USERPROFILE%\.local\bin` 并写入用户 PATH。
  - 两个脚本都要能"重跑即升级"（幂等），并支持 `--version vX.Y.Z` 指定版本。
- **`bs self-update`**：从 Release 拉 `checksums.txt` → 下载当前平台产物 → 校验 sha256 → 原子替换自身二进制（Windows 上先重命名旧文件再替换）→ 打印新版本。**仅 sha256，不引入 cosign**（ADR-0009）。
- **Makefile**：`make build|test|lint|snapshot|release|run`（`snapshot` = `goreleaser release --snapshot --clean`）。

---

## 15. 文档体系

- **`README.md`（给人看，简洁）**：一段话定位 → 安装（三平台各一行）→ **5 分钟上手**（`bs init`）→ 常用命令速查表（repo/proxy/tool/env 各 2–4 条）→ 配置示例 → 卸载。
- **`AGENTS.md`（给 AI Agent 看）** 必含：项目定位与不变量、**构建/测试/校验命令**、目录地图与分层依赖规则、**如何新增一个工具（只改 YAML 的三步流程）**、如何新增一种安装机制（实现 Installer + 注册 + 测试）、代码风格与提交规范、Definition of Done 清单、禁止事项（如禁止在 `cli` 层写业务逻辑、禁止无 sha256 的脚本安装）。
- **`docs/`**：`tool-schema.md`（清单字段全参考 + 每个 via 的 params）、`configuration.md`（配置项全参考 + 示例）、`design.md`（本文）、`adr/`（关键决策：为什么用系统 git、为什么 plan/apply 分离、为什么 macOS 用 `~/.config` 等）。
- **命令内建文档**：每个 cobra 命令有 `Long` + `Example`；`bs completion <shell>` 由 cobra 自动提供；`bs <cmd> --help` 即权威参考。

---

## 16. 安全边界

1. `curl | bash` 类安装必须固化 `sha256` 或要求 `--insecure-script` 显式放行，并在 plan 中显著标注。
2. 需要 root 的操作在 plan 中标注 `needs_root`，执行前二次确认；`--yes` 时也打印将执行的 `sudo` 命令。
3. 代理凭据不落明文日志；日志中 `*_PROXY` 值做掩码（保留 host:port，隐去 userinfo）。
4. `bs self-update` 仅校验 sha256（ADR-0009）；签名校验明确不做。
5. rc 文件只允许通过 `bs shell install` 的**受管块**修改：块由固定标记包围、可幂等替换、可精确移除，块外字节级不动；写前自动备份。其余场景一律只**输出**片段，不写文件。

---

## 17. 演进路线

策略：**先把整条 CLI 主干打通并能自举，再持续往清单里补工具**。每个阶段结束都必须"可编译、可发布、可用"。

| 阶段 | 目标 | 交付 | 完成判据 |
|---|---|---|---|
| **M0 骨架** | 能发布一个"什么都不能干但装得上"的 `bs` | go.mod 定版、目录结构、cobra root、`config path/show`、`version`、goreleaser + CI、`install.sh`/`install.ps1` | 六平台产物可下载；新机器一行命令装上 `bs version` |
| **M1 CLI 主干跑通** | 高频能力先可用 | `pkg/{giturl,version,proxyenv,hostinfo,cmdx,fsx}`；`proxy` 全套；`shellsnip` + `shell init/install/snippet`；`repo get/list/sync`；`env export`；`doctor`（基础版） | 新机器 `bs shell install` → `bs proxy on` 当前 shell 立即生效；`bs repo get kingfs/xxx` 可 clone/pull |
| **M2 工具引擎** | 清单驱动安装 | 清单 schema + 加载/合并/校验；`detect`；`plan`；首批 Installer：`manager`(g/fnm/uv/rustup/mise)、`brew`、`apt`、`winget`、`script`、`release`、`bun`、`npm`、`cargo`、`goinstall`、`source`；`tool list/info/plan/install` | `bs tool list` 能看到三平台差异；`bs tool install golang` 走 `g` 装好并可用 |
| **M3 初始闭环** | 新机器一把梭（**linux + darwin**） | `bs init` 向导（huh：已装/未装 + 勾选）→ plan → apply → 可选 `bs shell install`；`profiles.yaml`；首批清单 `base/golang/nodejs/python`；`self-update`；`upgrade`/`uninstall` | linux/arm64 与 darwin/arm64（可加 amd64）空机器上跑完 `bs init` 后 go/node/python 均可用 |
| **M4 打磨** | 长期好用 | `--json` 全覆盖、集成测试矩阵（Docker）、并发 `repo sync`、`proxy test`、shell completion、README/AGENTS.md/ADR 定稿 | 文档与命令 --help 一致；CI 全绿 |
| **M5+ 持续补全** | 你不断把喜欢的工具链补进来 | ① Windows 清单（`windows.yaml`：scoop/winget + pwsh 片段 + `repo.root` 的 `D:/src` 约定）；② 按 stack 补齐 rust/cpp/java/containers/agent；③ `bs tool manifest diff` 辅助发现本地覆盖漂移 | 每个新工具只改 YAML + 一条测试 |

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

### 18.3 后续可评估（非阻塞，不影响 M0–M3）

| 议题 | 说明 | 何时再看 |
|---|---|---|
| dotfiles 集成 | 是否让 `bs` 顺带 symlink/拉取 dotfiles 仓库（目前明确不做，只受管 shell 片段，且片段主要随仓库走） | M4 之后 |
| 远程清单 | ADR-0002 明确不做；若将来多机同步清单痛点明显再评估 | M5+ |
| 系统代理自动跟随 | 读取 macOS `scutil --proxy` / Windows 注册表 / Linux gsettings，让 `bs proxy` 支持 `--follow-system` | M4 |
| `mise` 作为统一兜底 | 若某些语言你不想为它单独配管理器，`mise` 可一个工具管多语言 | 补清单时按需 |
| Windows 工具清单与 `repo.root` | 首批不含；`D:/src` 约定与 scoop/winget 清单待 M5+ 单独设计 | M5+ |
| Windows arm64 产物 | 目前列为可选；若你有 arm64 Windows 机器再加入矩阵 | 需求出现时 |
| `bs doctor --fix` | 是否让 doctor 自动修复（补 PATH、重建受管块、清理缓存），还是只诊断 | M4 |
