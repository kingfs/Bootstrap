# 配置参考

`bs` 的配置是单个 YAML 文件。本文档列出**全部**字段、默认值与语义。
字段定义见 `internal/config/types.go`，校验规则见 `internal/config/validate.go`。

## 1. 文件位置与优先级

查找顺序（后面的覆盖前面的）：

| 层 | Linux / macOS | Windows |
| --- | --- | --- |
| 1. 内置默认值 | 编译进二进制 | 同左 |
| 2. 系统级 | `/etc/bootstrap/config.yaml`（macOS：`/Library/Application Support/Bootstrap/config.yaml`） | `%ProgramData%\Bootstrap\config.yaml` |
| 3. 用户级 | `$XDG_CONFIG_HOME/bootstrap/config.yaml`（默认 `~/.config/bootstrap/config.yaml`） | `%AppData%\bootstrap\config.yaml` |
| 4. 项目级 | `$PWD/.bootstrap.yaml`（存在才生效） | 同左 |
| 5. 环境变量 | 见下表 | 同左 |
| 6. 命令行 flag | `--repo-root` 等 | 同左 |

- 目录可用 `BS_CONFIG_HOME` / `BS_STATE_HOME` / `BS_CACHE_HOME` 整体重定向
  （用于测试和多环境隔离；`bs config path` 可查看实际路径）。
- `--config <文件>` 会**跳过**上面第 2–4 层，只读指定文件。
- 查看合并结果：`bs config show`；校验：`bs config validate`；生成模板：`bs config init`。

### 环境变量

| 变量 | 对应字段 |
| --- | --- |
| `BS_CONFIG_HOME` / `BS_STATE_HOME` / `BS_CACHE_HOME` | 目录重定向（不写入配置） |
| `BS_LOG_LEVEL` / `BS_LOG_COLOR` | `log.level` / `log.color` |
| `NO_COLOR` | 存在即等价于 `log.color: never` |
| `BS_REPO_ROOT` | `repo.root` |
| `BS_REPO_DEFAULT_HOST` | `repo.default_host` |
| `BS_REPO_CLONE_PROTOCOL` | `repo.clone.protocol` |
| `BS_REPO_PULL_STRATEGY` | `repo.pull.strategy` |
| `BS_PROXY_PROFILE` / `BS_PROXY_DEFAULT_PROFILE` | `proxy.default_profile` |
| `BS_PROXY_AUTO_APPLY` | `proxy.auto_apply` |
| `BS_TOOL_PROFILE` | `tool.default_profile` |
| `BS_SHELL_SNIPPETS_DIR` | `shell.snippets_dir` |

## 2. 顶层字段

```yaml
version: 1        # 配置格式版本，目前只能为 1
log: {...}
repo: {...}
proxy: {...}
tool: {...}
shell: {...}
ui: {...}
```

### `log`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `level` | `debug\|info\|warn\|error` | `info` | 日志级别；`-v` 会临时提升为 debug |
| `color` | `auto\|always\|never` | `auto` | 彩色输出；`NO_COLOR` 存在时强制 `never` |

### `repo`

仓库统一放在 `<root>/<layout>`，默认即维护者的目录习惯。

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `root` | 路径 | Linux/macOS `~/src`，Windows `%USERPROFILE%\src`（若 `D:\src` 存在则优先） | 仓库根目录；支持 `~` 与环境变量 |
| `layout` | 模板 | `{{.Host}}/{{.Owner}}/{{.Repo}}` | 可用字段：`.Host` `.Owner` `.Repo` `.Ref` `.SubPath`；必须含 `{{.Repo}}`，且不允许 `..` 越界 |
| `default_host` | 字符串 | `github.com` | 输入 `owner/repo` 时补全的 host |
| `hosts` | 字符串数组 | github/gitlab/gitee/codeberg/bitbucket | 已知 host 列表（用于识别输入形式） |
| `clone.protocol` | `https\|ssh` | `https` | 克隆协议；输入本身是 `git@host:...` 时保留 ssh |
| `clone.depth` | 整数 | `0` | `0` 表示完整克隆 |
| `clone.extra_args` | 字符串数组 | `[]` | 追加到 `git clone` 的参数 |
| `pull.strategy` | `ff-only\|rebase\|merge` | `ff-only` | 对应 `git pull --ff-only/--rebase/--merge` |
| `pull.autostash` | 布尔 | `false` | pull 前自动 `stash`（含未跟踪文件）并在结束后 `pop` |
| `pull.prune` | 布尔 | `true` | fetch 时加 `--prune` |
| `sync.jobs` | 整数 | `4` | `bs repo sync` 的并发度 |

> 脏工作区（有未提交改动）默认**跳过** pull，只提示；需要自动处理就开 `pull.autostash`。

### `proxy`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `default_profile` | 字符串 | 空（不使用代理） | 未显式 `use` 时采用的 profile；必须存在于 `profiles` |
| `auto_apply` | 布尔 | `true` | `bs` 自己发起的网络操作是否自动带代理 |
| `override_existing` | 布尔 | `false` | 是否覆盖父进程已有的 `*_PROXY` 变量；`false` 表示尊重你当前 shell 的设置 |
| `profiles.<名字>.http_proxy` | URL | — | 支持 `http`/`https`/`socks5(h)`/`socks4` |
| `profiles.<名字>.https_proxy` | URL | — | 通常与 `http_proxy` 相同 |
| `profiles.<名字>.all_proxy` | URL | — | 可选 |
| `profiles.<名字>.no_proxy` | 字符串 | — | 逗号分隔；直连的域名/IP 段 |

profile 至少要有一个非空代理地址，否则配置校验失败。

代理的"当前状态"由 `bs proxy use/off` 记录在 **state 文件**（不是配置文件）：
`mode: on|off|auto` + `active: <名字>`。`off` 是显式关闭，此后不再回退到环境变量。

### `tool`（M2 起生效，当前仅装载与校验）

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `manifest.builtin` | 布尔 | `true` | 是否加载内置清单（随二进制发布） |
| `manifest.user_dirs` | 路径数组 | `[<config>/tools]` | 用户清单目录，同名工具覆盖内置 |
| `default_profile` | 字符串 | `base` | `bs tool install` 默认安装的套装 |
| `default_via` | 字符串数组 | 见设计文档 §7 | 安装方式偏好顺序（如 `manager` → `script` → `system` → `source`） |
| `allow_system_for` | `kind` 数组 | `devdep, service, cli` | 允许动用系统包管理器的工具类型 |
| `managers.<技术栈>` | 字符串 | `golang: g`、`nodejs: fnm`、`python: uv`、`rust: rustup`、`universal: mise` | 各栈使用的版本管理器 |
| `versions.<工具 id>` | 字符串 | `{}` | 版本固定（如 `go: "1.24.1"`） |
| `extra_path` | 路径数组 | `[~/.local/bin, ~/go/bin]` | 需要前置到 PATH 的目录（`bs env path` 使用） |

### `shell`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `snippets_dir` | 路径 | `<config>/shell` | 用户片段目录，按 `posix/`、`fish/`、`pwsh/` 分；同名覆盖内置片段 |
| `rc_files.<shell>` | 路径 | 自动探测 | 指定某个 shell 的 rc 文件（如 `zsh: ~/.zshrc`） |
| `auto_path` | 布尔 | `true` | 受管块是否装配 PATH |
| `auto_proxy` | 布尔 | `false` | 受管块是否在开 shell 时自动应用当前代理 profile |

rc 文件自动探测规则：`bash` → `~/.bashrc`（macOS 优先 `~/.bash_profile`）；
`zsh` → `$ZDOTDIR/.zshrc` 或 `~/.zshrc`；`fish` → `~/.config/fish/conf.d/bootstrap.fish`；
`pwsh` → `~/.config/powershell/Microsoft.PowerShell_profile.ps1`
（Windows：`~/Documents/PowerShell/Microsoft.PowerShell_profile.ps1`）。
`cmd.exe` 没有 rc 文件，不支持受管块。

### `ui`

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `confirm` | 布尔 | `true` | 破坏性操作是否要确认（`--yes` 等价于关闭） |
| `progress` | `auto\|plain\|off` | `auto` | 进度显示方式；非 TTY 自动退化为 `plain` |

## 3. 完整示例

```yaml
version: 1

log:
  level: info
  color: auto

repo:
  root: ~/src
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
      no_proxy: 127.0.0.1,localhost,10.2.0.0/16,192.168.0.0/16,chaitin.net,chaitin.cn,.chaitin.net,.chaitin.cn,.baizhi.cloud

tool:
  default_profile: base
  managers:
    golang: g
    nodejs: fnm
    python: uv
    rust: rustup
    universal: mise
  extra_path:
    - ~/.local/bin
    - ~/go/bin

shell:
  auto_path: true
  auto_proxy: false

ui:
  confirm: true
  progress: auto
```

## 4. 常见问题

**改了配置但 `bs` 没生效？**
`bs config show` 看合并结果，`bs config path` 看实际读的是哪个文件（项目级 `.bootstrap.yaml`
经常是"意外覆盖"的来源）。

**配置写错了会不会导致 shell 起不来？**
不会。`bs shell init` / `bs env path` / `bs proxy env` 在配置解析失败时会退化为内置默认值，
并只在 stderr 打印一条警告——这几个命令每次开 shell 都会执行，必须保证不阻断登录。

**为什么 `bs proxy off` 之后状态还是"已关闭"而不是"未设置"？**
因为"关闭"是一个明确的选择，需要被记住；否则下次读取又会回退到 `default_profile`
或环境变量。要恢复自动行为就 `bs proxy use <名字>`。
