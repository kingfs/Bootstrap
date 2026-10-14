package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseUninstallRemovesArtifacts：`via: release` 装出去的东西必须能卸掉。
//
// 这条来自 Docker 集成测试的真实发现：`bs tool uninstall lazygit --yes` 返回 0、
// 表格里写着"不支持"，而 ~/.local/bin/lazygit 原封不动——用户以为卸掉了。
func TestReleaseUninstallRemovesArtifacts(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params:
          repo: owner/tool
          asset: tool-{{version}}-{{os}}-{{arch}}.tar.gz
          binary: tool
          dest: ~/.local/bin
          symlink: {from: ~/.local/bin/tool, to: ~/.local/bin/tt}
`)
	tool, _ := loaded.ByID("tool")
	in, _ := NewRegistry().Get("release")

	// 造出"已安装"的现场：一个可执行文件、一个软链，外加同目录里别人的文件。
	binDir := filepath.Join(env.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(binDir, "tool")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "tt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(binDir, "别人的东西")
	if err := os.WriteFile(neighbour, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpUninstall)
	if err != nil {
		t.Fatalf("Plan(uninstall): %v", err)
	}
	if len(action.Downloads) != 0 || action.Network {
		t.Errorf("卸载不该下载/联网：downloads=%d network=%v", len(action.Downloads), action.Network)
	}
	if err := in.Execute(context.Background(), action, env); err != nil {
		t.Fatalf("Execute(uninstall): %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("二进制没删掉：%v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("软链没删掉：%v", err)
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Errorf("共享目录里别人的文件必须原封不动：%v", err)
	}
	if info, err := os.Stat(binDir); err != nil || !info.IsDir() {
		t.Errorf("共享目录本身不能被删：%v", err)
	}

	// 幂等：再卸一次不报错（路径已经不存在）。
	if err := in.Execute(context.Background(), action, env); err != nil {
		t.Errorf("重复卸载应当无害：%v", err)
	}
}

// TestReleaseUninstallDirectURLWithVersionedURL：URL 里有 {{version}} 是常态，
// 只要产物名与 dest 确定，卸载就必须照常工作（kubectl 就是这么写的）。
func TestReleaseUninstallDirectURLWithVersionedURL(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: kubectl
    stack: infra
    kind: cli
    verify: {command: kubectl}
    install:
      - via: release
        params:
          url: "https://dl.k8s.io/release/v{{version}}/bin/{{os}}/{{arch}}/kubectl"
          version_source: "https://dl.k8s.io/release/stable.txt"
          binary: kubectl
          dest: "~/.local/bin"
`)
	tool, _ := loaded.ByID("kubectl")
	in, _ := NewRegistry().Get("release")
	binDir := filepath.Join(env.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(binDir, "kubectl")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpUninstall)
	if err != nil {
		t.Fatalf("Plan(uninstall): %v", err)
	}
	if err := in.Execute(context.Background(), action, env); err != nil {
		t.Fatalf("Execute(uninstall): %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("二进制没删掉：%v", err)
	}
}

// TestReleaseUninstallDryRun：干跑一行都不许删（§8.5 安全红线）。
func TestReleaseUninstallDryRun(t *testing.T) {
	env := newFakeEnv(t)
	env.dry = true
	loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params: {repo: owner/tool, asset: tool.tar.gz, binary: tool, dest: ~/.local/bin}
`)
	tool, _ := loaded.ByID("tool")
	in, _ := NewRegistry().Get("release")
	binDir := filepath.Join(env.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(binDir, "tool")
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpUninstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := in.Execute(context.Background(), action, env); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("干跑不许删文件：%v", err)
	}
}

// TestReleaseUninstallRefusesUnsafePlans：两条"删不干净就别乱删"的边界。
func TestReleaseUninstallRefusesUnsafePlans(t *testing.T) {
	cases := []struct {
		name     string
		params   string
		wantSub  string
		wantTmpl bool // 期望提示里点明"是模板导致路径不确定"
	}{
		{
			name:    "整包解压型",
			params:  "url: https://example.com/tool-{{version}}.zip\n          extract: true\n          dest: ~/.local/share/tool\n          binary: tool",
			wantSub: "手工删除",
		},
		{
			name:     "路径里有版本模板",
			params:   "repo: owner/tool\n          asset: tool.tar.gz\n          binary: tool\n          dest: ~/.local/share/tool-{{version}}",
			wantSub:  "手工删除",
			wantTmpl: true,
		},
		{
			name: "软链目标里有版本模板",
			params: "repo: owner/tool\n          asset: tool.tar.gz\n          binary: tool\n" +
				"          dest: ~/.local/bin\n          symlink:\n" +
				"            from: \"~/.local/share/tool-{{version}}/bin/tool\"\n" +
				"            to: \"~/.local/bin/tool-{{version}}\"",
			wantSub:  "手工删除",
			wantTmpl: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFakeEnv(t)
			loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params:
          `+tc.params+`
`)
			tool, _ := loaded.ByID("tool")
			in, _ := NewRegistry().Get("release")
			_, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpUninstall)
			var unsupported *ErrUnsupported
			if !errors.As(err, &unsupported) {
				t.Fatalf("应当报不支持，实际：%v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("提示里要给出下一步，实际：%v", err)
			}
			if tc.wantTmpl && !strings.Contains(err.Error(), "{{") {
				t.Errorf("应当指出是模板导致无法确定路径：%v", err)
			}
		})
	}
}

// TestReleaseUninstallKeepsRealFileAtSymlinkPath：软链路径上如果站着用户的真实文件，
// 卸载必须放过它——安装时 replaceSymlink 就不会覆盖它，卸载更不该删（§8.1）。
func TestReleaseUninstallKeepsRealFileAtSymlinkPath(t *testing.T) {
	env := newFakeEnv(t)
	binDir := filepath.Join(env.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(binDir, "tt")
	if err := os.WriteFile(userFile, []byte("用户自己写的\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeReleaseArtifacts(releasePlan{RemoveLinks: []string{userFile}}, env); err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	body, err := os.ReadFile(userFile)
	if err != nil || string(body) != "用户自己写的\n" {
		t.Errorf("用户文件必须原封不动：%v %q", err, body)
	}
}

// TestReleaseUninstallRefusesDirectory：万一 dest/binary 推出一个目录，
// 宁可报错也不能递归删（这属于清单写错，不是"卸载失败"）。
func TestReleaseUninstallRefusesDirectory(t *testing.T) {
	env := newFakeEnv(t)
	dir := filepath.Join(env.home, ".local", "bin", "tool")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := removeReleaseArtifacts(releasePlan{Remove: []string{dir}}, env)
	if err == nil || !strings.Contains(err.Error(), "refusing to remove a directory") {
		t.Fatalf("应当拒绝删目录，实际：%v", err)
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		t.Errorf("目录必须还在：%v", statErr)
	}
}
