package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kingfs/Bootstrap/internal/gitrepo"
	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// ErrRepoNotFound 表示仓库索引里没有与查询匹配的条目。
var ErrRepoNotFound = errors.New("repository not found in index")

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
	Note     string   `json:"note,omitempty"`
	Commands []string `json:"commands,omitempty"`
}

// RepoStatusView 在仓库状态上补充"根目录来源"信息。
type RepoStatusView struct {
	gitrepo.RepoStatus
	RootSource string `json:"root_source,omitempty"`
}

// RepoManager 返回编排器（Runner 已注入）。
func (a *App) RepoManager() *gitrepo.Manager {
	m := gitrepo.NewManager(a.Cfg, a.Runner)
	// known_hosts 的查找/写入基准：ssh 自己看的也是 $HOME。
	m.HomeDir = a.Platform.HomeDir
	return m
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
			res.Status, res.Message, res.Note = out.Status, out.Message, out.Note
			if err != nil {
				res.Status = "failed"
				results = append(results, res)
				applyErr = fmt.Errorf("%s: %w", in, err)
				return
			}
			results = append(results, res)
			// 干跑与计划阶段不写盘（AGENTS §4.2）；其余情况只要仓库真的在本地，
			// 就并进轻量索引，后续 `repo list`/`repo cd` 无需先跑 `repo scan`。
			//
			// 这里刻意**不**按状态过滤：pull 因"脏工作区 / 游离 HEAD / 没有 upstream"
			// 被跳过时状态也是 skipped，但仓库明明就在本地，把它漏掉正是用户
			// "get 成功却 cd 不进去"的来源。
			if a.DryRun || res.Status == "planned" {
				continue
			}
			if err := upsertRepoIndex(ctx, mgr, root, plan.Path, plan.Ref.Slug()); err != nil {
				applyErr = err
				return
			}
		}
	})
	return results, applyErr
}

// upsertRepoIndex 把一个本地仓库并进索引（索引不存在时创建）。
//
// slug 只是兜底：仓库实际配置的 origin 更权威（可能被 `url.<base>.insteadOf`
// 改写过，或与这次的输入写法不同），能探测到就用探测结果。
func upsertRepoIndex(ctx context.Context, mgr *gitrepo.Manager, root, path, slug string) error {
	idx, err := gitrepo.LoadIndex(ctx, root)
	if err != nil && !errors.Is(err, gitrepo.ErrNoIndex) {
		return fmt.Errorf("load repository index: %w", err)
	}
	st, _ := mgr.Inspect(ctx, path)
	if st.Slug != "" {
		slug = st.Slug
	}
	entry := gitrepo.IndexEntry{
		Path:          path,
		Slug:          slug,
		Branch:        st.Branch,
		DefaultBranch: st.DefaultBranch,
		Worktree:      st.Worktree,
	}
	for i := range idx {
		if idx[i].Path == path {
			idx[i] = entry
			return saveIndex(ctx, root, idx)
		}
	}
	return saveIndex(ctx, root, append(idx, entry))
}

// saveIndex 写索引并把错误统一带上上下文。
func saveIndex(ctx context.Context, root string, entries []gitrepo.IndexEntry) error {
	if err := gitrepo.SaveIndex(ctx, root, entries); err != nil {
		return fmt.Errorf("save repository index: %w", err)
	}
	return nil
}

// RepoLookup 把用户给的仓库写法解析成索引里的本地路径（`bs repo cd` 的实现）。
//
// 接受与 `bs repo get` 相同的输入形态——URL、`git@host:owner/repo.git`、
// `owner/repo`、带 `/tree/<branch>` 的网页地址——以及索引里记录的 slug、目录名
// 和本地绝对路径。理由很直接：用户刚 `get` 完手里就是那条 URL，没道理要求
// 他换成另一种写法才能 `cd`。
//
// 找不到时返回 ErrRepoNotFound；索引缺失时原样返回 gitrepo.ErrNoIndex，
// 调用方据此提示先 `bs repo scan`（绝不在这里偷偷遍历用户目录）。
func (a *App) RepoLookup(ctx context.Context, query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("%w: empty query", ErrRepoNotFound)
	}
	root, _ := a.RepoRoot()
	idx, err := gitrepo.LoadIndex(ctx, root)
	if err != nil {
		return "", err
	}
	// 归一化后的目标路径与 slug：URL 之类的写法靠它们命中。
	wantPath, wantSlug := "", ""
	if plan, perr := a.RepoManager().Plan(q, root, gitrepo.Options{}); perr == nil {
		wantPath, wantSlug = plan.Path, plan.Ref.Slug()
	}
	// 本地路径写法（绝对路径，或相对当前目录的路径）。
	if abs, aerr := filepath.Abs(q); aerr == nil {
		abs = filepath.Clean(abs)
		for _, e := range idx {
			if filepath.Clean(e.Path) == abs {
				return e.Path, nil
			}
		}
	}
	lower := strings.ToLower(q)
	for _, e := range idx {
		// 依次尝试：归一化路径 / 归一化 slug / 输入本身是 slug / 省略 host 的
		// slug 后缀（owner/repo、owner/sub/repo，`cd kingfs/Bootstrap` 不必关心
		// 它当初是从哪个站点拉的）/ 目录名。
		switch {
		case wantPath != "" && filepath.Clean(e.Path) == filepath.Clean(wantPath),
			wantSlug != "" && strings.EqualFold(e.Slug, wantSlug),
			strings.EqualFold(e.Slug, lower),
			strings.HasSuffix(strings.ToLower(e.Slug), "/"+lower),
			strings.EqualFold(filepath.Base(e.Path), q):
			return e.Path, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrRepoNotFound, query)
}

// RepoList 列出仓库根目录下已有的仓库。
func (a *App) RepoList(ctx context.Context) ([]RepoStatusView, string, error) {
	root, source := a.RepoRoot()
	if !fsx.IsDir(root) {
		// 根目录还不存在不是错误：`bs repo list` 应该安静地报告"还没有仓库"。
		return nil, root, nil
	}
	idx, err := gitrepo.LoadIndex(ctx, root)
	if err != nil {
		return nil, root, err
	}
	out := make([]RepoStatusView, 0, len(idx))
	for _, e := range idx {
		st := gitrepo.RepoStatus{Path: e.Path, Slug: e.Slug, Branch: e.Branch, DefaultBranch: e.DefaultBranch, Worktree: e.Worktree}
		out = append(out, RepoStatusView{RepoStatus: st, RootSource: source})
	}
	return out, root, nil
}

// RepoScan 全面扫描并刷新仓库索引。
func (a *App) RepoScan(ctx context.Context) ([]RepoStatusView, string, error) {
	mgr := a.RepoManager()
	root, source := a.RepoRoot()
	paths, err := gitrepo.DiscoverContext(ctx, root, gitrepo.ScanOptionsFrom(a.Cfg.Repo.Scan))
	if err != nil {
		return nil, root, err
	}
	entries := make([]gitrepo.IndexEntry, 0, len(paths))
	out := make([]RepoStatusView, 0, len(paths))
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, root, err
		}
		st, _ := mgr.Inspect(ctx, p)
		entries = append(entries, gitrepo.IndexEntry{Path: p, Slug: st.Slug, Branch: st.Branch, DefaultBranch: st.DefaultBranch, Worktree: st.Worktree})
		out = append(out, RepoStatusView{RepoStatus: st, RootSource: source})
	}
	if err := ctx.Err(); err != nil {
		return nil, root, err
	}
	if a.DryRun {
		return out, root, nil
	}
	if err := gitrepo.SaveIndex(ctx, root, entries); err != nil {
		return nil, root, err
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
