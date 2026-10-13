package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kingfs/Bootstrap/pkg/version"
)

// SourceKind 标识清单条目的来源层次。
type SourceKind string

// 清单来源层次。
const (
	SourceBuiltin SourceKind = "builtin"
	SourceUser    SourceKind = "user"
)

// Source 记录一个被加载的清单文件。
type Source struct {
	Path  string
	Kind  SourceKind
	Tools int
}

// LoadOptions 控制清单加载。
type LoadOptions struct {
	// Builtin 为 true 时从 BuiltinFS 加载 common.yaml 与 <GOOS>.yaml。
	Builtin   bool
	BuiltinFS fs.FS
	GOOS      string

	// UserDirs 是用户覆盖目录（按顺序，后覆盖前）。
	UserDirs []string

	// ReadFile / ReadDir 便于测试注入；为空时用 os 实现。
	ReadFile func(string) ([]byte, error)
	ReadDir  func(string) ([]fs.DirEntry, error)
}

// LoadFile 单独加载一个 YAML 文件（用于 `bs tool manifest diff`）。
//
// 只做"解析 + 校验"，不参与合并，因此调用方拿到的是该文件自己的声明。
func LoadFile(path string) (*Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mf Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&mf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if mf.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported manifest version %d (want 1)", path, mf.Version)
	}
	out := &Loaded{Profiles: map[string]Profile{}}
	for _, t := range mf.Tools {
		t.Origin = path
		out.Tools = append(out.Tools, t)
	}
	for name, p := range mf.Profiles {
		out.Profiles[name] = p
	}
	out.Sources = []Source{{Path: path, Kind: SourceUser, Tools: len(mf.Tools)}}
	return out, nil
}

// Loaded 是合并后的清单。
type Loaded struct {
	Tools    []Tool
	Profiles map[string]Profile
	Sources  []Source
}

// ByID 按 id 取工具。
func (l *Loaded) ByID(id string) (Tool, bool) {
	for _, t := range l.Tools {
		if t.ID == id {
			return t, true
		}
	}
	return Tool{}, false
}

// IDs 返回全部工具 id（保持清单顺序，便于稳定输出）。
func (l *Loaded) IDs() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.Tools))
	for _, t := range l.Tools {
		out = append(out, t.ID)
	}
	return out
}

// Dependents 返回直接依赖某个工具的工具 id（`depends_on` 的反向查询）。
func (l *Loaded) Dependents(id string) []string {
	if l == nil {
		return nil
	}
	var out []string
	for _, t := range l.Tools {
		for _, dep := range t.DependsOn {
			if dep == id {
				out = append(out, t.ID)
				break
			}
		}
	}
	return out
}

// ProfileNames 返回排序后的 profile 名。
func (l *Loaded) ProfileNames() []string {
	names := make([]string, 0, len(l.Profiles))
	for name := range l.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Load 按 §6.3 的顺序加载并合并清单：
//
//	内置 common.yaml → 内置 <GOOS>.yaml → 用户目录（文件名排序，后覆盖前）
//
// 合并按 id 进行；数组字段整体替换，需要追加时用 `+install` / `+tags` 等显式语法。
func Load(opts LoadOptions) (*Loaded, error) {
	readFile := opts.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	readDir := opts.ReadDir
	if readDir == nil {
		readDir = os.ReadDir
	}

	out := &Loaded{Profiles: map[string]Profile{}}
	index := map[string]int{}

	apply := func(mf *Manifest, src Source) error {
		count := 0
		seenInFile := map[string]bool{}
		for _, t := range mf.Tools {
			if seenInFile[t.ID] {
				return fmt.Errorf("%s: duplicate tool id %q", src.Path, t.ID)
			}
			seenInFile[t.ID] = true
			if err := compileTool(&t); err != nil {
				return fmt.Errorf("%s: tool %q: %w", src.Path, t.ID, err)
			}
			t.Origin = src.Path
			if i, ok := index[t.ID]; ok {
				out.Tools[i] = mergeTool(out.Tools[i], t)
				continue
			}
			index[t.ID] = len(out.Tools)
			out.Tools = append(out.Tools, t)
			count++
		}
		for name, p := range mf.Profiles {
			// profile 整体替换：需要组合时用 extends，避免"半合并"的意外结果。
			out.Profiles[name] = p
		}
		out.Sources = append(out.Sources, Source{Path: src.Path, Kind: src.Kind, Tools: count})
		return nil
	}

	loadBytes := func(path string, kind SourceKind, data []byte) error {
		var mf Manifest
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true) // 拼错字段名必须报错，而不是被静默忽略
		if err := dec.Decode(&mf); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if mf.Version != 1 {
			return fmt.Errorf("%s: unsupported manifest version %d (want 1)", path, mf.Version)
		}
		mf.Source = path
		return apply(&mf, Source{Path: path, Kind: kind})
	}

	if opts.Builtin {
		if opts.BuiltinFS == nil {
			return nil, errors.New("builtin manifest requested but no filesystem provided")
		}
		for _, name := range builtinFiles(opts.GOOS) {
			data, err := fs.ReadFile(opts.BuiltinFS, name)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("read builtin manifest %s: %w", name, err)
			}
			if err := loadBytes("builtin:"+name, SourceBuiltin, data); err != nil {
				return nil, err
			}
		}
	}

	for _, dir := range opts.UserDirs {
		if dir == "" {
			continue
		}
		entries, err := readDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // 用户目录不存在是正常情况（没有覆盖文件而已）
			}
			return nil, fmt.Errorf("read manifest dir %s: %w", dir, err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext != ".yaml" && ext != ".yml" {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			data, err := readFile(path)
			if err != nil {
				return nil, fmt.Errorf("read manifest %s: %w", path, err)
			}
			if err := loadBytes(path, SourceUser, data); err != nil {
				return nil, err
			}
		}
	}

	if err := resolveAppends(out); err != nil {
		return nil, err
	}
	return out, nil
}

// builtinFiles 返回内置清单文件列表（顺序即覆盖顺序）。
func builtinFiles(goos string) []string {
	files := []string{"common.yaml"}
	switch goos {
	case "linux", "darwin", "windows":
		files = append(files, goos+".yaml")
	}
	files = append(files, "profiles.yaml")
	return files
}

// resolveAppends 把 `+install` / `+uninstall` / `+tags` / `+provides` 合并进主字段。
//
// 放在最后统一处理：这样"追加"永远发生在所有覆盖文件之后，语义稳定。
func resolveAppends(l *Loaded) error {
	for i := range l.Tools {
		t := &l.Tools[i]
		if len(t.AppendInstall) > 0 {
			t.Install = append(t.Install, t.AppendInstall...)
		}
		if len(t.AppendUninstall) > 0 {
			t.Uninstall = append(t.Uninstall, t.AppendUninstall...)
		}
		if len(t.AppendTags) > 0 {
			t.Tags = append(t.Tags, t.AppendTags...)
		}
		if len(t.AppendProvides) > 0 {
			t.Provides = append(t.Provides, t.AppendProvides...)
		}
		if len(t.AppendPathDirs) > 0 {
			t.PathDirs = append(t.PathDirs, t.AppendPathDirs...)
		}
		t.AppendInstall, t.AppendUninstall, t.AppendTags, t.AppendProvides, t.AppendPathDirs = nil, nil, nil, nil, nil
		if t.Disabled {
			continue
		}
		if len(t.Install) == 0 {
			return fmt.Errorf("tool %q: no install method", t.ID)
		}
	}
	return nil
}

// mergeTool 把 over 合并到 base 上（数组整体替换，除非用 + 语法追加）。
func mergeTool(base, over Tool) Tool {
	out := base
	if over.Name != "" {
		out.Name = over.Name
	}
	if over.Stack != "" {
		out.Stack = over.Stack
	}
	if over.Kind != "" {
		out.Kind = over.Kind
	}
	if over.Description != "" {
		out.Description = over.Description
	}
	if over.Homepage != "" {
		out.Homepage = over.Homepage
	}
	if len(over.Tags) > 0 {
		out.Tags = over.Tags
	}
	if len(over.DependsOn) > 0 {
		out.DependsOn = over.DependsOn
	}
	out.When = mergeWhen(out.When, over.When)
	if len(over.Provides) > 0 {
		out.Provides = over.Provides
	}
	if len(over.PathDirs) > 0 {
		out.PathDirs = over.PathDirs
	}
	out.Verify = mergeVerify(out.Verify, over.Verify)
	if len(over.Install) > 0 {
		out.Install = over.Install
	}
	if over.Update != nil {
		out.Update = over.Update
	}
	if len(over.Uninstall) > 0 {
		out.Uninstall = over.Uninstall
	}
	if over.Disabled {
		out.Disabled = true
	}
	if over.ManagerOptional {
		out.ManagerOptional = true
	}
	if len(over.AppendInstall) > 0 {
		out.AppendInstall = append(out.AppendInstall, over.AppendInstall...)
	}
	if len(over.AppendUninstall) > 0 {
		out.AppendUninstall = append(out.AppendUninstall, over.AppendUninstall...)
	}
	if len(over.AppendTags) > 0 {
		out.AppendTags = append(out.AppendTags, over.AppendTags...)
	}
	if len(over.AppendProvides) > 0 {
		out.AppendProvides = append(out.AppendProvides, over.AppendProvides...)
	}
	if len(over.AppendPathDirs) > 0 {
		out.AppendPathDirs = append(out.AppendPathDirs, over.AppendPathDirs...)
	}
	out.Origin = over.Origin
	return out
}

func mergeWhen(base, over When) When {
	if len(over.OS) > 0 {
		base.OS = over.OS
	}
	if len(over.OSNot) > 0 {
		base.OSNot = over.OSNot
	}
	if len(over.Arch) > 0 {
		base.Arch = over.Arch
	}
	if len(over.Distro) > 0 {
		base.Distro = over.Distro
	}
	if len(over.DistroNot) > 0 {
		base.DistroNot = over.DistroNot
	}
	if len(over.Has) > 0 {
		base.Has = over.Has
	}
	if len(over.HasNot) > 0 {
		base.HasNot = over.HasNot
	}
	return base
}

func mergeVerify(base, over Verify) Verify {
	if over.Command != "" {
		base.Command = over.Command
	}
	if len(over.VersionArgs) > 0 {
		base.VersionArgs = over.VersionArgs
	}
	if over.VersionRegex != "" {
		base.VersionRegex = over.VersionRegex
	}
	if over.MinVersion != "" {
		base.MinVersion = over.MinVersion
	}
	if over.ExactVersion != "" {
		base.ExactVersion = over.ExactVersion
	}
	return base
}

// compileTool 预编译版本正则与版本号：清单加载期就报错，而不是探测时才发现写错。
func compileTool(t *Tool) error {
	if t.Verify.VersionRegex != "" {
		re, err := regexp.Compile(t.Verify.VersionRegex)
		if err != nil {
			return fmt.Errorf("verify.version_regex: %w", err)
		}
		if re.SubexpIndex("v") < 0 {
			return fmt.Errorf("verify.version_regex %q must contain a named group (?P<v>...)", t.Verify.VersionRegex)
		}
		t.Verify.re = re
	}
	if t.Verify.MinVersion != "" {
		min, err := version.Parse(t.Verify.MinVersion)
		if err != nil {
			return fmt.Errorf("verify.min_version %q: %w", t.Verify.MinVersion, err)
		}
		t.Verify.min = &min
	}
	if t.Verify.ExactVersion != "" {
		exact, err := version.Parse(t.Verify.ExactVersion)
		if err != nil {
			return fmt.Errorf("verify.exact_version %q: %w", t.Verify.ExactVersion, err)
		}
		t.Verify.exact = &exact
	}
	return nil
}
