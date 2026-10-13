# Bootstrap (`bs`)

个人开发环境重建工具：换一台新机器后，用几条命令把仓库、代理、shell 环境恢复成熟悉的样子。

- **一个二进制，零运行时依赖**（Go 静态编译，`linux`/`macOS`/`Windows` × `amd64`/`arm64`）
- **不改你的 rc 文件内容**：只在 rc 里放一个可精确移除的"受管块"
- **先看后做**：所有会改动文件系统或外部状态的命令都支持 `--dry-run`
- **可脚本化**：`--json` 输出 + 明确区分的退出码

> 当前进度：M1（仓库拉取、代理、shell 集成、体检）已完成。工具清单/安装引擎（`bs tool`）在 M2。

## 安装

```sh
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/kingfs/Bootstrap/main/scripts/install.sh | sh

# Windows (PowerShell)
irm https://raw.githubusercontent.com/kingfs/Bootstrap/main/scripts/install.ps1 | iex
```

安装脚本会从 GitHub Release 下载对应平台的归档，用 `checksums.txt` 校验 sha256 后再安装。
也可以直接下载归档，或从源码构建：

```sh
git clone https://github.com/kingfs/Bootstrap && cd Bootstrap
make build          # 产物在 bin/bs
make cross          # 交叉编译 6 个平台组合到 dist/
```

## 快速开始

```sh
bs config init                  # 生成默认配置（含本地/公司代理 profile 示例）
bs shell install                # 在 rc 里装受管块：代理立即生效 + PATH 装配
bs doctor                       # 看看环境还缺什么

bs proxy use local              # 启用代理（当前 shell 立即生效）
bs repo get kingfs/Bootstrap    # 克隆或更新到 ~/src/github.com/kingfs/Bootstrap
```

## 命令一览

### 仓库（`bs repo`）

把仓库统一放到 `<repo.root>/<host>/<owner>/<repo>`（默认 `~/src`，Windows 为 `%USERPROFILE%\src`）。

| 命令 | 说明 |
| --- | --- |
| `bs repo get <repo>...` | 克隆或更新；已存在则 `git pull` |
| `bs repo list` | 列出根目录下的仓库（分支、来源、是否有未提交改动） |
| `bs repo sync [-j N]` | 并发更新所有仓库；脏工作区默认跳过 |
| `bs repo root` | 打印仓库根目录及其来源 |

`<repo>` 支持任意常见写法，都会被归一化：

```
https://github.com/kingfs/Bootstrap          → ~/src/github.com/kingfs/Bootstrap
git@github.com:kingfs/Bootstrap.git          → 同上（保留 ssh 协议）
kingfs/Bootstrap                             → 同上（默认 host 来自配置）
https://github.com/kingfs/Bootstrap/tree/dev/internal/foo
                                             → 识别出 ref=dev、子路径 internal/foo（目录仍按仓库根算）
```

常用参数：`--protocol https|ssh`、`--depth N`、`--ref <分支>`、`--autostash`、`--dry-run`。

### 代理（`bs proxy`）

| 命令 | 说明 |
| --- | --- |
| `bs proxy list` | 列出所有 profile（`*` 标记当前生效项） |
| `bs proxy status` | 当前代理来源、地址（凭据脱敏）、shell 里已有的代理变量 |
| `bs proxy use <name>` / `bs proxy on <name>` | 激活 profile，并让当前 shell 立即生效（需装受管块） |
| `bs proxy off` | 关闭代理（状态被记住，不再回退到环境变量） |
| `bs proxy env [--apply] [--profile P] [--shell S]` | 输出 shell 语句供 `eval` |
| `bs proxy exec -- <cmd>` | 在带代理的子进程里执行命令（不需要 rc 集成） |
| `bs proxy test [url]` | 分别测试直连与走代理的连通性 |
| `bs proxy add <name> --http ... [--write]` | 新增 profile（默认只打印 YAML 片段） |

代理 profile 定义在配置文件里，`bs` 自己发起的网络操作会自动带上它：

```yaml
proxy:
  default_profile: local          # 可选：不设置则默认不使用代理
  auto_apply: true                # bs 发起的下载/克隆自动走代理
  override_existing: false        # 不覆盖你已手工设置的代理变量
  profiles:
    local:
      http_proxy: http://127.0.0.1:7890
      https_proxy: http://127.0.0.1:7890
      no_proxy: 127.0.0.1,localhost,10.2.0.0/16,.chaitin.net
    chaitin:
      http_proxy: http://proxy.in.chaitin.net:8123
      https_proxy: http://proxy.in.chaitin.net:8123
      no_proxy: 127.0.0.1,localhost,.chaitin.net,.chaitin.cn
```

> 子进程无法修改父 shell 的环境变量，所以"当前 shell 立即生效"依赖 `bs shell install`
> 装下的同名包装函数。不想动 rc 就用 `bs proxy exec -- <cmd>` 或 `bs shell`。

### shell（`bs shell`）

| 命令 | 说明 |
| --- | --- |
| `bs shell install [--shell S] [--rc FILE]` | 幂等写入受管块（写前备份，`--dry-run` 可预览） |
| `bs shell uninstall` | 精确移除受管块，块外内容字节级不变 |
| `bs shell status` | 受管块是否安装、是否与当前版本一致 |
| `bs shell init [shell]` | 输出受管块内容（供 rc 里 `eval`） |
| `bs shell snippet list\|path\|new` | 查看/新增 shell 片段（用户片段可覆盖内置） |
| `bs shell [--profile P]` | 打开已装配环境的子 shell，完全不碰 rc |

受管块长这样，升级 `bs` 即自动获得新片段：

```sh
# >>> bootstrap (managed by 'bs shell install') >>>
eval "$(bs shell init bash)"
# <<< bootstrap <<<
```

### 环境（`bs env`）

| 命令 | 说明 |
| --- | --- |
| `bs env show` | 将加入 PATH 的目录及来源、当前代理状态 |
| `bs env path` | 输出 PATH 装配语句（供 `eval`，幂等） |
| `bs env export` | 输出 PATH + 代理的完整语句 |

### 配置与体检

| 命令 | 说明 |
| --- | --- |
| `bs config path` | 各配置/状态目录的实际路径 |
| `bs config show` | 合并后的生效配置 |
| `bs config validate` | 校验配置（一次性列出全部问题） |
| `bs config init [--force]` | 生成默认配置（已存在不覆盖） |
| `bs doctor` | 只读体检：平台、配置、目录权限、git/curl、包管理器、代理、PATH、仓库根、受管块 |
| `bs version` | 版本信息（`--json` 可脚本消费） |

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 一般失败 |
| 2 | 用法错误（未知命令/参数非法） |
| 3 | 部分成功（批量操作中有失败项） |
| 4 | 前置条件不满足（配置缺失/非法、体检失败） |
| 5 | 需要交互但当前非交互 |

## 配置文件

默认位置（可用 `BS_CONFIG_HOME` 重定向，便于测试与多环境隔离）：

| 平台 | 路径 |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/bootstrap`（默认 `~/.config/bootstrap`） |
| macOS | `~/.config/bootstrap` |
| Windows | `%AppData%\bootstrap` |

优先级（后者覆盖前者）：

```
内置默认值 < 系统级 < 用户级 < 项目级(.bootstrap.yaml) < BS_* 环境变量 < 命令行 flag
```

常用环境变量：`BS_CONFIG_HOME`、`BS_STATE_HOME`、`BS_CACHE_HOME`、`BS_REPO_ROOT`、
`BS_PROXY_PROFILE`、`BS_LOG_LEVEL`、`NO_COLOR`。完整字段见
[docs/configuration.md](docs/configuration.md)。

## 开发

```sh
make build        # 构建 bin/bs
make test         # 单元测试
make race         # 单元测试（-race）
make smoke        # 无副作用端到端冒烟测试（沙箱目录，不碰真实 HOME，不发网络请求）
make check        # gofmt + vet + test + race
make cross        # 交叉编译 6 个平台
```

架构与协作约定见 [AGENTS.md](AGENTS.md)，设计文档见 [docs/design.md](docs/design.md)。
