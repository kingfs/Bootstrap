package app

import (
	"context"
	"fmt"

	"github.com/kingfs/Bootstrap/internal/gitrepo"
	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// RepoOptions 控制仓库操作。
type RepoOptions struct {
	Root      string
	Protocol  string
	Depth     int
	Ref       string
	Autostash bool
}

func (o RepoOptions) toManager() gitrepo.Options {
	return gitrepo.Options{
		Root:      o.Root,
		Protocol:  o.Protocol,
		Depth:     o.Depth,
		Ref:       o.Ref,
		Autostash: o.Autostash,
	}
}

// RepoGetResult 是单个仓库的处理结果。
type RepoGetResult struct {
	Input    string   `json:"input"`
	Slug     string   `json:"slug"`
	Path     string   `json:"path"`
	Action   string   `json:"action"`
	Reason   string   `json:"reason,omitempty"`
	Status   string   `json:"status,omitempty"`
	Message  string   `json:"message,omitempty"`
	Commands []string `json:"commands,omitempty"`
}

// RepoStatusView 在仓库状态上补充"根目录来源"信息。
type RepoStatusView struct {
	gitrepo.RepoStatus
	RootSource string `json:"root_source,omitempty"`
}

// RepoManager 返回编排器（Runner 已注入）。
func (a *App) RepoManager() *gitrepo.Manager {
	return gitrepo.NewManager(a.Cfg, a.Runner)
}

// RepoRoot 返回仓库根目录及其来源。
func (a *App) RepoRoot() (string, string) {
	return a.RepoManager().Root(a.Platform)
}

// withProxyEnv 在 fn 执行期间给 Runner 注入代理环境变量。
func (a *App) withProxyEnv(fn func()) {
	base := a.Runner.Env
	a.Runner.Env = mergeMaps(base, a.NetworkEnv())
	defer func() { a.Runner.Env = base }()
	fn()
}

// RepoGet 处理一批仓库输入（URL / owner/repo / 带分支的网页地址）。
//
// 顺序执行：clone/pull 本身是 IO 密集且用户通常只给一两个仓库；
// 批量场景用 `bs repo sync`（并发）。
func (a *App) RepoGet(ctx context.Context, inputs []string, opts RepoOptions) ([]RepoGetResult, error) {
	mgr := a.RepoManager()
	root := opts.Root
	if root == "" {
		root, _ = a.RepoRoot()
	}
	opts.Root = root
	if opts.Protocol == "" {
		opts.Protocol = a.Cfg.Repo.Clone.Protocol
	}

	results := make([]RepoGetResult, 0, len(inputs))
	var applyErr error
	a.withProxyEnv(func() {
		for _, in := range inputs {
			plan, err := mgr.Plan(in, root, opts.toManager())
			if err != nil {
				applyErr = err
				return
			}
			res := RepoGetResult{
				Input:  in,
				Slug:   plan.Ref.Slug(),
				Path:   plan.Path,
				Action: string(plan.Action),
				Reason: plan.Reason,
			}
			for _, c := range plan.Commands {
				res.Commands = append(res.Commands, c.String())
			}
			if plan.Action == gitrepo.ActionSkip {
				res.Status = "skipped"
				res.Message = plan.Reason
				results = append(results, res)
				continue
			}
			out, err := mgr.Apply(ctx, plan, opts.toManager())
			res.Status, res.Message = out.Status, out.Message
			if err != nil {
				res.Status = "failed"
				results = append(results, res)
				applyErr = fmt.Errorf("%s: %w", in, err)
				return
			}
			results = append(results, res)
		}
	})
	return results, applyErr
}

// RepoList 列出仓库根目录下已有的仓库。
func (a *App) RepoList(ctx context.Context) ([]RepoStatusView, string, error) {
	mgr := a.RepoManager()
	root, source := a.RepoRoot()
	if !fsx.IsDir(root) {
		// 根目录还不存在不是错误：`bs repo list` 应该安静地报告"还没有仓库"。
		return nil, root, nil
	}
	paths, err := gitrepo.Discover(root, 0)
	if err != nil {
		return nil, root, err
	}
	out := make([]RepoStatusView, 0, len(paths))
	for _, p := range paths {
		st, err := mgr.Inspect(ctx, p)
		if err != nil {
			return out, root, err
		}
		out = append(out, RepoStatusView{RepoStatus: st, RootSource: source})
	}
	return out, root, nil
}

// RepoSync 并发更新所有仓库。
func (a *App) RepoSync(ctx context.Context, jobs int, opts RepoOptions) ([]RepoStatusView, error) {
	mgr := a.RepoManager()
	root := opts.Root
	source := ""
	if root == "" {
		root, source = a.RepoRoot()
	}
	var statuses []gitrepo.RepoStatus
	var syncErr error
	a.withProxyEnv(func() {
		statuses, syncErr = mgr.Sync(ctx, root, jobs, opts.toManager())
	})
	if syncErr != nil {
		return nil, syncErr
	}
	out := make([]RepoStatusView, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, RepoStatusView{RepoStatus: st, RootSource: source})
	}
	return out, nil
}
