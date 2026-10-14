# Bootstrap (`bs`)

个人开发环境重建工具：换一台新机器后，用几条命令把仓库、代理、shell 环境恢复成熟悉的样子。

- **一个二进制，零运行时依赖**（Go 静态编译，`linux`/`macOS`/`Windows` × `amd64`/`arm64`）
- **不改你的 rc 文件内容**：只在 rc 里放一个可精确移除的"受管块"
- **先看后做**：所有会改动文件系统或外部状态的命令都支持 `--dry-run`
- **可脚本化**：`--json` 输出 + 明确区分的退出码

> 当前进度：M1（仓库拉取、代理、shell 集成、体检）、M2（工具清单 / 安装引擎，`bs tool`）与
> M3（新机器一把梭 `bs init`、自更新 `bs self-update`、交互式界面）已完成；Windows 的工具清单尚未编写。

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

新机器上先一把梭（认环境 → 选工具 → 装好）：

```sh
bs init                         # 交互式：看概览 → 勾选工具 → 确认 → 装（可选顺手装受管块）
bs init --dry-run               # 只看看会发生什么，一行都不写
bs init --profile base --yes    # 全自动：用默认套装，不问任何问题
bs init --missing-only --yes    # 只补当前缺的
```

日常则是这套循环：

```sh
bs config init                  # 生成默认配置（含本地/公司代理 profile 示例）
bs shell install                # 在 rc 里装受管块：代理立即生效 + PATH 装配
bs doctor                       # 看看环境还缺什么
bs doctor --fix                 # 缺什么顺手补上（配置 / 仓库根 / 受管块；--dry-run 可先看）

bs proxy use local              # 启用代理（当前 shell 立即生效）
bs repo get kingfs/Bootstrap    # 克隆或更新到 ~/src/github.com/kingfs/Bootstrap

bs tool list                    # 看清单里哪些装了、哪些缺（实时探测）
bs tool plan --profile golang   # 先看要做什么
bs tool install --profile golang  # 再动手（含依赖，按拓扑序）

bs self-update                  # 把 bs 自己更新到最新发布版（sha256 校验）
```

## 命令一览

### 开荒与自更新（`bs init` / `bs self-update`）

| 命令 | 说明 |
| --- | --- |
| `bs init` | 新机器一把梭：打印环境概览 → 选工具（交互勾选 / 套装 / 只补缺失）→ 确认 → 安装，可选顺手 `shell install` |
| `bs init --profile P` / `--tools a,b` / `--missing-only` | 明确"装什么"，不再问选择（三者互斥） |
| `bs init --with-shell` / `--no-shell` | 是否在收尾时安装 shell 受管块（交互时问一次） |
| `bs self-update` | 下载最新发布版、校验 sha256、自检新二进制能跑、备份旧文件后原地替换 |
| `bs self-update --check` | 只查有没有新版本（不下载、不改动） |
| `bs self-update --version vX.Y.Z` / `--force` / `--repo owner/repo` | 指定版本 / 同版本重装 / 换 release 来源 |

`bs init` 的安全约定：`--dry-run` 一个字节都不写；非交互环境（管道/CI/`--json`）没有 `--yes`
就以退出码 5 拒绝；确认框里按回车＝拒绝；Ctrl-C 取消＝什么都没做（退出码 0）。
`bs self-update` 在替换前会**先跑一遍新二进制的 `version`**，跑不起来就保留旧的。

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

### 工具（`bs tool`）

工具清单是**声明式 YAML**：每个条目说清"怎么判断装没装"（`verify`）和"按优先级排列的安装方式"（`install`）。
`bs` 严格按清单里的书写顺序选第一个可用方式，不在代码里硬编码偏好。

| 命令 | 说明 |
| --- | --- |
| `bs tool list` | 列出工具及状态（`installed`/`outdated`/`missing`/`unknown`） |
| `bs tool search <关键词>` | 按名字/描述/标签搜索 |
| `bs tool info <工具>` | 单个工具的详情、方式链（哪个可用、哪个会选中）与依赖 |
| `bs tool plan [...]` | 只打印将要执行的步骤，不做任何修改（离线预览，不解析"最新版本"） |
| `bs tool install [...]` | 安装（幂等：已装的跳过；含依赖，按拓扑序） |
| `bs tool install -i [...]` | 先探测再用交互界面勾选要处理的工具（`-i` 对 `upgrade`/`uninstall` 同样有效） |
| `bs tool upgrade [...]` | 更新（交给各自的包管理器/版本管理器） |
| `bs tool uninstall [...]` | 卸载（交给各自的包管理器/官方方式；清单可单独写 `uninstall:` 链） |
| `bs tool manifest path\|validate\|show\|diff` | 清单来自哪里、是否写错、合并后长什么样 |

选择方式（`plan`/`install`/`upgrade`/`uninstall` 通用）：`--profile`、`--stack`、`--kind`、`--tag`、`--missing`，
以及位置参数指定的工具 id 或 `前缀/*`。`--via <方式>` 强制某一种方式（**只约束显式点名的工具**，依赖仍按自己的清单解析）。

```sh
bs tool list --missing                 # 这台机器上还缺什么
bs tool install --profile golang        # 装整套：g → golang → gopls/dlv/staticcheck
bs tool install -i --stack base         # 在 base 里勾着装（缺失项已默认勾选）
bs tool uninstall -i                    # 反过来，从已装的中勾着卸载
bs tool install uv --via brew           # 只对 uv 强制用 brew
bs tool plan --tag monitor --json       # 计划也能机读
```

常用 profile：`base`（日常 CLI）、`golang`、`nodejs`、`python`、`rust`、`cpp`、`containers`、`infra`、`agent`、`full`、
`managers`（只装全部版本管理器）。用 `bs tool manifest show` 查看当前平台实际有哪些。

清单来源（后者覆盖前者，同名 id 按字段合并）：

```
二进制内置 tools/{common,<os>}.yaml + profiles.yaml   # 随 bs 发布，换机器不用同步文件
<config>/tools/*.yaml                                 # 你的覆盖层，支持 `+install` / `+tags` 追加
```

跨平台校验清单（在 Linux 上检查 macOS/Windows 的清单）：

```sh
bs tool manifest validate --os darwin
```

字段速查见 [docs/tool-schema.md](docs/tool-schema.md)。

### 配置与体检

| 命令 | 说明 |
| --- | --- |
| `bs config path` | 各配置/状态目录的实际路径 |
| `bs config show` | 合并后的生效配置 |
| `bs config validate` | 校验配置（一次性列出全部问题） |
| `bs config init [--force]` | 生成默认配置（已存在不覆盖） |
| `bs doctor` | 只读体检：平台、配置、目录权限、git/curl、包管理器、代理、PATH、仓库根、工具清单、受管块 |
| `bs doctor --fix` | 顺手修掉能安全自动修的问题：生成配置、建仓库根、装 shell 受管块（写前备份；先给计划再问一句） |
| `bs version` | 版本信息（`--json` 可脚本消费） |

全局参数里有两个和"等多久"有关：`--timeout 30s` 给整条命令设总时限（到点连子进程一起停掉），
配置文件 `timeouts.command`/`download` 则用来放宽单个命令与下载的默认时限（10m / 15m）。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 一般失败 |
| 2 | 用法错误（未知命令/参数非法） |
| 3 | 部分成功（批量操作中有失败项） |
| 4 | 前置条件不满足（配置缺失/非法、体检失败） |
| 5 | 需要交互但当前非交互 |

交互约定：安装/升级/卸载这类会改动系统或用户目录的操作，执行前会要求确认。

- 终端里：打印计划后问一句，按钮上写着"继续 / 取消"，**回车＝取消**（默认拒绝）；
- 在真终端上，多选/单选/确认用交互式界面（方向键、空格勾选、`y`/`n`、回车提交），
  想用朴素问答就加 `--plain-ui`；`--interactive` 可强制开启提问，`--non-interactive` 强制关闭；
- 取消（Ctrl-C，或回答 no）不是失败：退出码 0 + "未做任何修改"；
- 非交互（管道、CI、`--json`、`--non-interactive`）**绝不阻塞等待输入**：
  没有 `--yes` 就直接以退出码 5 结束，并提示加 `--yes`；
- `--dry-run` 在打印计划后就结束，连确认都不会问。

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

下载缓存放在 `BS_CACHE_HOME`（默认 `~/.cache/bootstrap/downloads`）：断线可以接着下，
同一个归档校验和一致时直接复用；删掉它只会让下次重新下载，不影响任何状态。

常用环境变量：`BS_CONFIG_HOME`、`BS_STATE_HOME`、`BS_CACHE_HOME`、`BS_REPO_ROOT`、
`BS_PROXY_PROFILE`、`BS_LOG_LEVEL`、`BS_TIMEOUT`（整条命令的总时限）、`NO_COLOR`。完整字段见
[docs/configuration.md](docs/configuration.md)。

## 开发

```sh
make build        # 构建 bin/bs
make test         # 单元测试
make race         # 单元测试（-race）
make smoke        # 无副作用端到端冒烟测试（沙箱目录，不碰真实 HOME，不发网络请求）
make check        # gofmt + vet + test + race
make pty          # 在真 PTY 里验证交互式界面（勾选/取消/默认拒绝）
make e2e          # 真实下载安装验证（**需要网络**，产物只落在 .cache/e2e）
make cross        # 交叉编译 6 个平台
```

架构与协作约定见 [AGENTS.md](AGENTS.md)，设计文档见 [docs/design.md](docs/design.md)。
