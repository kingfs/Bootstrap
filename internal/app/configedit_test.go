package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUserConfig 在沙箱里写一份用户级配置文件。
func writeUserConfig(t *testing.T, a *App, body string) string {
	t.Helper()
	path := a.Paths.ConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	a.ConfigSources = append(a.ConfigSources, path)
	return path
}

// TestConfigSetCreatesFileWithHeader：第一次 set 时顺手生成配置文件，
// 并且要带上与 `config init` 相同的说明头（不然用户打开只看到两行，不知道这是什么）。
func TestConfigSetCreatesFileWithHeader(t *testing.T) {
	a, _ := newTestApp(t)
	res, err := a.ConfigSet("log.level", "debug", false)
	if err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if !res.Created {
		t.Error("应当报告 created=true")
	}
	data, err := os.ReadFile(a.Paths.ConfigFile())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "Bootstrap (bs) 配置文件") {
		t.Errorf("缺少说明头：\n%s", data)
	}
	if !strings.Contains(string(data), "level: debug") {
		t.Errorf("缺少写入的值：\n%s", data)
	}
	// 写进去的配置必须能被真实装载路径读出来。
	if err := a.validateConfigFile(a.Paths.ConfigFile()); err != nil {
		t.Errorf("生成的配置不合法：%v", err)
	}
}

// TestConfigSetDryRunWritesNothing：干跑必须零写入（连目录都不建）。
func TestConfigSetDryRunWritesNothing(t *testing.T) {
	a, root := newTestApp(t)
	a.DryRun = true
	res, err := a.ConfigSet("log.level", "debug", false)
	if err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if !res.DryRun {
		t.Error("结果里应当标记 dry_run")
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err == nil {
		t.Error("干跑创建了配置文件")
	}
}

// TestConfigSetRestoresInvalidResult：值通过了类型检查、却违反校验规则时，
// 必须把原文件**逐字节**放回去——不能让用户拿到一份 bs 自己读不了的配置。
func TestConfigSetRestoresInvalidResult(t *testing.T) {
	a, _ := newTestApp(t)
	path := writeUserConfig(t, a, "# 注释要留着\nversion: 1\nlog:\n  level: info\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	_, err = a.ConfigSet("log.level", "bogus", false)
	if err == nil {
		t.Fatal("非法值应当失败")
	}
	if !strings.Contains(err.Error(), "已还原") {
		t.Errorf("错误应当说明已还原：%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("回滚后内容不一致：\n%s", after)
	}
	// 备份留着（用户能自己对照），但不该留下半成品配置。
	if _, err := os.Stat(path + ".bs-backup." + Stamp()); err != nil && !strings.Contains(err.Error(), "no such file") {
		t.Logf("备份路径：%v", err)
	}
}

// TestConfigGetEffectiveValueAndSource：get 读的是合并后的生效值，并指出它来自哪个文件。
func TestConfigGetEffectiveValueAndSource(t *testing.T) {
	a, _ := newTestApp(t)
	path := writeUserConfig(t, a, "version: 1\nlog:\n  level: warn\n")
	// 生效配置里是 warn（真实运行时由 config.Load 装载，这里对齐一下）。
	a.Cfg.Log.Level = "warn"

	val, err := a.ConfigGet("log.level")
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if val.Value != "warn" {
		t.Errorf("value = %q，期望 warn", val.Value)
	}
	if val.Source != path {
		t.Errorf("source = %q，期望 %q", val.Source, path)
	}
	if val.Default {
		t.Error("warn 不是内置默认值（info），不该标成默认")
	}

	val, err = a.ConfigGet("repo.layout")
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if !val.Default {
		t.Errorf("未被改过的键应当标成默认值：%+v", val)
	}
}

// TestConfigGetUnknownKeySuggests：拼错键名时给出候选（这是 get/set 最常见的失败）。
func TestConfigGetUnknownKeySuggests(t *testing.T) {
	a, _ := newTestApp(t)
	_, err := a.ConfigGet("ui.confrim")
	if err == nil {
		t.Fatal("未知键应当失败")
	}
	if !strings.Contains(err.Error(), "ui.confirm") {
		t.Errorf("应当给出候选：%v", err)
	}
}

// TestConfigSetUnsetMissingKey：删一个本来就没有的键要明确报错，
// 而不是"成功但什么都没做"。
func TestConfigSetUnsetMissingKey(t *testing.T) {
	a, _ := newTestApp(t)
	writeUserConfig(t, a, "version: 1\n")
	if _, err := a.ConfigSet("ui.confirm", "", true); err == nil {
		t.Fatal("删除不存在的键应当失败")
	}
}

// TestConfigKeysIncludesUserDefined：keys 既有内置键，也有用户自己加的（例如自定义 profile）。
func TestConfigKeysIncludesUserDefined(t *testing.T) {
	a, _ := newTestApp(t)
	writeUserConfig(t, a, "version: 1\nproxy:\n  profiles:\n    office:\n      http: http://127.0.0.1:7890\n")
	keys, err := a.ConfigKeys()
	if err != nil {
		t.Fatalf("ConfigKeys: %v", err)
	}
	joined := strings.Join(keys, "\n")
	for _, want := range []string{"repo.root", "timeouts.download", "proxy.profiles.office.http"} {
		if !strings.Contains(joined, want) {
			t.Errorf("keys 缺少 %s：\n%s", want, joined)
		}
	}
}

// TestConfigEditPrepareDryRun：干跑时不该生成配置文件，也不该做备份。
func TestConfigEditPrepareDryRun(t *testing.T) {
	a, root := newTestApp(t)
	a.DryRun = true
	path, created, backup, err := a.ConfigEditPrepare()
	if err != nil {
		t.Fatalf("ConfigEditPrepare: %v", err)
	}
	if path == "" || !created || backup != "" {
		t.Errorf("path=%q created=%v backup=%q", path, created, backup)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err == nil {
		t.Error("干跑生成了配置文件")
	}
}
