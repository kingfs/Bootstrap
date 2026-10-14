package config

import (
	"errors"
	"strings"
	"testing"
)

func defaultTemplate(t *testing.T) []byte {
	t.Helper()
	cfg := Defaults()
	data, err := Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}

// TestSetKeyPreservesCommentsAndOrder：`config set` 只该改那一行——
// 注释、键顺序、其它内容都要原样留着（这是用 yaml.Node 而不是 map 的全部理由）。
func TestSetKeyPreservesCommentsAndOrder(t *testing.T) {
	data := []byte(`# 我的配置（注释要留着）
version: 1
log:
  level: info # 这一行的说明
ui:
  confirm: true
`)
	out, old, err := SetKey(data, "ui.confirm", "false", EditOptions{Template: defaultTemplate(t)})
	if err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	if old != "true" {
		t.Errorf("old = %q，期望 true", old)
	}
	text := string(out)
	for _, want := range []string{"# 我的配置（注释要留着）", "# 这一行的说明", "confirm: false"} {
		if !strings.Contains(text, want) {
			t.Errorf("输出里缺少 %q：\n%s", want, text)
		}
	}
	// 键顺序：version 还在 log 之前、log 在 ui 之前
	if strings.Index(text, "version:") > strings.Index(text, "log:") || strings.Index(text, "log:") > strings.Index(text, "ui:") {
		t.Errorf("键顺序变了：\n%s", text)
	}
	if strings.Contains(text, "confirm: true") {
		t.Errorf("旧值还在：\n%s", text)
	}
}

// TestSetKeyTypes：类型由默认配置当模板——写错类型必须在写入前报错，
// 而不是等下一次 bs 启动才发现。
func TestSetKeyTypes(t *testing.T) {
	tpl := defaultTemplate(t)
	data := []byte("version: 1\n")
	cases := []struct {
		key     string
		value   string
		wantErr string
	}{
		{"ui.confirm", "true", ""},
		{"ui.confirm", "yes", "期望布尔值"},
		{"repo.clone.depth", "3", ""},
		{"repo.clone.depth", "deep", "期望整数"},
		{"log.level", "debug", ""},
		{"repo.hosts", `["github.com", "gitlab.com"]`, ""},
		{"repo.hosts", "github.com", "期望列表"},
	}
	for _, tc := range cases {
		out, _, err := SetKey(data, tc.key, tc.value, EditOptions{Template: tpl})
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("SetKey(%s=%s) 意外失败：%v", tc.key, tc.value, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("SetKey(%s=%s) err = %v，期望包含 %q", tc.key, tc.value, err, tc.wantErr)
		case tc.wantErr == "":
			// 写进去的值必须能被读回来（类型正确）
			if _, err := GetKey(out, tc.key); err != nil {
				t.Errorf("写完后读不回 %s：%v", tc.key, err)
			}
		}
	}
}

// TestSetKeyCreatesNestedMaps：`proxy.profiles.office.http` 这种带"任意名字"的
// 中间层要能自动补出来。
func TestSetKeyCreatesNestedMaps(t *testing.T) {
	data := []byte("version: 1\nproxy:\n  profiles: {}\n")
	out, _, err := SetKey(data, "proxy.profiles.office.http", "http://127.0.0.1:7890", EditOptions{Template: defaultTemplate(t)})
	if err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	kv, err := GetKey(out, "proxy.profiles.office.http")
	if err != nil {
		t.Fatalf("GetKey: %v\n%s", err, out)
	}
	if kv.Value != "http://127.0.0.1:7890" || kv.Kind != "scalar" {
		t.Errorf("get = %+v", kv)
	}
}

// TestSetKeyUnset：删掉键＝回到"由默认值/上层配置决定"，而不是写一个空串。
func TestSetKeyUnset(t *testing.T) {
	data := []byte("version: 1\nui:\n  confirm: false\n  progress: plain\n")
	out, old, err := SetKey(data, "ui.confirm", "", EditOptions{Unset: true})
	if err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	if old != "false" {
		t.Errorf("old = %q", old)
	}
	if strings.Contains(string(out), "confirm") {
		t.Errorf("键没被删掉：\n%s", out)
	}
	if !strings.Contains(string(out), "progress: plain") {
		t.Errorf("误删了别的键：\n%s", out)
	}
	// 删不存在的键要报 ErrKeyNotFound（而不是静默成功）
	if _, _, err := SetKey(out, "ui.confirm", "", EditOptions{Unset: true}); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("err = %v，期望 ErrKeyNotFound", err)
	}
}

// TestGetKeyKinds：标量原样返回，列表/映射返回 YAML 文本。
func TestGetKeyKinds(t *testing.T) {
	data := []byte("version: 1\nrepo:\n  hosts:\n    - github.com\n    - gitlab.com\nproxy:\n  profiles:\n    office:\n      http: http://127.0.0.1:7890\n")
	kv, err := GetKey(data, "repo.hosts")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if kv.Kind != "list" || !strings.Contains(kv.Value, "github.com") {
		t.Errorf("list = %+v", kv)
	}
	kv, err = GetKey(data, "proxy.profiles.office")
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if kv.Kind != "map" || !strings.Contains(kv.Value, "http: http://127.0.0.1:7890") {
		t.Errorf("map = %+v", kv)
	}
	if _, err := GetKey(data, "repo.nope"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("err = %v，期望 ErrKeyNotFound", err)
	}
	if _, err := GetKey(data, "repo.hosts.0"); err != nil {
		t.Errorf("列表下标应当可读：%v", err)
	}
}

// TestKeysAndSuggestions：拼错键名时要给出接近的候选。
func TestKeysAndSuggestions(t *testing.T) {
	keys, err := Keys([]byte("version: 1\nui:\n  confirm: true\n  progress: auto\nrepo:\n  root: /tmp/src\n"))
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	want := []string{"repo", "repo.root", "ui", "ui.confirm", "ui.progress", "version"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v，期望 %v", keys, want)
	}
	if got := SuggestKeys(keys, "ui.confrim"); len(got) == 0 || got[0] != "ui.confirm" {
		t.Errorf("建议 = %v，期望首选 ui.confirm", got)
	}
	if got := SuggestKeys(keys, "ui"); len(got) < 3 {
		t.Errorf("前缀匹配应当给出全部 ui.*：%v", got)
	}
}

// TestSetKeyKeepsEmptyDocumentUsable：空文件（刚 touch 出来的）也要能直接写。
func TestSetKeyKeepsEmptyDocumentUsable(t *testing.T) {
	out, _, err := SetKey(nil, "log.level", "debug", EditOptions{Template: defaultTemplate(t)})
	if err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	kv, err := GetKey(out, "log.level")
	if err != nil || kv.Value != "debug" {
		t.Fatalf("kv = %+v, err = %v\n%s", kv, err, out)
	}
}
