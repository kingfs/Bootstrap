package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// MergeAction 是对一个副根目录仓库将要执行的动作。
type MergeAction string

const (
	// MergeActionMove 表示目标不存在（或为空目录），直接把仓库搬过去。
	MergeActionMove MergeAction = "move"
	// MergeActionReplace 表示两边冲突且副路径更新：备份/删除主路径旧仓库，再把副路径仓库搬过去。
	MergeActionReplace MergeAction = "replace"
	// MergeActionDelete 表示两边冲突且主路径更新：删除副路径旧仓库。
	MergeActionDelete MergeAction = "delete"
	// MergeActionSkip 表示目标不是可安全覆盖的目录（文件、符号链接、非空非仓库目录）。
	MergeActionSkip MergeAction = "skip"
	// MergeActionConflict 表示需要人工处理（脏工作区、唯一提交、linked worktree 等）。
	MergeActionConflict MergeAction = "conflict"
)

// MergeOptions 控制一次多根目录合并。
type MergeOptions struct {
	// ToRoot 是主仓库根目录（合并后所有仓库都在它下面）。
	ToRoot string
	// FromRoots 是副仓库根目录；每个根目录下的相对布局必须与 ToRoot 一致。
	FromRoots []string
	// Depth 是仓库相对根目录的层级数：2 表示 <root>/<group>/<repo>。
	Depth int
	// Force 允许删除有未提交改动或较新一侧没有的本地引用的旧仓库。
	// linked worktree 永远不删：必须先 git worktree remove。
	Force bool
	// KeepBackup 为真时不真正删除较旧仓库，而是移到 .bootstrap/merge-backup 下。
	KeepBackup bool
	// PruneEmpty 为真时在迁移后清理副根目录下的空目录。
	PruneEmpty bool
	// DryRun 为真时只计算计划，不移动/删除任何东西。
	DryRun bool
}

// MergePlan 是单个仓库的迁移计划。
type MergePlan struct {
	FromRoot   string      `json:"from_root"`
	RepoPath   string      `json:"repo_path"`
	RelPath    string      `json:"rel_path"`
	TargetPath string      `json:"target_path"`
	Action     MergeAction `json:"action"`
	Reason     string      `json:"reason,omitempty"`
	// 冲突双方的 HEAD 提交时间（Unix 秒；0 表示空仓库）与提交哈希。
	FromHeadTime int64  `json:"from_head_time,omitempty"`
	ToHeadTime   int64  `json:"to_head_time,omitempty"`
	FromHead     string `json:"from_head,omitempty"`
	ToHead       string `json:"to_head,omitempty"`
	// 冲突双方是否有未提交改动（含 untracked 与 stash）。
	FromDirty bool `json:"from_dirty,omitempty"`
	ToDirty   bool `json:"to_dirty,omitempty"`
}

// MergeResult 是 ApplyMerge 对单个仓库的结果。
type MergeResult struct {
	Plan    MergePlan `json:"plan"`
	Status  string    `json:"status"`
	Message string    `json:"message,omitempty"`
}

// MergeReport 是一次合并的完整结果（计划或执行后）。
type MergeReport struct {
	ToRoot        string        `json:"to_root"`
	FromRoots     []string      `json:"from_roots"`
	Depth         int           `json:"depth"`
	DryRun        bool          `json:"dry_run"`
	Entries       []MergeResult `json:"entries"`
	Pruned        int           `json:"pruned,omitempty"`
	IndexWarnings []string      `json:"index_warnings,omitempty"`
}

// Counts 按计划动作统计条目数（执行失败也计入动作数，另由 failed 单独统计）。
func (r MergeReport) Counts() (move, replace, del, skip, conflict, failed int) {
	for _, e := range r.Entries {
		switch e.Plan.Action {
		case MergeActionMove:
			move++
		case MergeActionReplace:
			replace++
		case MergeActionDelete:
			del++
		case MergeActionSkip:
			skip++
		case MergeActionConflict:
			conflict++
		}
		if e.Status == "failed" {
			failed++
		}
	}
	return move, replace, del, skip, conflict, failed
}

// DestructiveCount 是需要用户确认的动作数（迁移、替换、删除）。
func (r MergeReport) DestructiveCount() int {
	move, replace, del, _, _, _ := r.Counts()
	return move + replace + del
}

// MoveCount 是会把副路径仓库搬走的动作数（move + replace）。
func (r MergeReport) MoveCount() int {
	move, replace, _, _, _, _ := r.Counts()
	return move + replace
}

// DeleteCount 是删除较旧仓库的动作数（replace + delete）。
func (r MergeReport) DeleteCount() int {
	_, replace, del, _, _, _ := r.Counts()
	return replace + del
}

// UnresolvedCount 是需要人工处理的动作数（skip + conflict + failed）。
func (r MergeReport) UnresolvedCount() int {
	_, _, _, skip, conflict, failed := r.Counts()
	return skip + conflict + failed
}

// AppliedCount 是真正完成了的动作数（moved + replaced + deleted）。
func (r MergeReport) AppliedCount() int {
	n := 0
	for _, e := range r.Entries {
		switch e.Status {
		case "moved", "replaced", "deleted":
			n++
		}
	}
	return n
}

// DiscoverAtDepth 在 root 下按固定层数枚举主检出。
//
// 与 DiscoverContext 的区别是：这里把 root 直接当作"仓库根目录"，只认
// root/*…/*/.git（`*` 个数 = depth），不再假设第一层是站点。这样既支持
// `--from ~/src --depth 3`（host/owner/repo），也支持
// `--from ~/repos/github.com --depth 2`（owner/repo）。
//
// 规则与 scan 保持一致：中间层遇到检出就不再深入（子模块、vendor 里的独立 clone、
// worktree 都不会被当成顶层仓库），只认 `.git` 目录（worktree/submodule 的 gitdir
// 文件不算），不跟随目录符号链接。
func DiscoverAtDepth(ctx context.Context, root string, depth int) ([]string, error) {
	if depth < 1 {
		return nil, fmt.Errorf("depth must be >= 1")
	}
	if !fsx.IsDir(root) {
		return nil, fmt.Errorf("%w: %s", ErrNoRoot, root)
	}
	if isCheckout(root) {
		return nil, fmt.Errorf("repository root %s is itself a git checkout; pass its parent directory", root)
	}
	dirs, err := dirsAtLevel(ctx, root, depth)
	if err != nil {
		return nil, err
	}
	found := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if isPrimaryCheckout(dir) {
			found = append(found, dir)
		}
	}
	sort.Strings(found)
	return found, nil
}

// PlanMerge 计算一次合并计划，不移动、不删除任何东西。
//
// 规划阶段会运行只读 git 命令（log/status/stash/worktree/for-each-ref），
// 因此调用方即使处于 --dry-run 也不能把 Runner 设成干跑，否则拿不到提交时间。
// 所有写文件系统的动作都在 ApplyMerge 里，并由 MergeOptions.DryRun 把关。
func (m *Manager) PlanMerge(ctx context.Context, opts MergeOptions) ([]MergePlan, error) {
	if opts.Depth < 1 {
		return nil, fmt.Errorf("depth must be >= 1")
	}
	if strings.TrimSpace(opts.ToRoot) == "" {
		return nil, errors.New("--to is required")
	}
	if len(opts.FromRoots) == 0 {
		return nil, errors.New("at least one --from is required")
	}

	var plans []MergePlan
	// 同一目标被两个来源占用时，后一个必须人工处理，否则会互相覆盖。
	seenTargets := map[string]string{}
	for _, fromRoot := range opts.FromRoots {
		repos, err := DiscoverAtDepth(ctx, fromRoot, opts.Depth)
		if err != nil {
			return nil, err
		}
		for _, repoPath := range repos {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			rel, err := filepath.Rel(fromRoot, repoPath)
			if err != nil {
				return nil, fmt.Errorf("compute relative path for %s: %w", repoPath, err)
			}
			if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("repository %s is not under root %s", repoPath, fromRoot)
			}
			plan := MergePlan{
				FromRoot:   fromRoot,
				RepoPath:   repoPath,
				RelPath:    rel,
				TargetPath: filepath.Join(opts.ToRoot, rel),
			}
			if prev, ok := seenTargets[plan.TargetPath]; ok {
				plan.Action = MergeActionConflict
				plan.Reason = fmt.Sprintf("目标 %s 已被 %s 占用，需人工处理", plan.TargetPath, prev)
				plans = append(plans, plan)
				continue
			}
			seenTargets[plan.TargetPath] = repoPath
			if err := m.planMergeOne(ctx, &plan, opts); err != nil {
				return nil, err
			}
			plans = append(plans, plan)
		}
	}
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].FromRoot != plans[j].FromRoot {
			return plans[i].FromRoot < plans[j].FromRoot
		}
		return plans[i].RelPath < plans[j].RelPath
	})
	return plans, nil
}

func (m *Manager) planMergeOne(ctx context.Context, plan *MergePlan, opts MergeOptions) error {
	info, err := os.Lstat(plan.TargetPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if reason, ok, err := m.checkMover(ctx, plan.RepoPath); err != nil {
			return err
		} else if !ok {
			plan.Action = MergeActionConflict
			plan.Reason = reason
			return nil
		}
		plan.Action = MergeActionMove
		plan.Reason = "目标不存在，将迁移"
		return nil
	case err != nil:
		return fmt.Errorf("stat target %s: %w", plan.TargetPath, err)
	}

	// 符号链接/文件一律不覆盖：用户可能把仓库根目录的某个名字软链到别处。
	if info.Mode()&os.ModeSymlink != 0 {
		plan.Action = MergeActionSkip
		plan.Reason = "目标是符号链接，拒绝覆盖"
		return nil
	}
	if !info.IsDir() {
		plan.Action = MergeActionSkip
		plan.Reason = "目标已存在且不是目录，拒绝覆盖"
		return nil
	}
	if !isPrimaryCheckout(plan.TargetPath) {
		if isDirEmpty(plan.TargetPath) {
			if reason, ok, err := m.checkMover(ctx, plan.RepoPath); err != nil {
				return err
			} else if !ok {
				plan.Action = MergeActionConflict
				plan.Reason = reason
				return nil
			}
			plan.Action = MergeActionMove
			plan.Reason = "目标是空目录，将迁移进去"
			return nil
		}
		plan.Action = MergeActionSkip
		plan.Reason = "目标已存在且不是 git 仓库，为避免覆盖已跳过"
		return nil
	}

	// 两边都是主检出：比较新旧。同一个目录（符号链接/大小写不敏感文件系统）不处理。
	if same, _ := sameFile(plan.RepoPath, plan.TargetPath); same {
		plan.Action = MergeActionSkip
		plan.Reason = "源与目标是同一个目录，无需处理"
		return nil
	}
	fromTime, fromHead, err := m.headCommitTime(ctx, plan.RepoPath)
	if err != nil {
		plan.Action = MergeActionConflict
		plan.Reason = fmt.Sprintf("无法读取副路径仓库 HEAD：%v", err)
		return nil
	}
	toTime, toHead, err := m.headCommitTime(ctx, plan.TargetPath)
	if err != nil {
		plan.Action = MergeActionConflict
		plan.Reason = fmt.Sprintf("无法读取主路径仓库 HEAD：%v", err)
		return nil
	}
	plan.FromHeadTime, plan.FromHead = fromTime, fromHead
	plan.ToHeadTime, plan.ToHead = toTime, toHead

	// 先比 HEAD 提交时间；相同再比所有 ref 的最新提交时间；再相同保留主路径。
	newerIsFrom := fromTime > toTime
	if fromTime == toTime {
		newerIsFrom = m.latestCommitTime(ctx, plan.RepoPath) > m.latestCommitTime(ctx, plan.TargetPath)
	}
	older, newer := plan.TargetPath, plan.RepoPath
	if !newerIsFrom {
		older, newer = plan.RepoPath, plan.TargetPath
	}
	reason, ok, err := m.checkLoser(ctx, older, newer, opts.Force)
	if err != nil {
		return err
	}
	if !ok {
		plan.Action = MergeActionConflict
		plan.Reason = reason
		return nil
	}

	if newerIsFrom {
		// 副路径要搬进主路径：它自己也不能有 linked worktree。
		if reason, ok, err := m.checkMover(ctx, plan.RepoPath); err != nil {
			return err
		} else if !ok {
			plan.Action = MergeActionConflict
			plan.Reason = reason
			return nil
		}
		plan.Action = MergeActionReplace
		plan.Reason = fmt.Sprintf("副路径更新（HEAD %s > %s），将替换主路径",
			formatMergeTime(fromTime), formatMergeTime(toTime))
		return nil
	}
	plan.Action = MergeActionDelete
	plan.Reason = fmt.Sprintf("主路径更新（HEAD %s >= %s），将删除副路径",
		formatMergeTime(toTime), formatMergeTime(fromTime))
	return nil
}

// checkMover 检查"要搬走的仓库"是否有 linked worktree。
// 搬走主仓库会让 worktree 的 gitdir 链接失效，所以必须先人工处理。
func (m *Manager) checkMover(ctx context.Context, path string) (string, bool, error) {
	wt, err := m.linkedWorktrees(ctx, path)
	if err != nil {
		return "", false, fmt.Errorf("check %s worktrees: %w", path, err)
	}
	if wt {
		return fmt.Sprintf("%s 还有 linked worktree，先 `git worktree remove` 再迁移", path), false, nil
	}
	return "", true, nil
}

// checkLoser 检查"要被删除的较旧仓库"是否安全。
//
// 三道闸门：未提交改动/stash、linked worktree、较新一侧没有的本地引用。
// 前两道和第三道都只报告冲突，不自动删除；--force 只能覆盖第一、三道。
func (m *Manager) checkLoser(ctx context.Context, older, newer string, force bool) (string, bool, error) {
	dirty, err := m.repoDirty(ctx, older)
	if err != nil {
		return "", false, fmt.Errorf("check %s worktree: %w", older, err)
	}
	if dirty && !force {
		return fmt.Sprintf("%s 有未提交改动或 stash，未删除；确认可丢弃后加 --force", older), false, nil
	}
	if wt, err := m.linkedWorktrees(ctx, older); err != nil {
		return "", false, fmt.Errorf("check %s worktrees: %w", older, err)
	} else if wt {
		return fmt.Sprintf("%s 还有 linked worktree，先 `git worktree remove` 再处理", older), false, nil
	}
	missing, err := m.missingRefs(ctx, older, newer)
	if err != nil {
		return "", false, fmt.Errorf("check %s refs: %w", older, err)
	}
	if len(missing) > 0 && !force {
		return fmt.Sprintf("%s 有较新一侧没有的本地引用（%s），未删除；确认可丢弃后加 --force",
			older, summarizeRefs(missing)), false, nil
	}
	return "", true, nil
}

// ApplyMerge 执行合并计划。
//
// 所有文件系统写操作都在这里；DryRun 为真时只返回 planned 结果。
// 执行前会重新检查较旧仓库的安全状态，避免计划与执行之间发生改动。
func (m *Manager) ApplyMerge(ctx context.Context, plans []MergePlan, opts MergeOptions) ([]MergeResult, error) {
	results := make([]MergeResult, 0, len(plans))
	var errs []error
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		res := MergeResult{Plan: plan}
		switch plan.Action {
		case MergeActionSkip:
			res.Status = "skipped"
			res.Message = plan.Reason
		case MergeActionConflict:
			res.Status = "conflict"
			res.Message = plan.Reason
		case MergeActionMove:
			if opts.DryRun {
				res.Status = "planned"
				res.Message = "dry-run：未移动任何仓库"
				break
			}
			if err := m.applyMove(plan); err != nil {
				res.Status = "failed"
				res.Message = err.Error()
				errs = append(errs, err)
			} else {
				res.Status = "moved"
				res.Message = "已迁移"
			}
		case MergeActionReplace:
			if opts.DryRun {
				res.Status = "planned"
				res.Message = "dry-run：未替换任何仓库"
				break
			}
			backup, err := m.applyReplace(ctx, plan, opts)
			if err != nil {
				res.Status = "failed"
				res.Message = err.Error()
				errs = append(errs, err)
			} else {
				res.Status = "replaced"
				if opts.KeepBackup && backup != "" {
					res.Message = fmt.Sprintf("已替换；旧仓库备份在 %s", backup)
				} else {
					res.Message = "已替换"
				}
			}
		case MergeActionDelete:
			if opts.DryRun {
				res.Status = "planned"
				res.Message = "dry-run：未删除任何仓库"
				break
			}
			dest, err := m.applyDelete(ctx, plan, opts)
			if err != nil {
				res.Status = "failed"
				res.Message = err.Error()
				errs = append(errs, err)
			} else {
				res.Status = "deleted"
				if opts.KeepBackup && dest != "" {
					res.Message = fmt.Sprintf("已移到备份目录 %s", dest)
				} else {
					res.Message = "已删除较旧的副路径仓库"
				}
			}
		default:
			res.Status = "failed"
			res.Message = fmt.Sprintf("unknown action %q", plan.Action)
			errs = append(errs, fmt.Errorf("unknown action %q", plan.Action))
		}
		results = append(results, res)
	}
	return results, errors.Join(errs...)
}

func (m *Manager) applyMove(plan MergePlan) error {
	// 目标可能在计划之后才出现：重新检查，绝不覆盖。
	if info, err := os.Lstat(plan.TargetPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("target %s is a symlink; refusing to overwrite", plan.TargetPath)
		}
		if !info.IsDir() {
			return fmt.Errorf("target %s exists and is not a directory", plan.TargetPath)
		}
		if !isDirEmpty(plan.TargetPath) {
			return fmt.Errorf("target %s is not empty; refusing to overwrite", plan.TargetPath)
		}
		if err := os.Remove(plan.TargetPath); err != nil {
			return fmt.Errorf("remove empty target %s: %w", plan.TargetPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat target %s: %w", plan.TargetPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.TargetPath), 0o755); err != nil {
		return fmt.Errorf("create target parent %s: %w", filepath.Dir(plan.TargetPath), err)
	}
	if err := os.Rename(plan.RepoPath, plan.TargetPath); err != nil {
		return fmt.Errorf("move %s -> %s: %w", plan.RepoPath, plan.TargetPath, err)
	}
	return nil
}

func (m *Manager) applyReplace(ctx context.Context, plan MergePlan, opts MergeOptions) (string, error) {
	// 计划之后主路径旧仓库可能变脏，绝不能直接删。
	reason, ok, err := m.checkLoser(ctx, plan.TargetPath, plan.RepoPath, opts.Force)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New(reason)
	}
	if reason, ok, err := m.checkMover(ctx, plan.RepoPath); err != nil {
		return "", err
	} else if !ok {
		return "", errors.New(reason)
	}

	backup := mergeBackupPath(opts.ToRoot, plan.RelPath)
	if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
		return "", fmt.Errorf("create backup parent %s: %w", filepath.Dir(backup), err)
	}
	if err := os.Rename(plan.TargetPath, backup); err != nil {
		return "", fmt.Errorf("backup target %s -> %s: %w", plan.TargetPath, backup, err)
	}
	if err := os.Rename(plan.RepoPath, plan.TargetPath); err != nil {
		// 回滚：把主路径旧仓库放回去，避免"删了旧的、新的又没进来"。
		if rerr := os.Rename(backup, plan.TargetPath); rerr != nil {
			return "", fmt.Errorf("move %s -> %s: %w; rollback failed: %w (backup at %s)",
				plan.RepoPath, plan.TargetPath, err, rerr, backup)
		}
		return "", fmt.Errorf("move %s -> %s: %w", plan.RepoPath, plan.TargetPath, err)
	}
	if !opts.KeepBackup {
		if err := os.RemoveAll(backup); err != nil {
			return "", fmt.Errorf("remove backup %s: %w", backup, err)
		}
		return "", nil
	}
	return backup, nil
}

func (m *Manager) applyDelete(ctx context.Context, plan MergePlan, opts MergeOptions) (string, error) {
	reason, ok, err := m.checkLoser(ctx, plan.RepoPath, plan.TargetPath, opts.Force)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New(reason)
	}

	// 先 rename 到同一文件系统的 trash，再删。这样即使删除中途失败，
	// 仓库也还完整地留在 trash 里，而不是被 RemoveAll 删掉一半。
	// --keep-backup 时改放 merge-backup 并保留，语义与 replace 一致。
	dest := mergeTrashPath(plan.FromRoot, plan.RelPath)
	if opts.KeepBackup {
		dest = mergeBackupPath(plan.FromRoot, plan.RelPath)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create trash parent %s: %w", filepath.Dir(dest), err)
	}
	if err := os.Rename(plan.RepoPath, dest); err != nil {
		return "", fmt.Errorf("move %s to trash %s: %w", plan.RepoPath, dest, err)
	}
	if opts.KeepBackup {
		return dest, nil
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", fmt.Errorf("delete %s (moved to %s): %w", plan.RepoPath, dest, err)
	}
	return "", nil
}

// PruneEmptyDirs 删除 root 下的空目录，保留 root 本身以及 .git/.bootstrap。
//
// 只删空目录（os.Remove 不会删非空目录），因此不会碰到仓库内容。
func PruneEmptyDirs(ctx context.Context, root string) (int, error) {
	return pruneEmptyDirs(ctx, root)
}

func pruneEmptyDirs(ctx context.Context, root string) (int, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == ".git" || name == ".bootstrap" {
			return filepath.SkipDir
		}
		if path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	// 从最深的目录开始删，父目录才可能跟着变空。
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	removed := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		if len(entries) != 0 {
			continue
		}
		if err := os.Remove(dir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func mergeBackupPath(root, rel string) string {
	return filepath.Join(root, ".bootstrap", "merge-backup", mergeStamp(), rel)
}

func mergeTrashPath(root, rel string) string {
	return filepath.Join(root, ".bootstrap", "merge-trash", mergeStamp(), rel)
}

func mergeStamp() string {
	// 带纳秒：同一秒内跑两次（或 --keep-backup 保留上一次的备份）时，
	// 备份/回收站目录不会撞名。
	return time.Now().UTC().Format("20060102T150405.000000000")
}

func formatMergeTime(sec int64) string {
	if sec <= 0 {
		return "无提交"
	}
	return time.Unix(sec, 0).Local().Format("2006-01-02 15:04:05")
}

func summarizeRefs(refs []string) string {
	const max = 3
	if len(refs) <= max {
		return strings.Join(refs, ", ")
	}
	return strings.Join(refs[:max], ", ") + fmt.Sprintf(" 等 %d 个", len(refs))
}

// headCommitTime 返回 HEAD 的提交时间与哈希；空仓库返回 0 且不报错。
func (m *Manager) headCommitTime(ctx context.Context, path string) (int64, string, error) {
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", path, "log", "-1", "--format=%ct %H", "HEAD",
	}})
	if err != nil {
		// 空仓库（unborn HEAD）没有提交，不算错误。
		if _, verr := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
			"--no-optional-locks", "-C", path, "rev-parse", "--verify", "--quiet", "HEAD",
		}}); verr != nil {
			return 0, "", nil
		}
		return 0, "", err
	}
	fields := strings.Fields(strings.TrimSpace(res.Stdout))
	if len(fields) == 0 {
		return 0, "", nil
	}
	sec, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parse commit time %q: %w", fields[0], err)
	}
	head := ""
	if len(fields) > 1 {
		head = fields[1]
	}
	return sec, head, nil
}

// latestCommitTime 返回所有 ref 里最新的提交时间；空仓库返回 0。
func (m *Manager) latestCommitTime(ctx context.Context, path string) int64 {
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", path, "log", "--all", "-1", "--format=%ct",
	}})
	if err != nil {
		return 0
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return 0
	}
	return sec
}

// repoDirty 报告工作区是否有未提交改动（含 untracked）或 stash。
//
// --no-optional-locks 保证 --dry-run 不会写 .git/index：git status 默认会
// 刷新 stat 缓存，那在 dry-run 里是不该发生的写入。
func (m *Manager) repoDirty(ctx context.Context, path string) (bool, error) {
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", path, "status", "--porcelain", "--untracked-files=all",
	}})
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(res.Stdout) != "" {
		return true, nil
	}
	stash, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", path, "stash", "list",
	}})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(stash.Stdout) != "", nil
}

// linkedWorktrees 报告仓库是否有主 worktree 之外的 linked worktree。
//
// 没有 .git/worktrees 目录就不可能有 linked worktree，先做一次文件系统检查，
// 避免每个普通仓库都跑一次 git worktree list。
func (m *Manager) linkedWorktrees(ctx context.Context, path string) (bool, error) {
	if !fsx.IsDir(filepath.Join(path, ".git", "worktrees")) {
		return false, nil
	}
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", path, "worktree", "list", "--porcelain",
	}})
	if err != nil {
		return false, err
	}
	count := 0
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			count++
		}
	}
	return count > 1, nil
}

// missingRefs 返回 older 的本地引用中、在 newer 的任何 ref 里都找不到的那些。
//
// 这是"不丢失仓库"的最后一道保险：即使 older 的 HEAD 更旧，它也可能有一个
// 从未推送的本地分支。只比较 refs/heads、refs/tags、refs/notes——remote-tracking
// ref 只是远端的本地副本，删掉后还能从远端取回。
func (m *Manager) missingRefs(ctx context.Context, older, newer string) ([]string, error) {
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", older, "for-each-ref",
		"--format=%(objectname) %(refname)", "refs/heads", "refs/tags", "refs/notes",
	}})
	if err != nil {
		return nil, err
	}
	var missing []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash, ref := fields[0], fields[1]
		if seen[hash] {
			continue
		}
		seen[hash] = true
		contained, err := m.refContained(ctx, newer, hash)
		if err != nil {
			return nil, err
		}
		if !contained {
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

// refContained 报告 newer 是否有任何一个 ref 包含 hash 指向的提交。
func (m *Manager) refContained(ctx context.Context, newer, hash string) (bool, error) {
	// 先解析成 commit：tag 对象的哈希不能直接喂给 for-each-ref --contains。
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", newer, "rev-parse", "--verify", "--quiet", hash + "^{commit}",
	}})
	if err != nil {
		return false, nil // 对象不存在：较新一侧确实没有这段历史
	}
	commit := strings.TrimSpace(res.Stdout)
	if commit == "" {
		return false, nil
	}
	refs, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{
		"--no-optional-locks", "-C", newer, "for-each-ref", "--contains", commit, "--format=%(refname)",
	}})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(refs.Stdout) != "", nil
}

func sameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}
