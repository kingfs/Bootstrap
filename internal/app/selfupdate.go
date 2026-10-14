package app

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/kingfs/Bootstrap/internal/selfupdate"
	"github.com/kingfs/Bootstrap/pkg/ghrelease"
	"github.com/kingfs/Bootstrap/pkg/httpx"
)

// errNoVersionOutput 表示新二进制跑起来了但没打印版本号（可疑，宁可不换）。
var errNoVersionOutput = errors.New("new binary printed no version output")

// SelfUpdateOptions 是 `bs self-update` 的输入。
type SelfUpdateOptions struct {
	// Repo 覆盖发布仓库（默认 selfupdate.DefaultRepo；便于 fork 自测）。
	Repo string
	// Tag 指定目标版本（空 = 最新）。
	Tag string
	// Force 表示同版本也重装。
	Force bool
	// CheckOnly 表示只查版本号：不下载校验和、不下载归档、不替换。
	CheckOnly bool
}

// SelfUpdatePlan 解析"要不要更新、更新到哪个归档"。
//
// 这一步除了查询版本/校验和（只读网络）不做任何改动，因此 `--check` 与
// `--dry-run` 都只走到这里。
func (a *App) SelfUpdatePlan(ctx context.Context, opt SelfUpdateOptions) (selfupdate.Plan, error) {
	return selfupdate.Resolve(ctx, a.selfUpdateOptions(opt), opt.CheckOnly)
}

// SelfUpdate 执行替换（下载 → 校验 → 自检 → 备份 → 原子替换）。
//
// 调用方要先拿 Plan 给用户看过并拿到同意；dry-run 由调用方直接不调用本方法。
func (a *App) SelfUpdate(ctx context.Context, opt SelfUpdateOptions) (selfupdate.Result, error) {
	if a.DryRun {
		// 双保险：dry-run 下连"下载归档"都不做（Resolve 已经把校验和算出来了）。
		plan, err := a.SelfUpdatePlan(ctx, SelfUpdateOptions{Repo: opt.Repo, Tag: opt.Tag})
		if err != nil {
			return selfupdate.Result{}, err
		}
		return selfupdate.Result{Plan: plan}, nil
	}
	plan, err := selfupdate.Resolve(ctx, a.selfUpdateOptions(opt), false)
	if err != nil {
		return selfupdate.Result{}, err
	}
	return selfupdate.Apply(ctx, a.selfUpdateOptions(opt), plan)
}

// selfUpdateOptions 把 App 的状态翻译成 selfupdate 的输入。
func (a *App) selfUpdateOptions(opt SelfUpdateOptions) selfupdate.Options {
	return selfupdate.Options{
		Repo:    opt.Repo,
		GOOS:    a.Platform.OS,
		GOARCH:  a.Platform.Arch,
		Current: a.CurrentVersion,
		Client:  a.httpClient(),
		Exe:     a.exePath(),
		Tag:     opt.Tag,
		Force:   opt.Force,
		Verify:  a.verifyBinary,
	}
}

// httpClient 造一个走当前网络环境的 HTTP 客户端（代理注入与其它下载一致）。
func (a *App) httpClient() *httpx.Client {
	c := httpx.New(httpx.DefaultTimeout, a.NetworkEnv())
	c.DownloadTimeout = withDefault(a.timeouts().Download, httpx.DefaultDownloadTimeout)
	return c
}

// exePath 返回被替换的二进制路径（测试可注入 ExePath）。
func (a *App) exePath() string {
	if a.ExePath != "" {
		return a.ExePath
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// verifyBinary 在替换前自检新二进制：跑得起来、能打印版本号才算数。
//
// 这是"最后一道防线"：sha256 保证内容没被篡改，但保证不了"这个文件真的能跑"
// （例如下错平台、或归档里放错了东西）。自检失败就保留当前二进制。
func (a *App) verifyBinary(ctx context.Context, path string) error {
	if a.Runner == nil {
		return nil
	}
	out, err := a.Runner.RunCapture(ctx, path, "version")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return errNoVersionOutput
	}
	return nil
}

// applyGitHubMirror 把镜像环境变量应用到 GitHub 地址前缀上；两个都没设时完全不介入。
//
// 为什么需要它：bs 的两条网络路径（按清单下载 release、自更新）都只认 github.com 与
// api.github.com，而这两个地址在很多网络环境下很慢甚至不通。于是提供两个逃生口：
//
//	BS_GITHUB_API          —— 覆盖 API 前缀（默认 https://api.github.com）
//	BS_GITHUB_ASSET_BASE   —— 覆盖产物/校验和下载前缀（默认 https://github.com）
//
// 安全提醒：**校验和与产物来自同一个前缀**，所以镜像必须完全可信——它有能力同时换掉
// 产物和 checksums.txt。这是"用镜像"这件事本身的代价，不是本实现的缺陷（见 design §16）。
// 刻意只认环境变量、不落配置文件：换机器/进公司内网时临时给一下，比把镜像固化进配置更不容易忘。
func applyGitHubMirror() {
	api := strings.TrimSpace(os.Getenv("BS_GITHUB_API"))
	assets := strings.TrimSpace(os.Getenv("BS_GITHUB_ASSET_BASE"))
	if api == "" && assets == "" {
		// 两个都不给＝完全不介入：默认地址怎么都不该被动过。
		// 这条不只是洁癖——测试会把前缀指到假 GitHub 上，构造 App 时若"顺手还原默认"，
		// 就会把注入的地址抹掉，测试转而真的去连 github.com（表现为一串超时假失败）。
		return
	}
	ghrelease.SetBaseURLs(api, assets)
}

// SelfUpdateRepo 返回默认的发布仓库（`--repo` > `BS_REPO` > 内置默认）。
func SelfUpdateRepo() string {
	if v := strings.TrimSpace(os.Getenv("BS_REPO")); v != "" {
		return v
	}
	return selfupdate.DefaultRepo
}
