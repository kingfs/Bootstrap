// Package gitrepo 负责"把代码拉到本地并保持更新"的编排（需求 1）。
//
// 设计要点：
//   - 直接调用系统 git，复用用户的 SSH key / credential helper / .gitconfig（不做 go-git 重实现）。
//   - Plan 只依赖文件系统做判断（路径是否存在、是否 git 仓库），不发任何网络请求；
//     脏工作区检查这类必须运行时才知道的信息放在 Apply 阶段，保证 Plan 绝对无副作用。
//   - 绝不删除用户已有内容：目标目录非空且不是 git 仓库时，默认 skip 并提示。
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/giturl"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// Runner 是执行外部命令的最小接口（cmdx.Runner 天然满足）。
type Runner interface {
	Run(ctx context.Context, c cmdx.Command) (cmdx.Result, error)
}

// Action 是对某个仓库将要执行的动作。
type Action string

// 动作枚举。
const (
	ActionClone Action = "clone"
	ActionPull  Action = "pull"
	ActionSkip  Action = "skip"
)

// Options 控制单次仓库操作。
type Options struct {
	Root      string
	Protocol  string
	Depth     int
	Ref       string // 覆盖 URL 中解析出的 ref
	Autostash bool
}

// Plan 是一次仓库操作的执行计划。
type Plan struct {
	Raw      string
	Ref      giturl.Ref
	Path     string
	Action   Action
	Reason   string
	Commands []cmdx.Command
	// Existed 表示目标目录在本次操作前是否已存在。
	Existed bool
}

// Outcome 是 Apply 的结果。
type Outcome struct {
	Plan    Plan
	Status  string // cloned | updated | up-to-date | skipped | failed
	Message string
}

// Manager 编排仓库操作。
type Manager struct {
	Cfg    *config.Config
	Runner Runner
}

// NewManager 构造 Manager。
func NewManager(cfg *config.Config, runner Runner) *Manager {
	return &Manager{Cfg: cfg, Runner: runner}
}

// Root 解析仓库根目录，返回值带上来源说明（供 `bs repo root` 展示）。
func (m *Manager) Root(p hostinfo.Platform) (string, string) {
	if strings.TrimSpace(m.Cfg.Repo.Root) != "" {
		return m.Cfg.Repo.Root, "config repo.root"
	}
	home := p.HomeDir
	if home == "" {
		home = fsx.HomeDir()
	}
	if p.OS == "windows" {
		for _, cand := range []string{`D:\src`, filepath.Join(home, "src")} {
			if fsx.IsDir(cand) {
				return cand, "auto-detected (existing)"
			}
		}
		return filepath.Join(home, "src"), "default (windows)"
	}
	if cand := filepath.Join(home, "src"); fsx.IsDir(cand) {
		return cand, "auto-detected (existing)"
	}
	return filepath.Join(home, "src"), "default"
}

// TargetPath 计算某个仓库在本地的目标路径。
func (m *Manager) TargetPath(ref giturl.Ref, root string) (string, error) {
	rel, err := giturl.RenderLayout(m.Cfg.Repo.Layout, ref)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// Plan 计算"这个仓库要做什么"。
func (m *Manager) Plan(raw, root string, opts Options) (Plan, error) {
	ref, err := giturl.Parse(raw, m.Cfg.Repo.DefaultHost)
	if err != nil {
		return Plan{}, err
	}
	if opts.Ref != "" {
		ref.Ref = opts.Ref
	}
	path, err := m.TargetPath(ref, root)
	if err != nil {
		return Plan{}, err
	}

	protocol := opts.Protocol
	if protocol == "" {
		protocol = m.Cfg.Repo.Clone.Protocol
	}
	depth := opts.Depth
	if depth == 0 {
		depth = m.Cfg.Repo.Clone.Depth
	}
	cloneURL := ref.CloneURL(protocol)

	plan := Plan{Raw: raw, Ref: ref, Path: path}

	switch {
	case !fsx.Exists(path):
		plan.Action = ActionClone
		plan.Reason = "目标不存在，将克隆"
		plan.Commands = []cmdx.Command{m.cloneCommand(cloneURL, path, ref.Ref, depth)}

	case fsx.IsDir(filepath.Join(path, ".git")):
		plan.Existed = true
		plan.Action = ActionPull
		plan.Reason = "目标已是 git 仓库，将更新"
		plan.Commands = m.pullCommands(path)

	case isDirEmpty(path):
		plan.Action = ActionClone
		plan.Reason = "目标为空目录，将克隆进去"
		plan.Commands = []cmdx.Command{m.cloneCommand(cloneURL, path, ref.Ref, depth)}

	default:
		plan.Existed = true
		plan.Action = ActionSkip
		plan.Reason = fmt.Sprintf("目标 %s 已存在且不是 git 仓库，为避免覆盖已跳过", path)
	}

	return plan, nil
}

func (m *Manager) cloneCommand(cloneURL, path, ref string, depth int) cmdx.Command {
	args := []string{"clone"}
	if depth > 0 {
		args = append(args, "--depth", fmt.Sprint(depth))
	}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, m.Cfg.Repo.Clone.ExtraArgs...)
	args = append(args, cloneURL, path)
	return cmdx.Command{Name: "git", Args: args}
}

func (m *Manager) pullCommands(path string) []cmdx.Command {
	fetchArgs := []string{"-C", path, "fetch", "--all"}
	if m.Cfg.Repo.Pull.Prune {
		fetchArgs = append(fetchArgs, "--prune")
	}
	strategy := m.Cfg.Repo.Pull.Strategy
	if strategy == "" {
		strategy = "ff-only"
	}
	pullArgs := []string{"-C", path, "pull", "--" + strategy}
	return []cmdx.Command{
		{Name: "git", Args: fetchArgs},
		{Name: "git", Args: pullArgs},
	}
}

// Apply 执行计划。
func (m *Manager) Apply(ctx context.Context, plan Plan, opts Options) (Outcome, error) {
	out := Outcome{Plan: plan}
	switch plan.Action {
	case ActionSkip:
		out.Status = "skipped"
		out.Message = plan.Reason
		return out, nil
	case ActionClone:
		res, err := m.Runner.Run(ctx, plan.Commands[0])
		if err != nil {
			out.Status = "failed"
			out.Message = err.Error()
			return out, err
		}
		if res.DryRun {
			// 干跑时命令没有真正执行，报告"计划"比谎报"已克隆"诚实得多。
			out.Status = "planned"
			out.Message = "dry-run：未执行任何命令"
			return out, nil
		}
		out.Status = "cloned"
		out.Message = plan.Ref.Slug()
		return out, nil
	case ActionPull:
		return m.applyPull(ctx, plan, opts)
	default:
		return out, fmt.Errorf("unknown action %q", plan.Action)
	}
}

func (m *Manager) applyPull(ctx context.Context, plan Plan, opts Options) (Outcome, error) {
	out := Outcome{Plan: plan}

	// 1) fetch
	fetchRes, err := m.Runner.Run(ctx, plan.Commands[0])
	if err != nil {
		out.Status = "failed"
		out.Message = err.Error()
		return out, err
	}
	if fetchRes.DryRun {
		out.Status = "planned"
		out.Message = "dry-run：未执行任何命令"
		return out, nil
	}

	// 2) 脏工作区检查（运行时信息，Plan 阶段无法得知）
	dirty, err := m.isDirty(ctx, plan.Path)
	if err != nil {
		out.Status = "failed"
		out.Message = err.Error()
		return out, err
	}
	autostash := opts.Autostash || m.Cfg.Repo.Pull.Autostash
	if dirty && !autostash {
		out.Status = "skipped"
		out.Message = "工作区有未提交改动，已跳过 pull（可用 --autostash 自动 stash）"
		return out, nil
	}

	before, _ := m.headCommit(ctx, plan.Path)

	// 3) 必要时 stash
	if dirty && autostash {
		if _, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", plan.Path, "stash", "push", "--include-untracked"}}); err != nil {
			out.Status = "failed"
			out.Message = err.Error()
			return out, err
		}
		defer func() {
			_, _ = m.Runner.Run(context.WithoutCancel(ctx), cmdx.Command{Name: "git", Args: []string{"-C", plan.Path, "stash", "pop"}})
		}()
	}

	// 4) pull
	if _, err := m.Runner.Run(ctx, plan.Commands[1]); err != nil {
		out.Status = "failed"
		out.Message = err.Error()
		return out, err
	}

	after, _ := m.headCommit(ctx, plan.Path)
	switch {
	case before != "" && before == after:
		out.Status = "up-to-date"
		out.Message = "已是最新"
	default:
		out.Status = "updated"
		out.Message = "已更新"
	}
	return out, nil
}

func (m *Manager) isDirty(ctx context.Context, path string) (bool, error) {
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "status", "--porcelain"}})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) != "", nil
}

func (m *Manager) headCommit(ctx context.Context, path string) (string, error) {
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "rev-parse", "HEAD"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// RepoStatus 是一个本地仓库的状态快照。
type RepoStatus struct {
	Path    string `json:"path"`
	Slug    string `json:"slug,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Dirty   bool   `json:"dirty"`
	Action  string `json:"action,omitempty"`
	Message string `json:"message,omitempty"`
}

// Discover 在 root 下查找所有 git 仓库目录（不进入仓库内部继续递归）。
func Discover(root string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		maxDepth = 6
	}
	if !fsx.IsDir(root) {
		// 用 %w 包住哨兵错误，调用方（CLI）才能把它映射成"前置条件不满足"而不是一般失败。
		return nil, fmt.Errorf("%w: %s", ErrNoRoot, root)
	}
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个目录不可读不应中断整体发现。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == ".git" {
			return fs.SkipDir
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil && rel != "." && strings.Count(rel, string(filepath.Separator)) >= maxDepth {
			return fs.SkipDir
		}
		if fsx.IsDir(filepath.Join(path, ".git")) {
			found = append(found, path)
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// Inspect 读取一个本地仓库的分支、远端与脏状态。
func (m *Manager) Inspect(ctx context.Context, path string) (RepoStatus, error) {
	st := RepoStatus{Path: path}
	if res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "rev-parse", "--abbrev-ref", "HEAD"}}); err == nil {
		st.Branch = strings.TrimSpace(res.Stdout)
	}
	if res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "remote", "get-url", "origin"}}); err == nil {
		remote := strings.TrimSpace(res.Stdout)
		if ref, perr := giturl.Parse(remote, m.Cfg.Repo.DefaultHost); perr == nil {
			st.Slug = ref.Slug()
		}
	}
	if dirty, err := m.isDirty(ctx, path); err == nil {
		st.Dirty = dirty
	}
	return st, nil
}

// Sync 并发更新 root 下的所有仓库。
func (m *Manager) Sync(ctx context.Context, root string, jobs int, opts Options) ([]RepoStatus, error) {
	paths, err := Discover(root, 0)
	if err != nil {
		return nil, err
	}
	if jobs <= 0 {
		jobs = m.Cfg.Repo.Sync.Jobs
	}
	if jobs <= 0 {
		jobs = config.DefaultSyncJobs
	}

	results := make([]RepoStatus, len(paths))
	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, jobs)
	for i, p := range paths {
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = m.syncOne(gctx, p, opts)
			return nil // 单个仓库失败不中断整体，结果记录在结果集中
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

func (m *Manager) syncOne(ctx context.Context, path string, opts Options) RepoStatus {
	st := RepoStatus{Path: path}
	if inspected, err := m.Inspect(ctx, path); err == nil {
		st = inspected
	}
	plan := Plan{Path: path, Action: ActionPull, Commands: m.pullCommands(path), Reason: "批量同步"}
	out, err := m.Apply(ctx, plan, opts)
	switch {
	case err != nil:
		st.Action = "failed"
		st.Message = out.Message
	case out.Status == "skipped":
		st.Action = "skipped"
		st.Message = out.Message
	default:
		st.Action = "pulled"
		st.Message = out.Message
	}
	return st
}

// ErrNoRoot 表示仓库根目录不存在。
var ErrNoRoot = errors.New("repository root does not exist")

func isDirEmpty(path string) bool {
	if !fsx.IsDir(path) {
		return false
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) == 0
}
