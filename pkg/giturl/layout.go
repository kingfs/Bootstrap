package giturl

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"text/template"
)

// LayoutData 是仓库目录布局模板可用的字段。
//
// 默认布局为 `{{.Host}}/{{.Owner}}/{{.Repo}}`，与维护者现有的
// /data/src/github.com/kingfs/Bootstrap 习惯一致。
type LayoutData struct {
	Host    string
	Owner   string
	Repo    string
	Ref     string
	SubPath string
}

// RenderLayout 渲染目录布局模板，返回**相对**路径（已清理、禁止越界）。
func RenderLayout(layout string, r Ref) (string, error) {
	rel, err := RenderLayoutData(layout, LayoutData{
		Host:    r.Host,
		Owner:   r.Owner,
		Repo:    r.Repo,
		Ref:     r.Ref,
		SubPath: r.SubPath,
	})
	if err != nil {
		return "", err
	}
	return rel, nil
}

// RenderLayoutData 渲染布局模板（纯函数，便于校验与测试）。
func RenderLayoutData(layout string, data LayoutData) (string, error) {
	t, err := template.New("layout").Option("missingkey=error").Parse(layout)
	if err != nil {
		return "", fmt.Errorf("invalid repo layout %q: %w", layout, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render repo layout %q: %w", layout, err)
	}
	out := strings.TrimSpace(buf.String())
	out = strings.Trim(out, "/")
	out = path.Clean(out)
	if out == "." || out == "" {
		return "", fmt.Errorf("repo layout %q produced an empty path", layout)
	}
	if out == ".." || strings.HasPrefix(out, "../") {
		return "", fmt.Errorf("repo layout %q escapes the repository root", layout)
	}
	return out, nil
}
