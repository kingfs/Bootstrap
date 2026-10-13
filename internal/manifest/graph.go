package manifest

import (
	"fmt"
	"strings"
)

// Problems 汇总一次校验中发现的全部问题。
//
// 校验的价值在于"一次把错找全"，而不是让用户改一个跑一次，
// 所以 Validate 返回的是这个聚合类型而不是第一个错误。
type Problems struct {
	Items []string
}

// Add 追加一条问题。
func (p *Problems) Add(format string, args ...any) {
	p.Items = append(p.Items, fmt.Sprintf(format, args...))
}

// Len 返回问题数量。
func (p *Problems) Len() int { return len(p.Items) }

// Err 在没有问题时返回 nil，否则返回自身。
func (p *Problems) Err() error {
	if len(p.Items) == 0 {
		return nil
	}
	return p
}

// Error 实现 error。
func (p *Problems) Error() string {
	if len(p.Items) == 1 {
		return p.Items[0]
	}
	return fmt.Sprintf("%d 个问题：\n  - %s", len(p.Items), strings.Join(p.Items, "\n  - "))
}

// TopoOrder 按 depends_on 做拓扑排序，同层保持清单书写顺序。
//
// 顺序即策略：`g` 必须先于 `golang`，`fnm` 必须先于 `nodejs`。
func TopoOrder(tools []Tool) ([]Tool, error) {
	byID := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byID[t.ID] = t
	}

	visited := map[string]int{} // 0=未访问 1=访问中 2=完成
	var order []Tool
	var stack []string

	var visit func(t Tool) error
	visit = func(t Tool) error {
		switch visited[t.ID] {
		case 2:
			return nil
		case 1:
			// 找到环：把环上的路径打印出来，便于直接定位。
			start := 0
			for i, id := range stack {
				if id == t.ID {
					start = i
					break
				}
			}
			cycle := append(append([]string{}, stack[start:]...), t.ID)
			return fmt.Errorf("dependency cycle: %s", strings.Join(cycle, " → "))
		}
		visited[t.ID] = 1
		stack = append(stack, t.ID)
		for _, dep := range t.DependsOn {
			d, ok := byID[dep]
			if !ok {
				// 引用不存在的 id 由 Validate 报告；这里跳过以免掩盖真正的问题。
				continue
			}
			if err := visit(d); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		visited[t.ID] = 2
		order = append(order, t)
		return nil
	}

	for _, t := range tools {
		if err := visit(t); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// ExpandDeps 把 ids 扩展为"这些工具 + 它们的全部传递依赖"，并按拓扑序返回。
//
// 抽取顺序与清单顺序无关：只要声明了 depends_on，就不会出现"先装 runtime 再装管理器"。
func ExpandDeps(l *Loaded, ids []string) ([]Tool, error) {
	seen := map[string]bool{}
	var out []Tool

	var add func(id string) error
	add = func(id string) error {
		if seen[id] {
			return nil
		}
		t, ok := l.ByID(id)
		if !ok {
			return fmt.Errorf("unknown tool %q", id)
		}
		seen[id] = true
		for _, dep := range t.DependsOn {
			if err := add(dep); err != nil {
				return err
			}
		}
		out = append(out, t)
		return nil
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	return TopoOrder(out)
}
