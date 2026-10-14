# 配置参考

`bs` 的配置是单个 YAML 文件。本文档列出**全部**字段、默认值与语义。
字段定义见 `internal/config/types.go`，默认值见 `Defaults()`，校验规则见 `internal/config/validate.go`，
装载与合并见 `internal/config/load.go`。

## 1. 文件位置与优先级

查找顺序（后面的覆盖前面的）：

| 层 | Linux | macOS | Windows |
| --- | --- | --- | --- |
| 1. 内置默认值 | 编译进二进制（`config.Defaults()`） | 同左 | 同左 |
| 2. 系统级 | `/etc/bootstrap/config.yaml` | `/Library/Application Support/Bootstrap/config.yaml` | `%ProgramData%\Bootstrap\config.yaml` |
| 3. 用户级 | `$XDG_CONFIG_HOME/bootstrap/config.yaml`（默认 `~/.config/bootstrap/config.yaml`） | 同左（ADR-0001：与 Linux 统一） | `%AppData%\bootstrap\config.yaml` |
| 4. 项目级 | `$PWD/.bootstrap.yaml`（存在才生效） | 同左 | 同左 |
| 5. 环境变量 | 见下表（**固定映射表**，不是全量 `BS_*`） | 同左 | 同左 |
| 6. 命令行 flag | `--repo-root`、`--log-level`、`--no-color` 等在装载后覆盖 | 同左 | 同左 |

- 第 2–4 层用 viper 的 `MergeInConfig` 逐层合并，**不存在的文件直接跳过**（没有系统级配置也不会报错）。
- 目录可用 `BS_CONFIG_HOME` / `BS_STATE_HOME` / `BS_CACHE_HOME` 整体重定向
  （用于测试与多环境隔离；`bs config path` 会打印实际路径与来源）。
- `--config <文件>` 跳过上面第 2–4 层，只读指定文件（文件不存在直接报错）。
- `bs config show` 看合并结果；`bs config validate` 校验；`bs config init` 生成模板（已存在不覆盖）。

### 环境变量

下表是**映射到配置字段**的变量（`internal/config/load.go` 的 `envBindings`）；
之所以不用 viper 的 `AutomaticEnv`，是因为它在 `Unmarshal` 路径上行为不可靠，
显式绑定让"支持哪些变量"一眼可查、可测试。

| 变量 | 对应字段 | 备注 |
| --- | --- | --- |
| `BS_CONFIG_HOME` / `BS_STATE_HOME` / `BS_CACHE_HOME` | 目录重定向（不写入配置） | |
| `BS_LOG_LEVEL` | `log.level` | |
| `BS_LOG_COLOR` | `log.color` | |
| `NO_COLOR` | `log.color: never` | **只要变量被设置**（哪怕是空值）就生效，且优先于配置文件 |
| `BS_REPO_ROOT` | `repo.root` | |
| `BS_REPO_DEFAULT_HOST` | `repo.default_host` | |
| `BS_REPO_CLONE_PROTOCOL` | `repo.clone.protocol` | |
| `BS_REPO_PULL_STRATEGY` | `repo.pull.strategy` | |
| `BS_PROXY_PROFILE` / `BS_PROXY_DEFAULT_PROFILE` | `proxy.default_profile` | 两个名字等价 |
| `BS_PROXY_AUTO_APPLY` | `proxy.auto_apply` | |
| `BS_TOOL_PROFILE` | `tool.default_profile` | |
| `BS_MANIFEST_BUILTIN` | `tool.manifest.builtin` | |
| `BS_SHELL_SNIPPETS_DIR` | `shell.snippets_dir` | |

此外还有几个**不落配置**的变量（都是"这一次运行"的临时覆盖，刻意不给配置文件对应项）：

| 变量 | 作用 | 备注 |
| --- | --- | --- |
| `BS_TIMEOUT` / `BS_TIMEOUT_COMMAND` / `BS_TIMEOUT_DOWNLOAD` | 覆盖超时 | 见 §6 |
| `BS_REPO` | 覆盖自更新/发布用的仓库（`owner/repo`） | 默认 `kingfs/Bootstrap`；fork 自测用 |
| `BS_GITHUB_API` | 覆盖 GitHub API 前缀 | 默认 `https://api.github.com`；镜像/加速站 |
| `BS_GITHUB_ASSET_BASE` | 覆盖产物与校验和的下载前缀 | 默认 `https://github.com`；镜像/加速站 |

`BS_GITHUB_*` 作用于**所有**从 GitHub Release 下载的路径（按清单装 release 工具、`bs self-update`）。
注意它们同时决定产物与 `checksums.txt` 的来源，因此镜像必须完全可信（见 design §16）。

## 2. 顶层字段

```yaml
version: 1        # 配置格式版本，目前只能为 1（0 会被补成 1）
log: {...}
repo: {...}
proxy: {...}
tool: {...}
shell: {...}
ui: {...}
timeouts: {...}
```

`bs config validate` 的校验面：`version`、`log.level`、`log.color`、`repo.layout`（必须含
`{{.Repo}}` 且能渲染）、`repo.clone.protocol`、`repo.pull.strategy`、`repo.clone.depth >= 0`、
`repo.sync.jobs >= 1`、`repo.hosts` 无空项、`proxy.default_profile` 必须存在于 `proxy.profiles`、
每个 profile 至少一个非空代理地址且 URL 合法（`http`/`https`/`socks5(h)`/`socks4`）、
`tool.allow_system_for` 与 `tool.managers` 的键值非空、`ui.progress`、
`timeouts.*` 必须是带单位的正数时长（`30s`/`10m`/`1h`；写 `300` 会报错，因为不同环节的裸数字含义不同）。
校验失败会**一次报出全部问题**（`errors.Join`），不会改一个跑一次。

### `log`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `level` | `debug\|info\|warn\|error` | `info` | 日志级别；`-v` 临时提升为 debug、`-vv` 为 trace |
| `color` | `auto\|always\|never` | `auto` | 彩色输出；`NO_COLOR` 存在时强制 `never` |

### `repo`

仓库统一放在 `<root>/<layout>`。`root` 在配置里默认是**空**，真正的默认值在运行时解析
（`bs repo root` 会告诉你用了哪个，以及来源）：

- Linux/macOS：`~/src`（若该目录已存在，来源标注为 auto-detected）；
- Windows：`D:\src`（存在时优先）→ 否则 `%USERPROFILE%\src`。

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `root` | 路径 | 空 → 运行时 `~/src` | 支持 `~` 与环境变量（装载时展开） |
| `layout` | 模板 | `{{.Host}}/{{.Owner}}/{{.Repo}}` | 可用字段：`.Host` `.Owner` `.Repo` `.Ref` `.SubPath`；必须含 `{{.Repo}}`，且不允许 `..` 越界 |
| `default_host` | 字符串 | `github.com` | 输入 `owner/repo` 时补全的 host |
| `hosts` | 字符串数组 | github/gitlab/gitee/codeberg/bitbucket | 已知 host 列表（用于识别输入形式） |
| `clone.protocol` | `https\|ssh` | `https` | 克隆协议；输入本身是 `git@host:...` 时保留 ssh |
| `clone.depth` | 整数 | `0` | `0` 表示完整克隆 |
| `clone.extra_args` | 字符串数组 | `[]` | 追加到 `git clone` 的参数 |
| `pull.strategy` | `ff-only\|rebase\|merge` | `ff-only` | 对应 `git pull --ff-only/--rebase/--merge` |
| `pull.autostash` | 布尔 | `false` | pull 前自动 `stash` 并在结束后 `pop` |
| `pull.prune` | 布尔 | `true` | fetch 时加 `--prune` |
| `sync.jobs` | 整数 | `4` | `bs repo sync` 的并发度 |

> 脏工作区（有未提交改动）默认**跳过** pull，只提示；需要自动处理就开 `pull.autostash`，
> 或临时用 `bs repo get --autostash` / `bs repo sync --autostash`。

### `proxy`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `default_profile` | 字符串 | 空（不使用代理） | 未显式 `use` 时采用的 profile；必须存在于 `profiles` |
| `auto_apply` | 布尔 | `true` | `bs` 自己发起的网络操作是否自动带代理 |
| `override_existing` | 布尔 | `false` | 是否覆盖父进程已有的 `*_PROXY`。`false`（默认）表示**尊重当前 shell 的设置**：同一组变量（http/https/all/no，大小写算一组）用 shell 里那份值，profile 里其余的照常注入。注意"尊重"不等于"这一组就不注入了"——那样 bs 自己的 HTTP 请求（`httpx` 把注入的 env 当权威）会变成直连，而子进程仍走 shell 的代理，同一台机器两种行为 |
| `profiles.<名字>.http_proxy` | URL | — | 支持 `http`/`https`/`socks5(h)`/`socks4` |
| `profiles.<名字>.https_proxy` | URL | — | 通常与 `http_proxy` 相同 |
| `profiles.<名字>.all_proxy` | URL | — | 可选 |
| `profiles.<名字>.no_proxy` | 字符串 | — | 逗号分隔；直连的域名/IP 段 |

profile 至少要有一个非空代理地址，否则配置校验失败。

代理的"当前状态"由 `bs proxy on/use/off` 记在 **state 文件**（不是配置文件）：
`mode: auto|on|off` + `active: <名字>`。子进程改不了父 shell 的环境变量，所以
"当前 shell 立即生效"靠 `tools/snippets/*/10-proxy.*` 里的同名 shell 函数包装实现（ADR-0007）。

### `tool`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `manifest.builtin` | 布尔 | `true` | 是否加载内置清单（随二进制发布，`go:embed`） |
| `manifest.user_dirs` | 路径数组 | 空 → 运行时等价 `[<config>/tools]` | 用户清单目录，按文件名排序，后覆盖前（另见 `BS_MANIFEST_BUILTIN`） |
| `default_profile` | 字符串 | `base` | 不给工具名、也不给选择条件时 `bs tool install` 用的套装 |
| `default_via` | 字符串数组 | `[]`（空） | 只取**第一个**元素作为强制安装方式；留空即"按清单书写顺序" |
| `allow_system_for` | `kind` 数组 | `[devdep, service, cli]` | 允许动用系统包管理器的工具类型（`apt`/`dnf`/`pacman`/`winget`…） |
| `managers.<技术栈>` | 字符串 | `golang: g`、`nodejs: fnm`、`python: uv`、`rust: rustup`、`universal: mise` | 各栈的版本管理器；`via: manager` 缺 `params.manager` 时用它落地 |
| `versions.<名字>` | 字符串 | `golang: latest`、`nodejs: latest`、`python: "3.12"`、`rust: stable` | 清单模板 `{{versions.<名字>}}` 的取值；想可复现就写死具体版本 |
| `extra_path` | 路径数组 | `[~/.local/bin, ~/go/bin]` | 额外前置到 PATH 的目录（`bs env path`）；装载时展开 `~` |

**这里没有 `tool.fallback`**：失败后是否自动换用后面的安装方式由**运行时**决定（ADR-0013）——
`--fallback` / `--no-fallback` 显式指定优先，否则在交互式终端默认开、在非交互/CI/`--json`
默认关。详见 [design.md](./design.md) §7.2。

`extra_path` 之外，`bs env path` 还会收进清单里各工具声明的 `path_dirs`；
而探测（`LookPath`）在配置之外还会看 `~/bin` 与 `~/.g/versions/current/bin`。

### `shell`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `snippets_dir` | 路径 | `<config>/shell` | 用户片段目录，按 `posix/`、`fish/`、`pwsh/` 分；同名**整体覆盖**内置片段 |
| `rc_files.<shell>` | 路径 | 自动探测 | 指定某个 shell 的 rc 文件（如 `zsh: ~/.zshrc`），优先于自动探测 |
| `auto_path` | 布尔 | `true` | 关掉则 `bs shell init` 不渲染 `20-path.*` 片段（自己管 PATH 的场景） |
| `auto_proxy` | 布尔 | `false` | 打开则每个新 shell 自动应用 bs 当前活跃的 proxy profile |

`auto_path` / `auto_proxy` 只影响 `bs shell init` 渲染的内容，**不写进 rc 文件**——受管块永远只有
`eval "$(bs shell init <shell>)"` 一行，所以改完配置立刻生效（重开 shell 或重新 `eval` 一次）。
`auto_proxy` 默认关是刻意的：它会让 `bs proxy off` 之后把新 shell 里**已有的**代理变量也清掉。

rc 文件自动探测规则：`bash` → `~/.bashrc`（macOS 优先 `~/.bash_profile`）；
`zsh` → `$ZDOTDIR/.zshrc` 或 `~/.zshrc`；`fish` → `~/.config/fish/conf.d/bootstrap.fish`；
`pwsh` → `~/.config/powershell/Microsoft.PowerShell_profile.ps1`
（Windows：`~/Documents/PowerShell/Microsoft.PowerShell_profile.ps1`）。
`cmd.exe` 没有 rc 文件，不支持受管块。

### `ui`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `confirm` | 布尔 | `true` | 置 `false` 时，**交互会话里**不再问"继续？"（非交互环境仍需 `--yes`，否则退出码 5） |
| `progress` | `auto\|plain\|off` | `auto` | 安装过程中的逐步骤进度：`auto` 只在交互终端打；`plain` 总是打；`off` 从不打 |

### `timeouts`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `global` | 时长 | 空（不限） | **整条命令**的总时限：到点后连子进程一起停掉（可用 `--timeout` 临时覆盖） |
| `command` | 时长 | 空（10m） | 单个外部命令（git / go / apt / cargo…）的时限 |
| `probe` | 时长 | 空（5s） | 单次版本探测的时限 |
| `lookup` | 时长 | 空（60s） | 查"最新 tag / 校验和文件"这类元数据请求的时限 |
| `download` | 时长 | 空（15m） | 单个归档下载的时限 |

```yaml
timeouts:
  command: 30m      # 在慢机器上编译/装依赖超过 10 分钟时调大
  download: 30m     # 限速代理下下载大归档
  # global: 1h      # 整条命令的上限；不建议常设，容易把"慢"误判成"坏"
```

留空＝用该环节的内置默认值。配置里的 `global` 会被命令行 `--timeout` 覆盖，
其余四项也可以用环境变量临时改：`BS_TIMEOUT`（总时限）、`BS_TIMEOUT_COMMAND`、
`BS_TIMEOUT_PROBE`、`BS_TIMEOUT_LOOKUP`、`BS_TIMEOUT_DOWNLOAD`。

> 超时不是"某一步失败"：bs 会明说"超过总时限（…）已中止"，并且**不会回滚**已经完成的步骤
>（重跑即可，安装类操作是幂等的）。因此 `--timeout` 适合放在脚本/CI 里兜底，而不是日常随手设。

## 3. 完整示例

```yaml
version: 1

log:
  level: info
  color: auto

repo:
  layout: "{{.Host}}/{{.Owner}}/{{.Repo}}"
  default_host: github.com
  clone:
    protocol: https
    depth: 0
  pull:
    strategy: ff-only
    autostash: false
    prune: true
  sync:
    jobs: 4

proxy:
  default_profile: local
  auto_apply: true
  override_existing: false
  profiles:
    local:
      http_proxy: http://127.0.0.1:7890
      https_proxy: http://127.0.0.1:7890
      no_proxy: 127.0.0.1,localhost,10.2.0.0/16,192.168.0.0/16,.chaitin.net,.chaitin.cn,.baizhi.cloud
    chaitin:
      http_proxy: http://proxy.in.chaitin.net:8123
      https_proxy: http://proxy.in.chaitin.net:8123
      no_proxy: 127.0.0.1,localhost,10.2.0.0/16,192.168.0.0/16,.chaitin.net,.chaitin.cn,.baizhi.cloud

tool:
  default_profile: base
  managers:
    golang: g
    nodejs: fnm
    python: uv
    rust: rustup
    universal: mise
  versions:
    golang: "1.24.1"     # 想可复现就写死；latest 需要联网解析
    nodejs: lts
    python: "3.12"
    rust: stable
  extra_path:
    - ~/.local/bin
    - ~/go/bin

shell:
  auto_path: true
  auto_proxy: false

ui:
  confirm: true
  progress: auto

timeouts:
  command: 30m
  download: 30m
```

## 4. 用命令行改配置

不必手改 YAML——`bs config` 有一组"只动该动的地方"的子命令（详见设计文档 §5.3）：

```sh
bs config keys                  # 列出全部可用键路径（get/set 的拼写参考）
bs config get repo.root -v      # 读**生效值**，并说明它来自哪个文件
bs config get repo.hosts        # 列表/映射按 YAML 输出
bs config set log.level debug   # 只改这一行：注释、键顺序、其它内容都保留
bs config set repo.hosts '["github.com", "gitlab.com"]'   # 列表写 YAML/JSON
bs config set ui.confirm --unset    # 删掉该键 → 回到"由默认值/上层配置决定"
bs config edit                  # 用 $EDITOR 打开（退出后立刻校验；需要终端）
```

几条定死的规矩：

- **只写用户级配置文件**（`<config>/config.yaml`）。系统级要 root、项目级属于某个仓库、
  环境变量是临时覆盖——在那些地方"帮忙"写值，只会制造下次找不到原因的状态。
- **类型由内置默认配置推断**：`ui.confirm` 只收 true/false、`repo.clone.depth` 只收整数，
  写错在落盘**之前**就报错。
- **写前备份、写后校验**：不合法会把原内容放回去（错误里写明"已还原"），
  绝不会出现"改完 `bs` 起不来"。备份路径会打印出来（`<文件>.bs-backup.<时间戳>`）。
- `--dry-run` 仍然零写入，连"顺手生成一份默认配置"都不做。

## 5. 常见问题

**改了配置但 `bs` 没生效？**
`bs config show` 看合并结果，`bs config path` 看实际读了哪些文件（项目级 `.bootstrap.yaml`
经常是"意外覆盖"的来源）；`bs config get <键> -v` 能直接指出某个值来自哪个文件。

**缓存目录里有什么？能删吗？**
`<cache>/downloads/` 是下载缓存：归档/安装脚本落在这里，断线时留着 `.<名字>.<指纹>.part`
供下次续传，校验和一致的已下载文件会被直接复用。**可以随时整个删掉**——最坏结果只是下次
重新下载；30 天没用过的条目也会在下次下载前自动清理。探测结果缓存在同目录的 `probe.json`，
带 10 分钟 TTL，删掉同样无害。

**配置写错了会不会导致 shell 起不来？**
不会。`bs shell init` / `bs env path` / `bs proxy env` 在配置解析失败时会退化为内置默认值，
并只在 stderr 打印一条警告——这几个命令每次开 shell 都会执行，必须保证不阻断登录。
其它命令则会直接报错并给出退出码 1（配置问题不静默）。

**为什么 `bs proxy off` 之后状态还是"已关闭"而不是"未设置"？**
因为"关闭"是一个明确的选择，需要被记住；否则下次读取又会回退到 `default_profile`
或环境变量。要恢复自动行为就 `bs proxy use <名字>`。

**`tool.versions` 与 `tool.managers` 是什么关系？**
`managers` 决定"谁来装"，`versions` 决定"装哪个版本"。清单里写
`params: {action: install, version: "{{versions.golang}}"}`，两者配合起来就能做到
"把 nodejs 从 fnm 换成 nvm、版本从 latest 换成 20.11.1"都只改配置、不动清单。

> 说明：`{{versions.*}}` 由引擎在选用方法前展开（"名字类"参数，见
> [tool-schema.md](./tool-schema.md) §6），因此 `bs tool plan`/`--dry-run` 里看到的是
> 真实版本号（例如 `g install 1.24.1`），而不是模板字面量。
