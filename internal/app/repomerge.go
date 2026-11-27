package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kingfs/Bootstrap/internal/gitrepo"
	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// RepoMergeOptions 控制 `bs repo merge` 的输入。
type RepoMergeOptions struct {
	// ToRoot 是主仓库根目录。
	ToRoot string
	// FromRoots 是副仓库根目录（可多个）。
	FromRoots []string
	// Depth 是仓库相对根目录的层级数（2 = <group>/<repo>）。
	Depth int
	// Force 允许删除有未提交改动或唯一本地引用的旧仓库。
	Force bool
	// KeepBackup 为真时把较旧仓库移到 .bootstrap/merge-backup 而不是删除。
	KeepBackup bool
	// PruneEmpty 为真时迁移后清理副根目录下的空目录。
	PruneEmpty bool
}

// RepoMergePlan 只读地计算合并计划。
//
// 规划阶段需要运行只读 git 命令（log/status/stash/worktree/for-each-ref）来比较
// 提交时间与检查工作区，因此这里临时把 Runner 的干跑关掉；所有写文件系统的动作
// 都在 RepoMergeApply 里，并由 a.DryRun 把关。
func (a *App) RepoMergePlan(ctx context.Context, opts RepoMergeOptions) (gitrepo.MergeReport, error) {
	if opts.Depth < 1 {
		return gitrepo.MergeReport{}, fmt.Errorf("depth must be >= 1")
	}
	toRoot, err := a.resolveMergeRoot(opts.ToRoot, false)
	if err != nil {
		return gitrepo.MergeReport{}, err
	}
	// 主根目录本身是检出时，目标路径会落进一个仓库内部；这几乎一定是传错了。
	if fsx.Exists(filepath.Join(toRoot, ".git")) {
		return gitrepo.MergeReport{}, fmt.Errorf("%s is itself a git checkout; pass its parent directory", toRoot)
	}
	fromRoots := make([]string, 0, len(opts.FromRoots))
	for _, raw := range opts.FromRoots {
		root, err := a.resolveMergeRoot(raw, true)
		if err != nil {
			return gitrepo.MergeReport{}, err
		}
		fromRoots = append(fromRoots, root)
	}
	if err := validateMergeRoots(toRoot, fromRoots); err != nil {
		return gitrepo.MergeReport{}, err
	}

	mgr := a.RepoManager()
	prev := a.Runner.DryRun
	a.Runner.DryRun = false // 只读 git 命令必须真跑，否则拿不到提交时间
	defer func() { a.Runner.DryRun = prev }()

	plans, err := mgr.PlanMerge(ctx, gitrepo.MergeOptions{
		ToRoot:     toRoot,
		FromRoots:  fromRoots,
		Depth:      opts.Depth,
		Force:      opts.Force,
		KeepBackup: opts.KeepBackup,
		PruneEmpty: opts.PruneEmpty,
		DryRun:     a.DryRun,
	})
	if err != nil {
		return gitrepo.MergeReport{}, err
	}
	entries := make([]gitrepo.MergeResult, 0, len(plans))
	for _, plan := range plans {
		status := "planned"
		switch plan.Action {
		case gitrepo.MergeActionSkip:
			status = "skipped"
		case gitrepo.MergeActionConflict:
			status = "conflict"
		}
		entries = append(entries, gitrepo.MergeResult{Plan: plan, Status: status, Message: plan.Reason})
	}
	return gitrepo.MergeReport{
		ToRoot:    toRoot,
		FromRoots: fromRoots,
		Depth:     opts.Depth,
		DryRun:    a.DryRun,
		Entries:   entries,
	}, nil
}

// RepoMergeApply 执行计划并更新索引。
//
// 索引更新是"尽力而为"：仓库已经移动/删除，索引写失败只记警告，不回滚文件操作。
func (a *App) RepoMergeApply(ctx context.Context, plan gitrepo.MergeReport, opts RepoMergeOptions) (gitrepo.MergeReport, error) {
	// 双保险：即使调用方忘了判断 --dry-run，也绝不移动/删除任何东西。
	if a.DryRun {
		return plan, nil
	}
	mgr := a.RepoManager()
	prev := a.Runner.DryRun
	a.Runner.DryRun = false // ApplyMerge 里的安全复查也是只读 git 命令
	defer func() { a.Runner.DryRun = prev }()

	plans := make([]gitrepo.MergePlan, 0, len(plan.Entries))
	for _, e := range plan.Entries {
		plans = append(plans, e.Plan)
	}
	results, err := mgr.ApplyMerge(ctx, plans, gitrepo.MergeOptions{
		ToRoot:     plan.ToRoot,
		FromRoots:  plan.FromRoots,
		Depth:      plan.Depth,
		Force:      opts.Force,
		KeepBackup: opts.KeepBackup,
		PruneEmpty: opts.PruneEmpty,
		DryRun:     false,
	})

	report := plan
	report.DryRun = false
	report.Entries = results
	if opts.PruneEmpty {
		for _, root := range plan.FromRoots {
			n, perr := gitrepo.PruneEmptyDirs(ctx, root)
			report.Pruned += n
			if perr != nil {
				report.IndexWarnings = append(report.IndexWarnings,
					fmt.Sprintf("清理空目录 %s 失败：%v", root, perr))
			}
		}
	}
	a.updateMergeIndexes(ctx, &report)
	return report, err
}

// resolveMergeRoot 展开 ~、转成绝对路径，并尽量解析符号链接。
//
// mustExist 为真时（--from）路径必须已存在；--to 允许不存在（首次合并到新根目录），
// 此时解析最近的存在祖先，避免符号链接父目录让重叠检查失效。
func (a *App) resolveMergeRoot(raw string, mustExist bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty repository root")
	}
	p := fsx.ExpandPath(raw, a.Paths.HomeDir)
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", raw, err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	switch {
	case err == nil:
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory", abs)
		}
		if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
			abs = resolved
		}
	case errors.Is(err, os.ErrNotExist):
		if mustExist {
			return "", fmt.Errorf("%s does not exist", abs)
		}
		if resolved, rerr := resolveExistingAncestor(abs); rerr == nil {
			abs = resolved
		}
	default:
		return "", fmt.Errorf("stat %s: %w", abs, err)
	}
	return abs, nil
}

func resolveExistingAncestor(p string) (string, error) {
	var tail []string
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// validateMergeRoots 拒绝重叠的根目录：主根与副根、副根与副根都不能互相包含。
//
// 重叠会让同一个仓库被发现两次，也让"目标路径"落进来源树里，移动语义不再安全。
func validateMergeRoots(to string, from []string) error {
	if len(from) == 0 {
		return errors.New("at least one --from is required")
	}
	for i, f := range from {
		if f == to {
			return fmt.Errorf("--to and --from are the same directory: %s", to)
		}
		if isSubpath(to, f) || isSubpath(f, to) {
			return fmt.Errorf("--to %s and --from %s overlap; choose roots that do not contain each other", to, f)
		}
		for j := 0; j < i; j++ {
			if f == from[j] {
				return fmt.Errorf("duplicate --from root: %s", f)
			}
			if isSubpath(from[j], f) || isSubpath(f, from[j]) {
				return fmt.Errorf("--from roots %s and %s overlap", from[j], f)
			}
		}
	}
	return nil
}

func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// updateMergeIndexes 把迁移结果同步到主/副根目录下已存在的索引。
//
// 主索引不存在时，只要本次有仓库迁移进来就创建；副索引只在已存在时清理，
// 避免为一个正在腾空的目录凭空建库。
func (a *App) updateMergeIndexes(ctx context.Context, report *gitrepo.MergeReport) {
	if report.DryRun {
		return
	}
	mgr := a.RepoManager()

	primary, err := gitrepo.LoadIndex(ctx, report.ToRoot)
	if err != nil && !errors.Is(err, gitrepo.ErrNoIndex) {
		report.IndexWarnings = append(report.IndexWarnings, fmt.Sprintf("读取主索引失败：%v", err))
	} else {
		entries := make(map[string]gitrepo.IndexEntry, len(primary))
		for _, e := range primary {
			entries[filepath.Clean(e.Path)] = e
		}
		changed := false
		for _, r := range report.Entries {
			switch r.Status {
			case "moved", "replaced":
				st, _ := mgr.Inspect(ctx, r.Plan.TargetPath)
				entries[filepath.Clean(r.Plan.TargetPath)] = gitrepo.IndexEntry{
					Path:          r.Plan.TargetPath,
					Slug:          st.Slug,
					Branch:        st.Branch,
					DefaultBranch: st.DefaultBranch,
					Worktree:      st.Worktree,
				}
				delete(entries, filepath.Clean(r.Plan.RepoPath))
				changed = true
			case "deleted":
				// 副路径旧仓库被删；主路径条目保持不变。
			}
		}
		if changed {
			out := make([]gitrepo.IndexEntry, 0, len(entries))
			for _, e := range entries {
				out = append(out, e)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
			if serr := gitrepo.SaveIndex(ctx, report.ToRoot, out); serr != nil {
				report.IndexWarnings = append(report.IndexWarnings, fmt.Sprintf("更新主索引失败：%v", serr))
			}
		}
	}

	for _, fromRoot := range report.FromRoots {
		idx, ierr := gitrepo.LoadIndex(ctx, fromRoot)
		if errors.Is(ierr, gitrepo.ErrNoIndex) {
			continue
		}
		if ierr != nil {
			report.IndexWarnings = append(report.IndexWarnings,
				fmt.Sprintf("读取副索引 %s 失败：%v", fromRoot, ierr))
			continue
		}
		remove := map[string]bool{}
		for _, r := range report.Entries {
			if r.Plan.FromRoot != fromRoot {
				continue
			}
			switch r.Status {
			case "moved", "replaced", "deleted":
				remove[filepath.Clean(r.Plan.RepoPath)] = true
			}
		}
		if len(remove) == 0 {
			continue
		}
		kept := make([]gitrepo.IndexEntry, 0, len(idx))
		for _, e := range idx {
			if !remove[filepath.Clean(e.Path)] {
				kept = append(kept, e)
			}
		}
		if serr := gitrepo.SaveIndex(ctx, fromRoot, kept); serr != nil {
			report.IndexWarnings = append(report.IndexWarnings,
				fmt.Sprintf("更新副索引 %s 失败：%v", fromRoot, serr))
		}
	}
}
