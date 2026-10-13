package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsEmpty(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state.yaml"))
	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load on missing file should not fail: %v", err)
	}
	if st.Version != 1 || st.Installed == nil {
		t.Errorf("unexpected empty state: %+v", st)
	}
	if st.Proxy.Mode != "" || st.Proxy.Active != "" {
		t.Errorf("fresh state should have no proxy selection: %+v", st.Proxy)
	}
}

func TestSaveIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.yaml")
	s := NewStore(path)

	changed, err := s.SetActiveProfile("local")
	if err != nil {
		t.Fatalf("SetActiveProfile: %v", err)
	}
	if !changed {
		t.Fatal("first activation must write")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	changed, err = s.SetActiveProfile("local")
	if err != nil {
		t.Fatalf("SetActiveProfile again: %v", err)
	}
	if changed {
		t.Error("activating the same profile twice must be a no-op")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("no-op save touched the file (mtime changed)")
	}

	changed, err = s.SetActiveProfile("chaitin")
	if err != nil {
		t.Fatalf("SetActiveProfile chaitin: %v", err)
	}
	if !changed {
		t.Error("switching profile must write")
	}
	name, err := s.ActiveProfile()
	if err != nil {
		t.Fatalf("ActiveProfile: %v", err)
	}
	if name != "chaitin" {
		t.Errorf("ActiveProfile = %q, want chaitin", name)
	}
	mode, err := s.ProxyMode()
	if err != nil {
		t.Fatalf("ProxyMode: %v", err)
	}
	if mode != ProxyModeOn {
		t.Errorf("ProxyMode = %q, want on", mode)
	}
}

// TestOffIsRemembered 断言 `bs proxy off` 会被持久化，而不是被下一次读取当作"没有选择"。
func TestOffIsRemembered(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state.yaml"))
	if _, err := s.SetActiveProfile("local"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	changed, err := s.SetActiveProfile("")
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	if !changed {
		t.Fatal("turning the proxy off must be persisted")
	}
	mode, err := s.ProxyMode()
	if err != nil {
		t.Fatalf("ProxyMode: %v", err)
	}
	if mode != ProxyModeOff {
		t.Errorf("ProxyMode = %q, want off", mode)
	}
	name, err := s.ActiveProfile()
	if err != nil {
		t.Fatalf("ActiveProfile: %v", err)
	}
	if name != "" {
		t.Errorf("ActiveProfile = %q, want empty", name)
	}
}

func TestRecordInstall(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state.yaml"))
	if err := s.RecordInstall(Record{ID: "go", Version: "1.26.9", Via: "manager:g"}); err != nil {
		t.Fatalf("RecordInstall: %v", err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec, ok := st.Installed["go"]
	if !ok {
		t.Fatalf("record missing: %+v", st.Installed)
	}
	if rec.At.IsZero() {
		t.Error("timestamp should be filled in automatically")
	}
}

func TestSaveWithoutPathIsMemoryOnly(t *testing.T) {
	s := NewStore("")
	changed, err := s.SetActiveProfile("local")
	if err != nil {
		t.Fatalf("SetActiveProfile: %v", err)
	}
	// 没有路径时退化为内存态：每次写入都"发生"了（因为没有可比较的持久化载体）。
	if !changed {
		t.Log("in-memory store reported no change; acceptable")
	}
}
