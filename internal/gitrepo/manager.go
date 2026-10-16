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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

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
	// IsDryRun 用来在"命令还没发出去"之前就跳过准备动作（例如写 known_hosts）：
	// 干跑必须一个字节都不写，不能等到看 Run 的结果才发现。
	IsDryRun() bool
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
	// CloneURL 是本次（或"若有需要"）要 clone 的地址；SSH 传输前的 known_hosts
	// 准备需要它（见 hostkey.go）。
	CloneURL string
	// Existed 表示目标目录在本次操作前是否已存在。
	Existed bool
}

// Outcome 是 Apply 的结果。
type Outcome struct {
	Plan    Plan
	Status  string // cloned | updated | up-to-date | skipped | failed | planned
	Message string
	// Note 是本次执行里值得让用户知道、但不影响成败的副作用说明
	// （目前只有"自动把某 SSH 主机公钥写进 known_hosts"）。
	Note string
}

// Manager 编排仓库操作。
type Manager struct {
	Cfg    *config.Config
	Runner Runner
	// HomeDir 是 ~ 的基准（known_hosts 的查找与写入）；空则用 fsx.HomeDir()。
	HomeDir string
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

	plan := Plan{Raw: raw, Ref: ref, Path: path, CloneURL: cloneURL}

	switch {
	case !fsx.Exists(path):
		plan.Action = ActionClone
		plan.Reason = "目标不存在，将克隆"
		plan.Commands = []cmdx.Command{m.cloneCommand(cloneURL, path, ref.Ref, depth)}

	case isRepository(path):
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
		// 干净机器上第一次连某个 SSH 主机时，ssh 会问"是否信任该主机"并在
		// /dev/tty 上等回答；cmdx 把子进程 stderr 收进缓冲区，于是用户看到的
		// 就是"没有输出的卡住"。所以先按 SSH 约定把主机公钥准备好（hostkey.go）。
		note, env := m.prepareSSHFor(ctx, "", plan.CloneURL)
		out.Note = note
		setCommandEnv(&plan.Commands[0], env)

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

	// 已有仓库也可能遇到"这台机器从没连过该主机"（目录是拷贝/同步过来的），
	// fetch 前同样准备一次；`fetch --all` 的其它远端不在覆盖范围内，见 design §9.3。
	note, env := m.prepareSSHFor(ctx, plan.Path, "origin")
	out.Note = note
	for i := range plan.Commands {
		setCommandEnv(&plan.Commands[i], env)
	}

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

	// 不猜 main/master，也不把远端主分支合并进功能分支；没有 upstream 时明确跳过。
	branch, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", plan.Path, "symbolic-ref", "--quiet", "--short", "HEAD"}})
	if err != nil || strings.TrimSpace(branch.Stdout) == "" {
		out.Status = "skipped"
		out.Message = "已 fetch；当前为 detached HEAD，未执行 pull"
		return out, nil
	}
	upstream, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", plan.Path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"}})
	if err != nil || strings.TrimSpace(upstream.Stdout) == "" {
		out.Status = "skipped"
		out.Message = "已 fetch；当前分支没有 upstream，未执行 pull"
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
	Path          string `json:"path"`
	Slug          string `json:"slug,omitempty"`
	Branch        string `json:"branch,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Worktree      bool   `json:"worktree"`
	Action        string `json:"action,omitempty"`
	Message       string `json:"message,omitempty"`
}

// ScanOptions 控制"按站点层数枚举"的仓库发现（`bs repo scan` / `bs repo sync`）。
type ScanOptions struct {
	// Depth 是普通站点下 `*` 的个数：Depth=2 表示 <site>/*/*/.git。
	Depth int
	// SubgroupHosts 是支持多级子组的站点，会从 Depth 逐层探测到 SubgroupDepth。
	SubgroupHosts []string
	// SubgroupDepth 是上述站点的最大 `*` 个数（须 >= Depth）。
	SubgroupDepth int
}

// DefaultScanOptions 是内置默认：普通站点 2 层（<host>/<owner>/<repo>），
// gitlab.com 这类支持子组的站点最多 4 层（<host>/<g>/<sg>/<ssg>/<repo>）。
func DefaultScanOptions() ScanOptions {
	return ScanOptions{Depth: 2, SubgroupHosts: []string{"gitlab.com"}, SubgroupDepth: 4}
}

// ScanOptionsFrom 把 `repo.scan` 配置映射成扫描参数。
func ScanOptionsFrom(scan config.ScanConfig) ScanOptions {
	return ScanOptions{Depth: scan.Depth, SubgroupHosts: scan.SubgroupHosts, SubgroupDepth: scan.SubgroupDepth}
}

// normalized 补齐未设置的字段（配置零值时仍然可用）。
func (o ScanOptions) normalized() ScanOptions {
	def := DefaultScanOptions()
	if o.Depth < 1 {
		o.Depth = def.Depth
	}
	if o.SubgroupHosts == nil {
		o.SubgroupHosts = def.SubgroupHosts
	}
	if o.SubgroupDepth < o.Depth {
		o.SubgroupDepth = o.Depth
	}
	return o
}

func (o ScanOptions) isSubgroupHost(name string) bool {
	for _, h := range o.SubgroupHosts {
		if strings.EqualFold(strings.TrimSpace(h), name) {
			return true
		}
	}
	return false
}

// Discover 按站点层数枚举 root 下的主检出。opts 为零值时用 DefaultScanOptions。
func Discover(root string, opts ScanOptions) ([]string, error) {
	return DiscoverContext(context.Background(), root, opts)
}

// DiscoverContext 允许取消大目录扫描；读取失败时返回错误，调用方据此保留旧索引。
//
// 枚举规则（即"按站点 glob"）：
//   - root 的一级子目录视为站点；
//   - 普通站点只认 <site>/*…/*/.git（`*` 个数 = opts.Depth）；
//   - opts.SubgroupHosts 里的站点从 Depth 逐层探测到 SubgroupDepth；
//   - 只有 `.git` **目录**才算主检出：worktree 与 submodule 的 `.git` 是 gitdir
//     文件，它们属于另一个仓库，既不记录也不再深入（worktree 分支经常被清理，
//     submodule 以父仓库为准）；
//   - 因此嵌套在某个检出里的候选取不到，天然完成了"最外层优先"的去重。
func DiscoverContext(ctx context.Context, root string, opts ScanOptions) ([]string, error) {
	opts = opts.normalized()
	if !fsx.IsDir(root) {
		// 用 %w 包住哨兵错误，调用方（CLI）才能把它映射成"前置条件不满足"而不是一般失败。
		return nil, fmt.Errorf("%w: %s", ErrNoRoot, root)
	}
	sites, err := dirsAtLevel(ctx, root, 1)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, site := range sites {
		if isCheckout(site) {
			// 一级目录本身就是个检出（不是站点）：按布局它不是仓库，也不能当站点往下扫，
			// 否则会把它内部的东西当成 layer-2/3/4 的候选。
			continue
		}
		depths := []int{opts.Depth}
		if opts.isSubgroupHost(filepath.Base(site)) {
			for d := opts.Depth + 1; d <= opts.SubgroupDepth; d++ {
				depths = append(depths, d)
			}
		}
		for _, depth := range depths {
			dirs, err := dirsAtLevel(ctx, site, depth)
			if err != nil {
				return nil, err
			}
			for _, dir := range dirs {
				if isPrimaryCheckout(dir) {
					found = append(found, dir)
				}
			}
		}
	}
	sort.Strings(found)
	return found, nil
}

// dirsAtLevel 返回 dir 下第 n 层（n >= 1）的目录。
//
// 中间层遇到检出（`.git` 目录或 gitdir 文件）就不再深入：检出内部的东西
// ——子模块、vendor 里的独立 clone、agent 工具留下的 worktree——
// 都不属于"用户管理的顶层仓库"这个集合，扫它们既慢又只会产生噪声。
// 不跟随目录符号链接（DirEntry.IsDir 对符号链接为 false），与既有语义一致。
func dirsAtLevel(ctx context.Context, dir string, n int) ([]string, error) {
	current := []string{dir}
	for level := 1; level <= n; level++ {
		var next []string
		for _, d := range current {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(d)
			if err != nil {
				return nil, fmt.Errorf("scan %s: %w", d, err)
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				if name == ".git" || name == ".bootstrap" {
					continue
				}
				path := filepath.Join(d, name)
				if level == n {
					next = append(next, path)
					continue
				}
				if !isCheckout(path) {
					next = append(next, path)
				}
			}
		}
		current = next
	}
	return current, nil
}

// isCheckout 判断目录是不是一个 git 检出（主检出的 `.git` 目录，或 worktree/submodule 的 gitdir 文件）。
func isCheckout(path string) bool {
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}

// isPrimaryCheckout 只认 `.git` 是目录的检出：worktree/submodule 用的是 gitdir 文件。
func isPrimaryCheckout(path string) bool {
	return fsx.IsDir(filepath.Join(path, ".git"))
}

// Inspect 读取一个本地仓库的分支与远端。
//
// 这里刻意不跑 `git status`：`bs repo scan` 的索引不存脏状态，而大仓库上的
// `git status --porcelain` 是整次扫描里最贵的一步。真正需要脏状态的是 pull 前的
// 检查（applyPull 自己做）。
func (m *Manager) Inspect(ctx context.Context, path string) (RepoStatus, error) {
	st := RepoStatus{Path: path}
	if info, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		st.Worktree = info.Mode().IsRegular() && fsx.Exists(filepath.Join(repositoryGitDir(path), "commondir"))
	}
	if res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"}}); err == nil {
		st.DefaultBranch = strings.TrimPrefix(strings.TrimSpace(res.Stdout), "origin/")
	}
	if res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "rev-parse", "--abbrev-ref", "HEAD"}}); err == nil {
		st.Branch = strings.TrimSpace(res.Stdout)
	}
	if res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "remote", "get-url", "origin"}}); err == nil {
		remote := strings.TrimSpace(res.Stdout)
		if ref, perr := parseRemote(remote); perr == nil {
			st.Slug = ref.Slug()
		}
	}
	return st, nil
}

// Sync 并发更新 root 下的所有仓库。
func (m *Manager) Sync(ctx context.Context, root string, jobs int, opts Options) ([]RepoStatus, error) {
	paths, err := DiscoverContext(ctx, root, ScanOptionsFrom(m.Cfg.Repo.Scan))
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
	var locks sync.Map
	for i, p := range paths {
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				return gctx.Err()
			}
			// 同一仓库的 worktree 共享 refs 和 stash，避免并发 fetch/stash 互相干扰。
			value, _ := locks.LoadOrStore(commonGitDir(p), &sync.Mutex{})
			lock := value.(*sync.Mutex)
			lock.Lock()
			defer lock.Unlock()
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
	// fetch --all 和 pull 可能访问不同远端，因此必须逐个检查，不能只检查 origin。
	remotes, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"-C", path, "remote", "-v"}})
	if err != nil {
		st.Action = "failed"
		st.Message = fmt.Sprintf("read remotes: %v", err)
		return st
	}
	for _, line := range strings.Split(remotes.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ref, err := parseRemote(fields[1])
		allowed := false
		if err == nil {
			for _, host := range m.Cfg.Repo.Hosts {
				if strings.EqualFold(host, ref.Host) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			st.Action = "skipped"
			st.Message = "远端不在 repo.hosts 中，未执行网络同步（可调整 hosts 后重试）"
			return st
		}
	}
	plan := Plan{Path: path, Action: ActionPull, Commands: m.pullCommands(path), Reason: "批量同步"}
	out, err := m.Apply(ctx, plan, opts)
	switch {
	case err != nil:
		st.Action = "failed"
		st.Message = out.Message
	case out.Status == "planned":
		st.Action = "planned"
		st.Message = out.Message
	case out.Status == "skipped":
		st.Action = "skipped"
		st.Message = out.Message
	default:
		st.Action = "pulled"
		st.Message = out.Message
	}
	// 首次信任某台 SSH 主机的提示并进结果行：批量同步里也值得让用户看见。
	if out.Note != "" {
		if st.Message == "" {
			st.Message = out.Note
		} else {
			st.Message += "；" + out.Note
		}
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

// isRepository 判断目录是不是一个 git 仓库（主检出的 `.git` 目录，或 worktree/submodule
// 的 gitdir 文件）。`bs repo get` 需要认识后者：用户完全可能指定一个 worktree 目录。
func isRepository(path string) bool {
	marker := filepath.Join(path, ".git")
	if fsx.IsDir(marker) {
		return true
	}
	info, err := os.Stat(marker)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return false
	}
	data, err := os.ReadFile(marker)
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir: ")
}

func parseRemote(raw string) (giturl.Ref, error) {
	prefix, _, colon := strings.Cut(raw, ":")
	if !strings.Contains(raw, "://") && (!colon || strings.ContainsAny(prefix, `/\\`) || !strings.ContainsAny(prefix, ".@")) {
		return giturl.Ref{}, fmt.Errorf("not a network repository URL")
	}
	return giturl.Parse(raw, "")
}

func repositoryGitDir(repo string) string {
	dir := filepath.Join(repo, ".git")
	if data, err := os.ReadFile(dir); err == nil && strings.HasPrefix(string(data), "gitdir: ") {
		dir = strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir: "))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(repo, dir)
		}
	}
	return filepath.Clean(dir)
}

func commonGitDir(repo string) string {
	dir := repositoryGitDir(repo)
	if data, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil {
		common := strings.TrimSpace(string(data))
		if filepath.IsAbs(common) {
			dir = common
		} else {
			dir = filepath.Join(dir, common)
		}
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return filepath.Clean(dir)
}
