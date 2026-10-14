package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kingfs/Bootstrap/internal/buildinfo"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// CheckStatus 是体检项的结论。
type CheckStatus string

// 体检结论枚举。
const (
	StatusOK   CheckStatus = "ok"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
	StatusSkip CheckStatus = "skip"
)

// trustStoreHint 给出"怎么补系统 CA 证书"的具体命令。
//
// 按已探测到的包管理器选，而不是让用户自己猜发行版——这正是这条检查存在的意义。
func trustStoreHint(p hostinfo.Platform) string {
	cmds := map[string]string{
		"apt-get": "sudo apt-get install -y ca-certificates",
		"apt":     "sudo apt-get install -y ca-certificates",
		"dnf":     "sudo dnf install -y ca-certificates",
		"yum":     "sudo yum install -y ca-certificates",
		"pacman":  "sudo pacman -S --noconfirm ca-certificates",
		"zypper":  "sudo zypper install -y ca-certificates",
		"apk":     "sudo apk add ca-certificates",
	}
	cmd := ""
	for _, m := range p.PackageManagers {
		if c, ok := cmds[m]; ok {
			cmd = c
			break
		}
	}
	head := "没有系统 CA 证书时，所有 HTTPS 下载都会失败在 x509 校验上"
	if cmd == "" {
		return head + "：请先安装所在发行版的 CA 证书包（如 ca-certificates），再重试"
	}
	return head + "：先执行 `" + cmd + "` 再重试"
}

// Check 是单项体检结果。
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
	Hint   string      `json:"hint,omitempty"`
}

// trustStore 取"系统 CA 证书"探测结果（测试可注入）。
func (a *App) trustStore() hostinfo.TrustStore {
	if a.TrustStore != nil {
		return a.TrustStore()
	}
	return hostinfo.DetectTrustStore()
}

// Doctor 执行一组只读的环境体检。
//
// 原则：**只读**。doctor 不创建任何文件、不改任何配置（`--fix` 属未来项，见 design §18.3）。
func (a *App) Doctor(ctx context.Context) []Check {
	var checks []Check
	add := func(c Check) { checks = append(checks, c) }

	// 1. 平台
	ver := buildinfo.Get()
	platDetail := fmt.Sprintf("%s/%s", a.Platform.OS, a.Platform.Arch)
	if a.Platform.Distro != "" {
		platDetail += " " + a.Platform.Distro + " " + a.Platform.DistroVer
	}
	if a.Platform.Shell != "" {
		platDetail += " shell=" + a.Platform.Shell
	}
	platStatus := StatusOK
	if !a.Platform.Supported() {
		platStatus = StatusWarn
	}
	add(Check{Name: "platform", Status: platStatus, Detail: platDetail,
		Hint: hintIf(platStatus == StatusWarn, "该平台暂未在首批清单中覆盖（M5+ 补齐 Windows）")})
	add(Check{Name: "bs", Status: StatusOK, Detail: ver.Version + " (" + ver.GoVersion + ")"})

	// 2. 配置
	// 配置：优先报"实际读了哪几份"。修复过（或刚生成）之后 ConfigSources 还是旧的，
	// 因此再补一条"磁盘上确实有用户级配置吗"的实况判断——否则 `--fix` 修好了、
	// 紧接着的复查却还说"没找到配置"，看起来像没修成功。
	cfgStatus := StatusWarn
	cfgDetail := "未找到配置文件，正在使用内置默认值"
	switch {
	case len(a.ConfigSources) > 0:
		cfgStatus = StatusOK
		cfgDetail = strings.Join(a.ConfigSources, ", ")
	case fsx.IsFile(a.Paths.ConfigFile()):
		cfgStatus = StatusOK
		cfgDetail = a.Paths.ConfigFile() + "（重启后生效）"
	}
	add(Check{Name: "config", Status: cfgStatus, Detail: cfgDetail,
		Hint: hintIf(cfgStatus == StatusWarn, "运行 `bs config init` 生成默认配置")})

	// 3. 目录可写性（不创建目录，只检查最近的已存在祖先）
	for _, item := range []struct{ name, path string }{
		{"config dir", a.Paths.ConfigDir},
		{"state dir", a.Paths.StateDir},
		{"cache dir", a.Paths.CacheDir},
	} {
		st, detail := writableAncestor(item.path)
		add(Check{Name: item.name, Status: st, Detail: item.path + " — " + detail,
			Hint: hintIf(st != StatusOK, "请检查目录权限")})
	}

	// 4. 外部命令
	//
	// 只做路径检查 + 无副作用的版本查询。刻意**不**执行 `go version`：
	// 即便设置 GOTELEMETRY=off，Go 工具链仍会在 os.UserConfigDir() 下创建
	// go/telemetry 计数文件，而 doctor 承诺"只读"。Go 版本探测留给
	// `bs tool`（M2），届时会把 HOME 指向 bs 自己的缓存目录再探测。
	for _, bin := range []struct {
		name    string
		args    []string
		probe   bool
		require bool
	}{
		{"git", []string{"--version"}, true, true},
		{"curl", []string{"--version"}, true, false},
		{"go", nil, false, false},
	} {
		path, ok := a.lookPath(bin.name)
		if !ok {
			st := StatusSkip
			hint := ""
			if bin.require {
				st = StatusFail
				hint = fmt.Sprintf("`bs repo` 依赖 git，请先安装 %s", bin.name)
			}
			add(Check{Name: bin.name, Status: st, Detail: "未找到", Hint: hint})
			continue
		}
		detail := path
		if bin.probe {
			if res, err := a.Runner.Run(ctx, cmdx.Command{Name: bin.name, Args: bin.args}); err == nil {
				if line := firstLine(strings.TrimSpace(res.Stdout + res.Stderr)); line != "" {
					detail += " — " + line
				}
			}
		}
		add(Check{Name: bin.name, Status: StatusOK, Detail: detail})
	}

	// 4.5 系统 CA 证书
	//
	// 这一条是"在干净容器里跑集成测试"逼出来的：ubuntu:24.04 的 docker 镜像里没有
	// /etc/ssl/certs/ca-certificates.crt，于是**所有** HTTPS 下载都失败在
	// `x509: certificate signed by unknown authority`，而那句话完全没提"装 CA 证书"。
	// doctor 的职责就是把这种"下一步该做什么"讲清楚（真正的修复是装系统包，超出
	// doctor --fix 的范围：它只改 bs 自己的配置/目录/受管块）。
	if ts := a.trustStore(); ts.Checked {
		st, detail := StatusOK, ts.Path
		if !ts.Present {
			st = StatusFail
			detail = "未找到系统 CA 证书（找过：" + strings.Join(ts.Searched, "、") + "）"
		}
		add(Check{Name: "ca certs", Status: st, Detail: detail,
			Hint: hintIf(st == StatusFail, trustStoreHint(a.Platform))})
	}

	// 5. 包管理器
	if len(a.Platform.PackageManagers) > 0 {
		add(Check{Name: "package managers", Status: StatusOK, Detail: strings.Join(a.Platform.PackageManagers, ", ")})
	} else {
		add(Check{Name: "package managers", Status: StatusWarn, Detail: "未检测到系统包管理器"})
	}

	// 5.5 工具清单（只读，且**不探测**：探测会执行工具自身的 --version，
	// 那可能触发对方写自己的状态文件，例如 go 的遥测计数）。
	if loaded, err := a.LoadedManifest(); err != nil {
		add(Check{Name: "tool manifest", Status: StatusFail, Detail: err.Error(),
			Hint: "运行 `bs tool manifest validate` 查看具体问题"})
	} else if verr := a.ValidateManifestChecked(loaded); verr != nil {
		add(Check{Name: "tool manifest", Status: StatusFail, Detail: verr.Error(),
			Hint: "运行 `bs tool manifest validate` 查看具体问题"})
	} else {
		add(Check{Name: "tool manifest", Status: StatusOK,
			Detail: fmt.Sprintf("%d 个工具，%d 个 profile，%d 个来源（用 `bs tool list` 看安装状态）",
				len(loaded.Tools), len(loaded.Profiles), len(loaded.Sources))})
	}

	// 6. 代理
	summary, err := a.ProxyStatus()
	switch {
	case err != nil:
		add(Check{Name: "proxy", Status: StatusFail, Detail: err.Error()})
	case summary.Source == "none":
		add(Check{Name: "proxy", Status: StatusSkip, Detail: "未启用代理",
			Hint: "如在国内网络环境，可运行 `bs proxy use <profile>`；连通性用 `bs proxy test` 验证"})
	default:
		detail := fmt.Sprintf("%s (%s)", firstNonEmpty(summary.Active, summary.Source), summary.Detail)
		if summary.HTTPSProxy != "" {
			detail += " https=" + summary.HTTPSProxy
		}
		add(Check{Name: "proxy", Status: StatusOK, Detail: detail})
	}

	// 7. PATH 条目
	missing := 0
	var entries []string
	for _, e := range a.PathEntries() {
		entries = append(entries, e.Path)
		if !e.Exists {
			missing++
		}
	}
	switch {
	case len(entries) == 0:
		add(Check{Name: "path entries", Status: StatusSkip, Detail: "无"})
	case missing > 0:
		add(Check{Name: "path entries", Status: StatusWarn,
			Detail: fmt.Sprintf("%d/%d 个目录尚不存在", missing, len(entries)),
			Hint:   "安装对应工具后目录会自动出现；`bs env path` 可查看完整清单"})
	default:
		add(Check{Name: "path entries", Status: StatusOK, Detail: strings.Join(entries, ", ")})
	}

	// 8. 仓库根目录
	root, source := a.RepoRoot()
	if fsx.IsDir(root) {
		count := 0
		if paths, err := discoverCount(root); err == nil {
			count = paths
		}
		add(Check{Name: "repo root", Status: StatusOK, Detail: fmt.Sprintf("%s (%s, %d 个仓库)", root, source, count)})
	} else {
		add(Check{Name: "repo root", Status: StatusWarn, Detail: root + " 不存在 (" + source + ")",
			Hint: "`bs repo get <repo>` 会自动创建"})
	}

	// 9. shell 受管块
	shell := a.Platform.Shell
	if shell == "" {
		shell = "bash"
	}
	if st, err := a.ShellStatusOf(shell, ""); err == nil {
		switch {
		case !st.HasBlock:
			add(Check{Name: "shell rc", Status: StatusWarn, Detail: st.RCFile + " 中未安装受管块",
				Hint: "`bs shell install` 安装（可先用 `--dry-run` 预览）"})
		case !st.UpToDate:
			add(Check{Name: "shell rc", Status: StatusWarn, Detail: st.RCFile + " 中的受管块与当前版本不一致",
				Hint: "`bs shell install` 原地更新受管块"})
		default:
			add(Check{Name: "shell rc", Status: StatusOK, Detail: st.RCFile + " 已安装且为最新"})
		}
	} else {
		add(Check{Name: "shell rc", Status: StatusSkip, Detail: err.Error()})
	}

	return checks
}

// Summary 返回 (ok, warn, fail, skip) 计数。
func CheckSummary(checks []Check) (ok, warn, fail, skip int) {
	for _, c := range checks {
		switch c.Status {
		case StatusOK:
			ok++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		case StatusSkip:
			skip++
		}
	}
	return
}

func discoverCount(root string) (int, error) {
	paths, err := gitrepoDiscover(root)
	if err != nil {
		return 0, err
	}
	return len(paths), nil
}

func writableAncestor(path string) (CheckStatus, string) {
	dir := path
	for {
		if fsx.Exists(dir) {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return StatusFail, "存在但不是目录"
			}
			if err := probeWritable(dir); err != nil {
				return StatusFail, "不可写：" + err.Error()
			}
			if dir == path {
				return StatusOK, "存在且可写"
			}
			return StatusOK, "将创建（父目录 " + dir + " 可写）"
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return StatusFail, "无法确定可写位置"
		}
		dir = parent
	}
}

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".bs-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func hintIf(cond bool, hint string) string {
	if cond {
		return hint
	}
	return ""
}

// Platform 暴露只读平台信息（供 CLI 展示）。
func (a *App) PlatformInfo() hostinfo.Platform { return a.Platform }
