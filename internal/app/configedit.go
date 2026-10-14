package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// 本文件是 `bs config get|set|unset|edit` 的用例层。
//
// 三条贯穿始终的规矩：
//  1. **只改用户级配置文件**（`<config>/config.yaml`）。系统级/项目级/环境变量都不碰——
//     那些地方要么需要 root，要么属于某个项目，改了会让人下次完全找不到原因。
//  2. **只改那一行**：用 yaml.Node 原地改，保留注释与键顺序（见 config.SetKey）。
//  3. **不留下坏配置**：写之前备份；写之后立刻用真实装载路径校验一遍，
//     不合法就把原内容放回去并报错——绝不能出现"改完 bs 起不来了"。

// ConfigGet 读取合并后生效配置里的某个键。
func (a *App) ConfigGet(key string) (ConfigValue, error) {
	data, err := config.Marshal(a.Cfg)
	if err != nil {
		return ConfigValue{}, err
	}
	kv, err := config.GetKey(data, key)
	if err != nil {
		return ConfigValue{}, a.keyError(key, err)
	}
	// 有几个键的"生效值"是运行时算出来的，而不是配置里的字面量（目前只有 repo.root：
	// 配置留空时按平台约定 + 已有目录自动挑一个）。用户问 `config get repo.root`
	// 想要的是这个真正的落点，所以这里把它补上并如实标注来源。
	out := ConfigValue{Key: kv.Key, Value: kv.Value, Kind: kv.Kind, Source: a.keySource(key)}
	// 注意这里判的是"值为空"，不是"键不存在"：`bs config init` 生成的模板里
	// `repo.root: ""` 是显式写着的空串，光看 Source 会把它当成"用户设了个空值"。
	if kv.Kind == "scalar" && strings.TrimSpace(kv.Value) == "" {
		if derived, ok := a.derivedConfigValue(key); ok {
			out.Value, out.Derived, out.Source = derived, true, ""
		}
	}
	out.Default = a.keyIsDefault(key)
	return out, nil
}

// derivedConfigValue 返回"运行时才确定"的键的生效值。
func (a *App) derivedConfigValue(key string) (string, bool) {
	switch key {
	case "repo.root":
		root, _ := a.RepoRoot()
		if root == "" {
			return "", false
		}
		return root, true
	default:
		return "", false
	}
}

// ConfigValue 是 `bs config get` 的结果。
type ConfigValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Kind  string `json:"kind"`
	// Source 是提供该值的配置文件（空 = 只用内置默认值）。
	Source string `json:"source,omitempty"`
	// Default 为真表示当前值与内置默认值完全一致（提示"其实没被改过"）。
	Default bool `json:"default"`
	// Derived 表示这个值不是配置里的字面量，而是运行时算出来的（如 repo.root 的自动检测）。
	Derived bool `json:"derived,omitempty"`
}

// ConfigSetResult 是 `bs config set|unset` 的结果。
type ConfigSetResult struct {
	File   string `json:"file"`
	Key    string `json:"key"`
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
	Backup string `json:"backup,omitempty"`
	// Created 表示这次顺手创建了配置文件（因为原本不存在）。
	Created bool `json:"created"`
	// Unset 表示这是删除操作。
	Unset  bool `json:"unset"`
	DryRun bool `json:"dry_run"`
}

// ConfigSet 修改用户级配置文件里的一个键；unset 为真时删除它。
//
// `--dry-run` 时只回显"会改成什么"，一个字节都不写。
func (a *App) ConfigSet(key, value string, unset bool) (ConfigSetResult, error) {
	path := a.Paths.ConfigFile()
	res := ConfigSetResult{File: path, Key: key, Unset: unset, DryRun: a.DryRun}

	original, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		if unset {
			// 配置都还没有，没什么可删的。
			return res, fmt.Errorf("config file %s does not exist", path)
		}
		res.Created = true
	default:
		return res, fmt.Errorf("read %s: %w", path, err)
	}

	tpl, err := config.Marshal(a.defaultConfig())
	if err != nil {
		return res, err
	}
	updated, old, err := config.SetKey(original, key, value, config.EditOptions{Template: tpl, Unset: unset})
	if err != nil {
		return res, a.keyError(key, err)
	}
	res.Old = old
	if strings.TrimSpace(res.Old) == "" {
		// 用户文件里原本没写这个键：回显"当前生效值"更有用（否则旧值永远是空串）。
		if eff, err := a.ConfigGet(key); err == nil {
			res.Old = eff.Value
		}
	}
	if unset {
		res.New = ""
	} else {
		res.New = strings.TrimSpace(value)
	}

	if a.DryRun {
		return res, nil
	}
	if err := a.EnsureRuntimeDirs(); err != nil {
		return res, err
	}
	if len(original) > 0 {
		backup, err := fsx.BackupFile(path, Stamp())
		if err != nil {
			return res, err
		}
		res.Backup = backup
	}
	// 新建文件时带上说明头（与 `config init` 一致），避免生成一个"光秃秃"的配置。
	body := updated
	if res.Created {
		body = append(configHeader(), updated...)
	}
	if err := fsx.AtomicWriteFile(path, body, 0o644); err != nil {
		return res, err
	}

	// 写后立刻按真实装载路径校验：不合法就回滚，绝不留一份"bs 读不了"的配置。
	if err := a.validateConfigFile(path); err != nil {
		rollbackErr := a.restoreConfig(path, original, res.Created)
		if rollbackErr != nil {
			return res, fmt.Errorf("新配置不合法（%w），且回滚失败：%w", err, rollbackErr)
		}
		return res, fmt.Errorf("新配置不合法，已还原原文件：%w", err)
	}
	return res, nil
}

// ConfigEditPrepare 为 `bs config edit` 准备可编辑的文件：不存在时先生成默认配置。
//
// 返回的 backup 只在"文件原本就存在"时非空——刚生成的文件没有"改之前"可言。
func (a *App) ConfigEditPrepare() (path string, created bool, backup string, err error) {
	path = a.Paths.ConfigFile()
	existed := fsx.IsFile(path)
	if !existed && a.DryRun {
		return path, true, "", nil
	}
	if a.DryRun {
		return path, false, "", nil
	}
	if !existed {
		if err := a.EnsureRuntimeDirs(); err != nil {
			return path, false, "", err
		}
	}
	if _, created, err = a.EnsureConfig(false); err != nil {
		return path, created, "", err
	}
	if existed {
		// 交给编辑器之前先留一份：用户手改出问题时能一眼看到"改之前长什么样"。
		if backup, err = fsx.BackupFile(path, Stamp()); err != nil {
			return path, created, "", err
		}
	}
	return path, created, backup, nil
}

// ConfigEdit 用 $VISUAL/$EDITOR（缺省 vi）打开配置文件，并沿用交互式终端的 stdio。
//
// 这里**不走 pkg/cmdx**：编辑器必须继承真实终端（cmdx 会捕获输出、并在 dry-run 下不执行）。
// 与 subshell.go 的取舍一致：交互式进程是这个规则唯一的例外。
func (a *App) ConfigEdit(ctx context.Context, path string) error {
	// 默认编辑器按平台选：Windows 上 vi 通常不存在，notepad 是唯一保证有的。
	fallback := "vi"
	if a.Platform.OS == "windows" {
		fallback = "notepad"
	}
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"), fallback)
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return errors.New("empty $EDITOR")
	}
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.Out, a.Err
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("编辑器（%s）退出码 %d，配置未校验", editor, exitErr.ExitCode())
		}
		return fmt.Errorf("launch editor %s: %w", editor, err)
	}
	return a.validateConfigFile(path)
}

// ConfigKeys 返回用户级配置文件（不存在则用内置默认配置）里可用的键路径。
func (a *App) ConfigKeys() ([]string, error) {
	tpl, err := config.Marshal(a.defaultConfig())
	if err != nil {
		return nil, err
	}
	keys, err := config.Keys(tpl)
	if err != nil {
		return nil, err
	}
	// 再并上用户文件里自己加的键（例如自定义的 proxy profile）。
	if data, err := os.ReadFile(a.Paths.ConfigFile()); err == nil {
		if userKeys, err := config.Keys(data); err == nil {
			seen := map[string]bool{}
			for _, k := range keys {
				seen[k] = true
			}
			for _, k := range userKeys {
				if !seen[k] {
					keys = append(keys, k)
				}
			}
		}
	}
	return keys, nil
}

// SuggestConfigKeys 给出拼错键名时的候选（供 CLI 提示）。
func (a *App) SuggestConfigKeys(key string) []string {
	keys, err := a.ConfigKeys()
	if err != nil {
		return nil
	}
	return config.SuggestKeys(keys, key)
}

func (a *App) keyError(key string, err error) error {
	switch {
	case errors.Is(err, config.ErrKeyNotFound):
		msg := fmt.Sprintf("unknown config key %q", key)
		if hints := a.SuggestConfigKeys(key); len(hints) > 0 {
			return fmt.Errorf("%s（是不是想写 %s？）", msg, strings.Join(hints, " / "))
		}
		return fmt.Errorf("%s（用 `bs config keys` 看全部可用键）", msg)
	case errors.Is(err, config.ErrKeyNotScalar):
		return fmt.Errorf("%w（该键是映射/列表，请用 `bs config edit` 手工改）", err)
	default:
		return err
	}
}

// keySource 找出"最后一个定义了这个键的配置文件"。
func (a *App) keySource(key string) string {
	for i := len(a.ConfigSources) - 1; i >= 0; i-- {
		data, err := os.ReadFile(a.ConfigSources[i])
		if err != nil {
			continue
		}
		if _, err := config.GetKey(data, key); err == nil {
			return a.ConfigSources[i]
		}
	}
	return ""
}

// keyIsDefault 判断当前值是否与内置默认值一致。
//
// 刻意不走 ConfigGet：那会绕回 keyIsDefault（它由 ConfigGet 调用）——写成递归
// 就是一个不会结束的循环，而不是编译错误。
func (a *App) keyIsDefault(key string) bool {
	tpl, err := config.Marshal(a.defaultConfig())
	if err != nil {
		return false
	}
	want, err := config.GetKey(tpl, key)
	if err != nil {
		return false
	}
	effective, err := config.Marshal(a.Cfg)
	if err != nil {
		return false
	}
	got, err := config.GetKey(effective, key)
	if err != nil {
		return false
	}
	return got.Value == want.Value
}

// defaultConfig 是"内置默认值 + 派生字段"，用作类型模板与默认值比较。
func (a *App) defaultConfig() *config.Config {
	cfg := config.Defaults()
	config.ApplyDerived(&cfg, a.Paths)
	return &cfg
}

// validateConfigFile 按真实装载路径校验单个文件（含派生字段与校验规则）。
func (a *App) validateConfigFile(path string) error {
	_, err := config.Load(config.LoadOptions{
		Paths:        a.Paths,
		GOOS:         a.manifestGOOS(),
		ExplicitFile: path,
		Getenv:       getenv,
	})
	return err
}

// restoreConfig 回滚：原来没有文件就删掉新文件，否则写回原内容。
func (a *App) restoreConfig(path string, original []byte, created bool) error {
	if created && len(original) == 0 {
		return os.Remove(path)
	}
	return fsx.AtomicWriteFile(path, original, 0o644)
}

func configHeader() []byte { return config.DefaultHeader() }
