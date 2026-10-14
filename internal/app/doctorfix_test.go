package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/ui"
)

// doctorFixApp 造一个"配置未生成、rc 里没有受管块、仓库根不存在"的干净沙箱。
func doctorFixApp(t *testing.T) (*App, string) {
	t.Helper()
	a, root := newTestApp(t)
	a.ConfigSources = nil
	return a, root
}

// TestPlanFixesOnlySafeItems：只对"结论明确、修法唯一"的问题出动作，
// 缺 git / 清单写错 / 代理没开这类一律不代劳。
func TestPlanFixesOnlySafeItems(t *testing.T) {
	a, _ := doctorFixApp(t)
	checks := []Check{
		{Name: "config", Status: StatusWarn, Detail: "未找到配置文件"},
		{Name: "path entries", Status: StatusWarn, Detail: "3/3 个目录尚不存在"},
		{Name: "tool manifest", Status: StatusFail, Detail: "manifest: bad field"},
		{Name: "proxy", Status: StatusSkip, Detail: "未启用代理"},
		{Name: "shell rc", Status: StatusWarn, Detail: "未安装受管块"},
	}
	plan := a.PlanFixes(checks)
	ids := map[string]bool{}
	for _, act := range plan.Actions {
		ids[act.ID] = true
		if act.Blocked {
			t.Errorf("%s 不该是 Blocked：%s", act.ID, act.Message)
		}
	}
	for _, want := range []string{"config", "shell"} {
		if !ids[want] {
			t.Errorf("缺少动作 %s（实际 %v）", want, ids)
		}
	}
	if len(plan.Actions) != 2 {
		t.Errorf("只应当有 2 个动作，实际 %d：%+v", len(plan.Actions), plan.Actions)
	}
	if plan.Fixable() != 2 {
		t.Errorf("Fixable = %d，期望 2", plan.Fixable())
	}

	// 仓库根不存在时要多一条动作。
	repoChecks := append(checks, Check{Name: "repo root", Status: StatusWarn, Detail: "不存在"})
	if got := len(a.PlanFixes(repoChecks).Actions); got != 3 {
		t.Errorf("带 repo root 时应当有 3 个动作，实际 %d", got)
	}

	// 全部正常时没有动作（`--fix` 不该无中生有）。
	okChecks := []Check{{Name: "config", Status: StatusOK}, {Name: "shell rc", Status: StatusOK}}
	if got := a.PlanFixes(okChecks); len(got.Actions) != 0 {
		t.Errorf("没有问题时不该有计划：%+v", got.Actions)
	}
}

// TestPlanFixesIsReadOnly：计划阶段一个字节都不能写（--dry-run 靠这条成立）。
func TestPlanFixesIsReadOnly(t *testing.T) {
	a, root := doctorFixApp(t)
	before := listTreePaths(t, root)
	checks := a.Doctor(context.Background())
	plan := a.PlanFixes(checks)
	if len(plan.Actions) == 0 {
		t.Fatal("沙箱里应当有可修项（配置/仓库根/受管块）")
	}
	after := listTreePaths(t, root)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("PlanFixes 改了文件系统：\nbefore=%v\nafter=%v", before, after)
	}
}

// TestApplyFixesPreservesUserFiles：修复只能"加东西"——rc 里的原有内容必须字节不变，
// 而且必须留备份；配置文件生成后要能被读出来。
func TestApplyFixesPreservesUserFiles(t *testing.T) {
	a, root := doctorFixApp(t)
	rc := filepath.Join(root, "home", ".bashrc")
	original := "# 我自己的 rc\nexport FOO=1\n"
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatalf("write rc: %v", err)
	}
	a.Platform.Shell = "bash"

	checks := a.Doctor(context.Background())
	plan := a.PlanFixes(checks)
	result := a.ApplyFixes(context.Background(), plan)
	for _, act := range result.Actions {
		if act.Err != "" {
			t.Errorf("%s 修复失败：%s", act.ID, act.Err)
		}
	}

	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("read rc: %v", err)
	}
	if !strings.HasPrefix(string(got), original) {
		t.Errorf("原有 rc 内容被改动：\n%s", got)
	}
	if !strings.Contains(string(got), ">>> bootstrap (managed by 'bs shell install') >>>") {
		t.Errorf("受管块没有写进去：\n%s", got)
	}
	found := false
	for _, act := range result.Actions {
		if act.ID == "shell" && strings.Contains(act.Message, "备份") {
			found = true
		}
	}
	if !found {
		t.Errorf("修复结果应当报出备份路径：%+v", result.Actions)
	}
	if matches, _ := filepath.Glob(rc + ".bs-backup.*"); len(matches) == 0 {
		t.Error("应当留下备份文件")
	}
	if !strings.Contains(mustRead(t, a.Paths.ConfigFile()), "version: 1") {
		t.Errorf("默认配置没有生成：%s", a.Paths.ConfigFile())
	}

	// 复查：受管块与配置这两项应当从 warn 变成 ok。
	after := a.Doctor(context.Background())
	for _, c := range after {
		if (c.Name == "shell rc" || c.Name == "config") && c.Status != StatusOK {
			t.Errorf("修复后 %s 仍为 %s：%s", c.Name, c.Status, c.Detail)
		}
	}
}

// TestApplyFixesSkipsBlocked：预判"修不了"的动作不能装成修过了。
func TestApplyFixesSkipsBlocked(t *testing.T) {
	a, _ := doctorFixApp(t)
	plan := DoctorFixPlan{Actions: []FixAction{
		{ID: "shell", Title: "安装 shell 受管块", Blocked: true, Message: "无法确定 rc 文件"},
	}}
	result := a.ApplyFixes(context.Background(), plan)
	if result.Actions[0].Applied {
		t.Error("Blocked 的动作不该被标记为已执行")
	}
	if result.Fixable() != 0 {
		t.Errorf("Fixable = %d，期望 0", result.Fixable())
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func listTreePaths(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

var _ ui.Prompter = ui.NonInteractive{}
