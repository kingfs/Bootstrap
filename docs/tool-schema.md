# 工具清单字段速查（`tools/*.yaml`）

这份文档是**清单字段的权威说明**，与 `internal/manifest/types.go`、`internal/install/` 一一对应。
改代码时若动了字段，请同步这里（AGENTS.md §9）。

清单是声明式的：条目只说"我要什么、怎么判断装没装"，不说"怎么解压、怎么切 PATH"。
那些是安装引擎的事，由 `via` 后面的实现决定。

```sh
bs tool manifest path       # 清单从哪来、按什么顺序合并
bs tool manifest validate   # 写错了会一次性全部报出来（含模板变量拼写）
bs tool manifest show       # 合并后的实际结果
bs tool manifest diff x.yaml  # 我的草稿和当前结果差在哪
bs tool manifest validate --os darwin   # 在 Linux 上校验 macOS 的清单
```

## 1. 文件结构

```yaml
version: 1                 # 目前恒为 1，将来做不兼容变更时递增

tools:                     # 工具条目列表
  - id: ripgrep
    ...

profiles:                  # 套装：一组工具 id，可继承
  base: [git, jq]                    # 简写
  golang:                            # 完整形式
    description: Go 工具链
    extends: [base]
    tools: [g, golang, gopls]
```

加载顺序：`common.yaml` → `<goos>.yaml` → `profiles.yaml`，其后是用户目录（`<config>/tools/*.yaml`）。
**同名 tool 按字段合并、数组整体替换**（要追加用下面的 `+` 语法）；**同名 profile 整体替换**。
每条工具最终来自哪个文件记录在 `Origin` 里，`bs tool list --json` 可以看到。

## 2. Tool 字段

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `id` | ✔ | 唯一标识，`^[a-z0-9][a-z0-9._-]*$`；命令里用它选择工具 |
| `stack` | ✔ | 技术栈：`base`/`golang`/`python`/`nodejs`/`rust`/`cpp`/`java`/`shell`/`containers`/`cloud`/`infra`/`editor`/`agent` |
| `kind` | ✔ | 功能类型：`runtime`/`versionmgr`/`pkgmgr`/`cli`/`devdep`/`service`/`gui`/`agent` |
| `name` `description` `homepage` | | 展示用 |
| `tags` | | 自由标签，`--tag` 过滤（多个标签是"与"关系） |
| `depends_on` | | 依赖的工具 id；安装按拓扑序，循环依赖在校验期报错 |
| `when` | | 适用条件，不匹配则从本机可见清单里隐藏（`--all` 仍可看到） |
| `provides` | | 安装后的产物路径（可含 `~`），用于探测"装没装"。**可以是文件** |
| `path_dirs` | | 要加进 PATH 的目录（可含 `~`）。与 `provides` 分开的理由见 §4 |
| `verify` | ✔ | 怎么判断装好了、版本是多少（见 §3） |
| `install` | ✔ | 安装方式链，书写顺序即优先级（见 §5） |
| `update` | | 升级方式（**单个**方法）。不写则复用 `install` 链，由安装器按 op 换成对应的升级命令 |
| `uninstall` | | 卸载方式链。不写则复用 `install` 链（`brew install` ↔ `brew uninstall` 这样推导） |
| `disabled` | | `true` 表示整条不参与选择（用户覆盖时最常用的手段） |
| `manager_optional` | | `true` 允许 `runtime` 没有 `via: manager`（默认不允许，见 §6） |
| `+install` `+uninstall` `+tags` `+provides` `+path_dirs` | | 追加语法，**只在用户覆盖文件里有意义** |

### `when` 的条件

```yaml
when:
  os: [linux, darwin]          # 命中其中之一
  os_not: [windows]            # 排除
  arch: [amd64, arm64]
  distro: [ubuntu, debian]     # 大小写不敏感；来自 /etc/os-release
  distro_not: [alpine]
  has: [cargo]                 # 机器上存在该命令才适用（用 PATH 判断）
  has_not: [nvm]
```

空 `when` 表示"总是适用"。条件在工具层与方法层都可用，**筛选在运行时真做**，
不是加载期裁剪——否则 `--os darwin` 这类跨平台校验就没意义了。

## 3. `verify`：怎么判断装没装

```yaml
verify:
  command: go                          # 可执行文件名（用 LookPath 找）
  version_args: [version]               # 取版本的参数
  version_regex: 'go version go(?P<v>\d+\.\d+(?:\.\d+)?)'   # 从输出里取版本
  min_version: "1.22"                   # 低于它算 outdated
  exact_version: "1.24.1"               # 高或低都算 outdated
```

- `command` 与 `file` **互斥**，且必须有一个：
  - `command`：PATH 里能找到即 `installed`；给了 `version_regex` 才能判断 `outdated`。
  - `file`：文件存在即 `installed`（用于 nvm/rvm 这类 **shell 函数**型工具，
    PATH 里没有稳定可执行文件）。例：`verify: {file: ~/.nvm/nvm.sh}`。
- `version_regex` 必须带一个**命名捕获组** `(?P<v>...)`；取不到版本时状态是 `unknown`（不算失败）。
- 探测四态：`installed` / `outdated` / `missing` / `unknown`。
  `--refresh` 忽略缓存；`--dry-run` 既不写缓存也不留任何磁盘痕迹。

## 4. `provides` 与 `path_dirs`

```yaml
provides: ["~/.nvm/nvm.sh"]     # 产物（文件），用于探测
path_dirs: ["~/.local/bin"]     # PATH 目录
```

这两个字段分开是刻意的：`provides` 回答"东西装在哪、装没装"，
`path_dirs` 回答"PATH 要加哪个目录"。

早期版本只有 `provides`，`bs env path` 靠"路径有没有扩展名"猜它是文件还是目录——
结果 `~/.local/bin/rg`（没有扩展名）被整个当成目录写进 PATH。
让清单把意图写清楚，比在代码里猜可靠。

> 大多数工具装进 `~/.local/bin`，而它已经在配置 `tool.extra_path` 的默认值里，
> 所以通常不需要写 `path_dirs`；`~/go/bin`、`~/.cargo/bin` 这类才有必要。

## 5. `install` / `uninstall` / `update`：方式链

`install` 是一个**有序列表**，从上到下取第一个"可用"的方式。
可用 = 该方法未被 `enabled: false` 禁用 + `when` 匹配 + 对应 `via` 在当前环境里可用。

```yaml
install:
  - via: brew
    when: {os: [darwin]}
    params: {formula: ripgrep}
  - via: release
    when: {os: [linux, darwin]}
    params:
      repo: BurntSushi/ripgrep
      asset: "ripgrep-{{version}}-{{arch|archmap:amd64=x86_64,arm64=aarch64}}-{{os|osmap:linux=unknown-linux-musl,darwin=apple-darwin}}.tar.gz"
      strip_components: 1
      binary: rg
      dest: "~/.local/bin"
  - via: apt
    when: {os: [linux], distro: [ubuntu, debian]}
    params: {packages: [ripgrep]}
```

方法级字段：

| 字段 | 说明 |
| --- | --- |
| `via` | 方式名，必须是已注册的实现（见 §7） |
| `when` | 该方法专用条件（例如只在装了 bun 时才用 bun） |
| `enabled` | `false` 显式禁用该方法（保留写法但跳过，便于用户临时启用） |
| `params` | 该方式自己的参数（见 §7） |

关于"最新版本"：URL/文件名里的 `{{version}}` 在**执行安装时**才联网解析
（`bs tool plan` 与 `--dry-run` 只显示 `<latest>` 占位符，是离线、瞬间完成的预览）。
网络元数据查询有 15s 上限，机器还没配好网络时不会长时间卡住。

执行语义：
- 计划里每个工具只**选中一个**方式（`bs tool info <工具>` 会标出选中的是哪个）。
- 选中的方式**执行失败**时，是否尝试后面的方式由 `--fallback` / `--no-fallback` 决定
  （交互式默认开，非交互/CI/`--json` 默认关）；报告里会标 `fallback_from` 与失败原因。
- `--via <方式>` 强制某一种方式，**只约束显式点名的工具**；依赖仍按自己的清单解析。
  该工具没有这个方法时直接报错（退出码 2），不会静默改用别的方式。

### 追加语法

```yaml
# <config>/tools/ripgrep.yaml —— 我的覆盖层
- id: ripgrep
  +install:
    - via: cargo
      when: {has: [cargo]}
      params: {crate: ripgrep}
  +tags: [mine]
```

`+install` 追加到链尾（即优先级最低），`+tags`/`+provides`/`+path_dirs` 追加到列表末尾。
数组字段（`tags`、`install`、`path_dirs`…）默认是**整体替换**，所以想要"叠加"就必须用 `+` 语法。

## 6. 版本管理器与运行时（`kind: runtime` / `versionmgr`）

约定：**语言运行时由版本管理器安装，而不是直接下官方二进制**（ADR-0005）。
所以 `runtime` 条目必须有一条 `via: manager`（除非 `manager_optional: true`），
并且 `depends_on` 对应的版本管理器。

```yaml
- id: golang
  stack: golang
  kind: runtime
  depends_on: [g]
  verify: {command: go, version_args: [version], version_regex: 'go version go(?P<v>\d+\.\d+(?:\.\d+)?)', min_version: "1.22"}
  install:
    - via: manager
      params: {action: install, version: "{{versions.golang}}"}   # 管理器由配置 tool.managers 决定
```

- `params.manager` 留空时，由配置 `tool.managers.<stack>` 决定（默认 `golang=g`、`nodejs=fnm`、`python=uv`、`rust=rustup`）。
  这样**换管理器只需要改配置**，清单不用分叉。
- `params.version` 支持 `{{versions.<name>}}`，取自配置 `tool.versions`（如 `golang=1.24.1`、`rust=stable`）。
  引擎会在选用方法前先把这类"名字类"参数（`version`/`package`/`formula`/`cask`/`crate`/`module`/
  `id`/`python`/`version_source`/`source`/`repo`）里的 `{{versions.*}}` 渲染成真实值，再交给
  安装器——它们会被当作**值**塞进命令模板，不能留到后面才展开。
  反过来 `asset`/`url`/`sha256`/`dest`/`build` 这些引用 `{{version}}` 的参数必须等到版本号
  确定后由各 installer 渲染，别写成 `{{versions.*}}`。
- `tool.update` 与 `tool.uninstall` 是可选的显式覆盖：写了就用它（`update` 是单个方法，
  `uninstall` 是一条链），没写则复用 `install` 链并由安装器推算命令。
- 内置支持的管理器：`g`、`fnm`、`nvm`（`NeedsShell`，会先 `source ~/.nvm/nvm.sh`）、`uv`、`rustup`、`mise`、`pyenv`。
- 管理器动作：`install` / `use` / `upgrade` / `uninstall` / `list-remote`。
  `version: latest` 时会先调 `list-remote` 解析出最新版本号再安装。

## 7. `via` 与其 `params`

### 系统包管理器

`apt`（Debian/Ubuntu）、`dnf`/`yum`（Fedora/RHEL）、`pacman`（Arch）、`zypper`（SUSE）、`apk`（Alpine）、
`winget`/`scoop`/`choco`（Windows）。

```yaml
params: {packages: [git, curl]}
```

**护栏**：系统包管理器默认只允许装 `kind` 为 `devdep`/`service`/`cli` 的东西。
`runtime`/`versionmgr` 想走系统包管理器必须显式放行（`params.allow_system: true`
或工具级 `allow_system_for: [system]`），因为那会让"版本由谁管"变得含糊。
`brew`/`scoop` 属于用户态安装在 Home 目录，不受此限。

`needs_root` 显式声明是否需要提权（默认按包管理器判断）。

### `brew`

```yaml
params: {formula: ripgrep}      # 或 {cask: iterm2}、{tap: owner/tap}
```

### npm 系与 Python 系

`npm`、`pnpm`、`bun`、`pipx`、`uvpm`（`uv tool`）：

```yaml
params: {package: "@anthropic-ai/claude-code", global: true}
params: {package: ruff}                     # pipx/uvpm 不需要 global
```

### `cargo` / `goinstall`

```yaml
params: {crate: fd, features: [cli], git: "..."}      # cargo install
params: {module: golang.org/x/tools/gopls@latest}     # go install
```

### `release`：从 GitHub Release 取归档

```yaml
params:
  repo: astral-sh/uv                    # owner/name
  asset: "uv-{{arch|archmap:amd64=x86_64,arm64=aarch64}}-unknown-linux-gnu.tar.gz"
  version_source: ""                    # 空/`latest-release`=按最新 tag；可以是固定 tag、
                                        # 也可以是一个 URL（从该 URL 的文本里抓出版本号）
  dest: "~/.local/share/uv"
  strip_components: 1                   # 只在归档有外层目录时写！实测：uv/ripgrep/ruff 有，
                                        # lazygit/k9s/g 没有（根下直接是二进制），写错会把文件剥没
  extract: true                         # 整包解压到 dest（默认只挑出 binary 一个文件）
  binary: uv                            # extract=false 时挑哪个文件
  symlink: {from: ..., to: ...}         # 单个软链
  symlinks:                             # 多个软链（uv+uvx、go+gofmt）
    - {from: "~/.local/share/uv/uv", to: "~/.local/bin/uv"}
    - {from: "~/.local/share/uv/uvx", to: "~/.local/bin/uvx"}
  checksum_file: SHASUMS256.txt         # 指定校验和文件名（默认按候选链找）
  sha256: "..."                         # 或直接固化下载物的哈希
  checksum: sha256                      # `none` 显式放弃校验（逃生口，内置清单不用）
```

- **校验和是硬要求**：候选链为 `checksums.txt` → `sha256sum.txt` → `checksums.sha256` →
  `SHA256SUMS` → `<asset>.sha256`，都拿不到就**中止**（`params.sha256` 可以完全免联网校验）。
  因此清单里不要给"不发布校验和的仓库"写 `via: release`。
- `asset` 支持模板；`symlink`/`symlinks` 里的 `from`/`to` **同样支持模板**
  （gradle 要写 `~/.local/share/gradle/gradle-{{version}}/bin/gradle`）。
- 没写 `binary` 时：`repo` 模式取仓库名，直链模式取产物名去掉归档后缀。

#### 直链模式（`params.url`）

产物不在 GitHub Release 上（或 Release 里只有签名）时改用直链，**校验和同样是硬要求**：

```yaml
params:
  url: "https://dl.k8s.io/release/v{{version}}/bin/{{os}}/{{arch}}/kubectl"
  version_source: "https://dl.k8s.io/release/stable.txt"   # 固定版本、或能读出最新版本的 URL
  binary: kubectl
  dest: "~/.local/bin"
```

- 直链模式下**不要**写 `asset`（产物名从 URL 推出来）。
- **版本来源必须能确定**：`version_source` 写固定版本（`v9.9.9`）或一个 URL；
  或者给 `params.repo`，引擎去问 GitHub 的 `releases/latest`
  （helm：Release 里只有 `.asc` 签名，但 tag 仍然是最可靠的版本来源）。
- 校验和按 URL 的**兄弟文件**找：`<url>.sha256` → `<url>.sha256sum`；
  `checksum_file` 给绝对 URL 就用它，给名字（如 `sha256sum`）就当作 `<url>.<名字>`。
  文件内容两种都认：带文件名的列表（`<hash>  <名字>`）与只有裸哈希。
- 干跑（`bs tool plan`）**不联网**：`version_source` 是 URL 时也用 `<latest>` 占位，
  真正的版本解析发生在 Apply。

### `script`：执行官方安装脚本

```yaml
params:
  url: "https://.../install.sh"
  sha256: "..."             # 必填（除非 insecure: true）
  args: ["--version", "1.24.1"]
  shell: bash
```

`insecure: true` 是显式放弃校验，只用于脚本没有稳定哈希的场景；内置清单里**不使用**这种做法。

### `source`：源码/仓库构建

```yaml
params:
  repo: nvm-sh/nvm           # 直接克隆仓库
  ref: v0.40.8               # 固定 tag（比跟 HEAD 可控）
  dest: "~/.nvm"
  build: "make"              # 可选：构建命令
  install: "make install"     # 可选：安装命令
```

`params.dest` 与 `repo` 同时存在时是"把仓库直接克隆到 dest"（仓库即安装物，如 nvm）。

## 8. 模板变量

清单里的字符串字段支持 `{{...}}`（可用字段见 `Params.TemplateFields()`），
`bs tool manifest validate` 会把它们都预渲染一遍，**变量名写错立即报错**（不会静默变成空串）。

| 变量 | 值 |
| --- | --- |
| `{{version}}` `{{tag}}` | 解析出的版本号 / 原始 tag |
| `{{os}}` `{{arch}}` | `linux`/`darwin`/`windows`、`amd64`/`arm64` |
| `{{package}}` `{{crate}}` `{{module}}` `{{formula}}` `{{cask}}` `{{id}}` | 对应参数回填 |
| `{{manager}}` `{{python}}` `{{action}}` | 管理器名 / Python 版本 / 动作 |
| `{{versions.golang}}` | 配置 `tool.versions` 里的值 |

过滤器：`{{arch|archmap:amd64=x86_64,arm64=aarch64}}`、`{{os|osmap:darwin=macos}}`、
`{{version|trimv}}`、`{{version|default:latest}}`、`{{x|lower}}`、`{{x|upper}}`（可串联）。

## 9. 写清单的几条纪律

1. **优先交给已有的包管理器**：能 `brew`/`apt`/`npm`/`cargo` 装就不要下官方二进制。
2. **`via: release` 只在确实有校验和时使用**（直链模式也一样：`<url>.sha256`/`.sha256sum`），
   没有就走管理器；宁可版本旧一点。
3. **不要在用户看不到的地方用 sudo**：系统包管理器会被护栏拦住，需要时显式 `allow_system`。
4. **一条工具一条 id**，不要用 id 表达平台差异（用 `when`）。
5. **改完跑 `bs tool manifest validate`**（含 `--os` 交叉校验），再看 `bs tool plan`。
