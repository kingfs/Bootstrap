package install

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kingfs/Bootstrap/internal/manifest"
)

// RenderData 是模板渲染可用的变量（docs/design.md §7.3）。
type RenderData struct {
	Version string
	Tag     string
	OS      string
	Arch    string
	Package string
	Manager string
	Crate   string
	Module  string
	Python  string
	Action  string
	// Versions 对应配置 tool.versions（模板里写 {{versions.golang}}）。
	Versions map[string]string
}

var placeholder = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// Render 展开清单模板。
//
// 支持：
//
//	{{version}} {{tag}} {{os}} {{arch}} {{package}} {{manager}} {{crate}} {{module}} {{python}} {{action}}
//	{{versions.golang}}
//	{{arch|archmap:amd64=x86_64,arm64=aarch64}}     {{os|osmap:darwin=macos}}
//	{{version|trimv}}                               {{version|default:latest}}
//
// 未知变量一律报错（而不是渲染成空串）——清单里写错变量名是最常见的失误，
// 必须在 `bs tool manifest validate` 阶段就暴露。
func Render(tpl string, d RenderData) (string, error) {
	var firstErr error
	out := placeholder.ReplaceAllStringFunc(tpl, func(m string) string {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(m, "{{"), "}}"))
		val, err := renderExpr(inner, d)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return m
		}
		return val
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// ContainsTemplate 报告字符串里是否含模板占位符。
func ContainsTemplate(s string) bool { return placeholder.MatchString(s) }

func renderExpr(expr string, d RenderData) (string, error) {
	parts := strings.Split(expr, "|")
	val, err := renderVar(strings.TrimSpace(parts[0]), d)
	if err != nil {
		return "", err
	}
	for _, f := range parts[1:] {
		val, err = applyFilter(strings.TrimSpace(f), val)
		if err != nil {
			return "", err
		}
	}
	return val, nil
}

func renderVar(name string, d RenderData) (string, error) {
	switch name {
	case "version":
		return d.Version, nil
	case "tag":
		if d.Tag != "" {
			return d.Tag, nil
		}
		return d.Version, nil
	case "os":
		return d.OS, nil
	case "arch":
		return d.Arch, nil
	case "package":
		return d.Package, nil
	case "manager":
		return d.Manager, nil
	case "crate":
		return d.Crate, nil
	case "module":
		return d.Module, nil
	case "python":
		return d.Python, nil
	case "action":
		return d.Action, nil
	}
	if key, ok := strings.CutPrefix(name, "versions."); ok {
		if v, ok := d.Versions[key]; ok {
			return v, nil
		}
		return "", fmt.Errorf("template variable {{versions.%s}} is not set in tool.versions", key)
	}
	return "", fmt.Errorf("unknown template variable {{%s}}", name)
}

func applyFilter(spec, val string) (string, error) {
	name, arg, hasArg := strings.Cut(spec, ":")
	switch name {
	case "trimv":
		return strings.TrimPrefix(val, "v"), nil
	case "default":
		if !hasArg {
			return "", fmt.Errorf("filter default requires a value ({{...|default:X}})")
		}
		if val == "" || val == "latest" {
			return arg, nil
		}
		return val, nil
	case "archmap", "osmap":
		if !hasArg {
			return "", fmt.Errorf("filter %s requires a mapping ({{...|%s:amd64=x86_64}})", name, name)
		}
		for _, pair := range strings.Split(arg, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok {
				return "", fmt.Errorf("filter %s: bad mapping %q (want from=to)", name, pair)
			}
			if strings.EqualFold(strings.TrimSpace(k), val) {
				return strings.TrimSpace(v), nil
			}
		}
		return val, nil // 没有映射时保持原值
	case "lower":
		return strings.ToLower(val), nil
	case "upper":
		return strings.ToUpper(val), nil
	case "":
		return "", fmt.Errorf("empty filter in template")
	}
	return "", fmt.Errorf("unknown template filter %q", name)
}

// sampleRenderData 用于清单校验：把所有模板用样例数据渲染一遍，
// 从而在 validate 阶段就能发现拼错的变量名或非法的过滤器。
func sampleRenderData(versions map[string]string) RenderData {
	merged := map[string]string{"golang": "1.24.1", "nodejs": "22.1.0", "python": "3.12.0", "rust": "stable"}
	for k, v := range versions {
		merged[k] = v
	}
	return RenderData{
		Version: "1.2.3", Tag: "v1.2.3", OS: "linux", Arch: "amd64",
		Package: "pkg", Manager: "g", Crate: "crate", Module: "example.com/m",
		Python: "3.12", Action: "install",
		Versions: merged,
	}
}

// validateTemplates 把 params 里所有可能含模板的字符串渲染一遍，
// 提前暴露写错的变量名（这是清单最容易出错的地方）。
func validateTemplates(p manifest.Params, versions map[string]string) error {
	for _, field := range p.TemplateFields() {
		if !ContainsTemplate(field.Value) {
			continue
		}
		if _, err := Render(field.Value, sampleRenderData(versions)); err != nil {
			return fmt.Errorf("%s: %w", field.Name, err)
		}
	}
	return nil
}

// nameParamFields 是"会被当作值塞进命令模板"的参数。
//
// 为什么需要提前展开：清单里写 `version: "{{versions.golang}}"` 是让
// "用哪个 Go 版本"由配置（而不是清单）决定；而安装器把 params.version
// 当成**值**再插进命令模板的 {{version}}。不提前展开，命令里就会出现
// 字面量 `g install {{versions.golang}}`。
//
// 反过来 asset/url/sha256/dest/build 这些必须在真实版本号确定之后由各自的
// Installer 渲染（它们的模板本来就引用 {{version}}），绝不能在这里提前渲染。
var nameParamFields = []struct {
	Name  string
	Value func(*manifest.Params) *string
}{
	{"params.version", func(p *manifest.Params) *string { return &p.Version }},
	{"params.package", func(p *manifest.Params) *string { return &p.Package }},
	{"params.formula", func(p *manifest.Params) *string { return &p.Formula }},
	{"params.cask", func(p *manifest.Params) *string { return &p.Cask }},
	{"params.crate", func(p *manifest.Params) *string { return &p.Crate }},
	{"params.module", func(p *manifest.Params) *string { return &p.Module }},
	{"params.id", func(p *manifest.Params) *string { return &p.ID }},
	{"params.python", func(p *manifest.Params) *string { return &p.Python }},
	{"params.version_source", func(p *manifest.Params) *string { return &p.VersionSource }},
	{"params.source", func(p *manifest.Params) *string { return &p.Source }},
	{"params.repo", func(p *manifest.Params) *string { return &p.Repo }},
}

// resolveNameParams 原地展开"名字类"参数里的模板，返回是否有改动。
//
// 渲染结果为空（例如写了 `{{version}}` 这种只有安装时才成立的引用）时保持原样，
// 让后续流程按原值报错，而不是悄悄把参数变成空字符串。
func resolveNameParams(p *manifest.Params, data RenderData) (bool, error) {
	changed := false
	for _, f := range nameParamFields {
		ptr := f.Value(p)
		if !ContainsTemplate(*ptr) {
			continue
		}
		out, err := Render(*ptr, data)
		if err != nil {
			return changed, fmt.Errorf("%s: %w", f.Name, err)
		}
		if out == "" {
			continue
		}
		*ptr = out
		changed = true
	}
	return changed, nil
}
