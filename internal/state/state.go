// Package state 持久化跨命令共享的状态：当前活跃的代理 profile、安装记录等。
//
// 设计要点（docs/design.md §7.4、ADR-0010）：
//   - 状态文件**不是**事实来源，实时探测才是；这里只存"用户的选择"与审计信息。
//   - 写入使用 WriteFileIfChanged：内容不变时完全不触碰文件，保证重复执行无副作用。
package state

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// State 是状态文件的根结构。
type State struct {
	Version   int               `yaml:"version"`
	Proxy     ProxyState        `yaml:"proxy"`
	Installed map[string]Record `yaml:"installed,omitempty"`
}

// 代理模式：auto 跟随配置默认值，on 使用 Active，off 明确关闭（且不再回退到环境变量）。
const (
	ProxyModeAuto = "auto"
	ProxyModeOn   = "on"
	ProxyModeOff  = "off"
)

// ProxyState 记录代理相关的用户选择。
type ProxyState struct {
	// Mode 是显式模式：auto|on|off（缺省视为 auto）。
	Mode string `yaml:"mode,omitempty"`
	// Active 是 on 模式下使用的 profile 名。
	Active string `yaml:"active,omitempty"`
}

// Record 是一次成功安装的审计记录。
type Record struct {
	ID      string    `yaml:"id"`
	Version string    `yaml:"version,omitempty"`
	Via     string    `yaml:"via,omitempty"`
	At      time.Time `yaml:"at"`
}

// New 返回一个空状态。
func New() *State {
	return &State{
		Version:   1,
		Installed: map[string]Record{},
	}
}

// Store 读写状态文件。
type Store struct {
	path string
	// mem 是 path 为空时使用的内存态（测试与"无持久化"场景）。
	mem *State
}

// NewStore 构造 Store；path 为空时所有操作退化为内存态（便于测试）。
func NewStore(path string) *Store { return &Store{path: path} }

// Path 返回状态文件路径。
func (s *Store) Path() string { return s.path }

// Load 读取状态；文件不存在时返回空状态而不是错误。
func (s *Store) Load() (*State, error) {
	if s.path == "" {
		if s.mem == nil {
			s.mem = New()
		}
		return s.mem, nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", s.path, err)
	}
	st := New()
	if err := yaml.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", s.path, err)
	}
	if st.Installed == nil {
		st.Installed = map[string]Record{}
	}
	if st.Version == 0 {
		st.Version = 1
	}
	return st, nil
}

// Save 写回状态，返回是否真的发生了写入。
func (s *Store) Save(st *State) (bool, error) {
	if st.Version == 0 {
		st.Version = 1
	}
	if s.path == "" {
		changed := s.mem == nil || !sameState(s.mem, st)
		s.mem = st
		return changed, nil
	}
	data, err := yaml.Marshal(st)
	if err != nil {
		return false, fmt.Errorf("marshal state: %w", err)
	}
	header := "# 本文件由 bs 自动维护，请勿手工编辑（除排障外）\n"
	return fsx.WriteFileIfChanged(s.path, append([]byte(header), data...), 0o644)
}

// SetActiveProfile 激活一个 profile；name 为空表示显式关闭代理（mode=off）。
//
// 返回是否发生变化——内容不变时不写盘，保证重复执行零副作用。
func (s *Store) SetActiveProfile(name string) (bool, error) {
	mode := ProxyModeOn
	if name == "" {
		mode = ProxyModeOff
	}
	return s.SetProxyMode(mode, name)
}

// SetProxyMode 显式设置代理模式。
func (s *Store) SetProxyMode(mode, active string) (bool, error) {
	st, err := s.Load()
	if err != nil {
		return false, err
	}
	if st.Proxy.Mode == mode && st.Proxy.Active == active {
		return false, nil
	}
	st.Proxy.Mode = mode
	st.Proxy.Active = active
	return s.Save(st)
}

// ProxyMode 返回当前代理模式（缺省 auto）。
func (s *Store) ProxyMode() (string, error) {
	st, err := s.Load()
	if err != nil {
		return "", err
	}
	if st.Proxy.Mode == "" {
		return ProxyModeAuto, nil
	}
	return st.Proxy.Mode, nil
}

// ActiveProfile 返回当前活跃的代理 profile 名（可能为空）。
func (s *Store) ActiveProfile() (string, error) {
	st, err := s.Load()
	if err != nil {
		return "", err
	}
	return st.Proxy.Active, nil
}

// RecordInstall 记录一次成功安装。
func (s *Store) RecordInstall(rec Record) error {
	st, err := s.Load()
	if err != nil {
		return err
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	st.Installed[rec.ID] = rec
	_, err = s.Save(st)
	return err
}

// sameState 比较两份状态是否等价（用序列化结果做比较，避免逐字段维护）。
func sameState(a, b *State) bool {
	left, errA := yaml.Marshal(a)
	right, errB := yaml.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(left) == string(right)
}
