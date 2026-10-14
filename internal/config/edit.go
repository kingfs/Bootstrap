package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// 本文件是 `bs config get|set|unset` 的底座：在 YAML 文档上按"点分路径"读写单个键。
//
// 为什么用 yaml.Node 而不是"反序列化成 map 再序列化"：后者会把用户的注释、
// 键顺序、空行全部丢掉——用一个命令改一个值却重写了整份配置，是很容易被抱怨的行为。
// yaml.v3 的 Node 能保留注释与顺序，代价是遍历要自己写。

// ErrKeyNotFound 表示路径在文档里不存在（用于 `get` 与拼写建议）。
var ErrKeyNotFound = errors.New("config key not found")

// ErrKeyNotScalar 表示目标位置是映射/序列，不能直接当标量赋值。
var ErrKeyNotScalar = errors.New("config key is not a scalar")

// EditOptions 控制 SetKey 的行为。
type EditOptions struct {
	// Template 是"类型模板"（通常是内置默认配置的 YAML）。
	// 有它在，`ui.confirm` 就只接受 true/false、`repo.clone.depth` 只接受整数，
	// 类型写错在**写入前**就能报出来，而不是等下一次 bs 启动才炸。
	Template []byte
	// Unset 为真时删除该键（回到"由默认值/上层配置决定"的状态）。
	Unset bool
}

// KeyValue 是一次读取的结果。
type KeyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Kind  string `json:"kind"` // scalar | list | map
}

// GetKey 读取点分路径的值。标量原样返回，列表/映射返回 YAML 文本。
func GetKey(data []byte, key string) (KeyValue, error) {
	doc, err := parseDoc(data)
	if err != nil {
		return KeyValue{}, err
	}
	segments := splitKey(key)
	if len(segments) == 0 {
		return KeyValue{}, fmt.Errorf("%w: empty key", ErrKeyNotFound)
	}
	node, err := lookup(body(doc), segments)
	if err != nil {
		return KeyValue{}, err
	}
	out := KeyValue{Key: key, Kind: kindOf(node)}
	if out.Kind == "scalar" {
		out.Value = node.Value
		return out, nil
	}
	text, err := marshalNode(node)
	if err != nil {
		return KeyValue{}, err
	}
	out.Value = strings.TrimRight(text, "\n")
	return out, nil
}

// SetKey 写入（或按 opt.Unset 删除）点分路径的键，返回新文档与旧值。
//
// 旧值用于"改了什么"的回显；路径中不存在的中间层会被自动创建（例如
// `proxy.profiles.office.http` 会补出 profiles.office 两层映射）。
func SetKey(data []byte, key, value string, opt EditOptions) ([]byte, string, error) {
	segments := splitKey(key)
	if len(segments) == 0 {
		return nil, "", fmt.Errorf("invalid key: %q", key)
	}
	doc, err := parseDoc(data)
	if err != nil {
		return nil, "", err
	}
	current, lerr := lookup(body(doc), segments)
	if lerr != nil && !errors.Is(lerr, ErrKeyNotFound) {
		return nil, "", lerr
	}
	old := ""
	if lerr == nil {
		if current.Kind == yaml.ScalarNode {
			old = current.Value
		} else if text, merr := marshalNode(current); merr == nil {
			old = strings.TrimRight(text, "\n")
		}
	}

	var opErr error
	if opt.Unset {
		opErr = deletePath(body(doc), segments)
	} else {
		opErr = setPath(body(doc), segments, value, opt.Template)
	}
	if opErr != nil {
		return nil, old, opErr
	}
	out, err := marshalNode(doc)
	if err != nil {
		return nil, old, err
	}
	return []byte(out), old, nil
}

// Keys 列出文档里所有键路径（叶子优先，映射本身也列出）。
func Keys(data []byte) ([]string, error) {
	doc, err := parseDoc(data)
	if err != nil {
		return nil, err
	}
	var out []string
	collectKeys(body(doc), "", &out)
	sort.Strings(out)
	return out, nil
}

// SuggestKeys 给出与 key 最接近的一组键路径（拼错时的提示）。
func SuggestKeys(keys []string, key string) []string {
	lower := strings.ToLower(key)
	var prefix, fuzzy []string
	for _, k := range keys {
		switch {
		case strings.HasPrefix(strings.ToLower(k), lower):
			prefix = append(prefix, k)
		case similar(lower, strings.ToLower(k)):
			fuzzy = append(fuzzy, k)
		}
	}
	out := append(prefix, fuzzy...)
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// similar 判断两个键路径是否"像到值得推荐"。
//
// 不做通用的模糊搜索：只有"整串编辑距离很小"或"最后一段很像"才提示，
// 免得 `config get ui.confrim` 收到一堆无关候选。
func similar(a, b string) bool {
	if levenshtein(a, b) <= 2 {
		return true
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if i == len(as)-1 {
			continue
		}
		if as[i] != bs[i] {
			return false
		}
	}
	return levenshtein(as[len(as)-1], bs[len(bs)-1]) <= 2
}

// levenshtein 是标准编辑距离（键很短，O(n*m) 足够）。
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func parseDoc(data []byte) (*yaml.Node, error) {
	doc := &yaml.Node{}
	if len(strings.TrimSpace(string(data))) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
		return doc, nil
	}
	if err := yaml.Unmarshal(data, doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind == 0 { // 空文档
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	return doc, nil
}

// body 返回文档的根节点（映射）。
func body(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func splitKey(key string) []string {
	var out []string
	for _, seg := range strings.Split(key, ".") {
		if seg = strings.TrimSpace(seg); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func lookup(root *yaml.Node, segments []string) (*yaml.Node, error) {
	node := root
	for i, seg := range segments {
		var child *yaml.Node
		switch node.Kind {
		case yaml.MappingNode:
			child = mapChild(node, seg)
		case yaml.SequenceNode:
			// 列表按数字下标访问：`config get repo.hosts.0`。Keys() 也按这个形式产出路径。
			if idx, err := strconv.Atoi(seg); err == nil && idx >= 0 && idx < len(node.Content) {
				child = node.Content[idx]
			}
		}
		if child == nil {
			return nil, fmt.Errorf("%w: %s", ErrKeyNotFound, strings.Join(segments[:i+1], "."))
		}
		node = child
	}
	return node, nil
}

func mapChild(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func mapKeyNode(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i]
		}
	}
	return nil
}

// setPath 逐段下沉，必要时补出中间映射。
func setPath(root *yaml.Node, segments []string, value string, template []byte) error {
	node := root
	for i, seg := range segments {
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("%w: %s 不是映射，无法设置其子键", ErrKeyNotScalar, strings.Join(segments[:i], "."))
		}
		last := i == len(segments)-1
		if last {
			expected := templateKind(template, segments)
			newNode, err := buildValue(value, expected)
			if err != nil {
				return fmt.Errorf("%s: %w", strings.Join(segments, "."), err)
			}
			if existing := mapChild(node, seg); existing != nil {
				// 保留原有注释（HeadComment/LineComment/FootComment）与位置。
				newNode.HeadComment = existing.HeadComment
				newNode.LineComment = existing.LineComment
				newNode.FootComment = existing.FootComment
				replaceChild(node, seg, newNode)
				return nil
			}
			node.Content = append(node.Content, scalar(seg), newNode)
			return nil
		}
		child := mapChild(node, seg)
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			node.Content = append(node.Content, scalar(seg), child)
		}
		node = child
	}
	return nil
}

func replaceChild(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
}

func deletePath(root *yaml.Node, segments []string) error {
	node := root
	for i, seg := range segments {
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("%w: %s", ErrKeyNotFound, strings.Join(segments[:i], "."))
		}
		if i == len(segments)-1 {
			if mapKeyNode(node, seg) == nil {
				return fmt.Errorf("%w: %s", ErrKeyNotFound, strings.Join(segments, "."))
			}
			out := node.Content[:0]
			for j := 0; j+1 < len(node.Content); j += 2 {
				if node.Content[j].Value == seg {
					continue
				}
				out = append(out, node.Content[j], node.Content[j+1])
			}
			node.Content = out
			return nil
		}
		child := mapChild(node, seg)
		if child == nil {
			return fmt.Errorf("%w: %s", ErrKeyNotFound, strings.Join(segments[:i+1], "."))
		}
		node = child
	}
	return nil
}

func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// buildValue 按模板推断的类型把字符串转成 YAML 节点。
//
// expected 为空（模板里没有这个键）时按 YAML 标量规则自动判断：
// `true` 变布尔、`123` 变整数，其余当字符串。
func buildValue(value, expected string) (*yaml.Node, error) {
	if expected == "!!map" || expected == "!!seq" {
		node := &yaml.Node{}
		if err := yaml.Unmarshal([]byte(value), node); err != nil {
			return nil, fmt.Errorf("期望%s，但无法解析为 YAML: %w", kindLabel(expected), err)
		}
		target := body(node)
		if target.Kind == 0 || (expected == "!!map" && target.Kind != yaml.MappingNode) ||
			(expected == "!!seq" && target.Kind != yaml.SequenceNode) {
			return nil, fmt.Errorf("期望%s，收到 %s", kindLabel(expected), strings.TrimSpace(value))
		}
		return target, nil
	}
	if expected == "" {
		return autoScalar(value), nil
	}
	if !scalarMatches(expected, value) {
		if expected == "!!bool" {
			return nil, fmt.Errorf("期望布尔值（true/false），收到 %q", value)
		}
		return nil, fmt.Errorf("期望%s，收到 %q", kindLabel(expected), value)
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: expected, Value: normalizeScalar(expected, value)}, nil
}

func autoScalar(value string) *yaml.Node {
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte(value), node); err == nil {
		target := body(node)
		if target.Kind == yaml.ScalarNode && target.Tag != "!!null" {
			return target
		}
		if target.Kind == yaml.MappingNode || target.Kind == yaml.SequenceNode {
			return target
		}
	}
	return scalar(value)
}

func scalarMatches(tag, value string) bool {
	switch tag {
	case "!!bool":
		_, err := strconv.ParseBool(strings.ToLower(value))
		return err == nil
	case "!!int":
		_, err := strconv.Atoi(value)
		return err == nil
	case "!!float":
		_, err := strconv.ParseFloat(value, 64)
		return err == nil
	default:
		return true // 字符串没有"格式"可言
	}
}

func normalizeScalar(tag, value string) string {
	if tag == "!!bool" {
		return strings.ToLower(value)
	}
	return value
}

func kindLabel(tag string) string {
	switch tag {
	case "!!bool":
		return "布尔值"
	case "!!int":
		return "整数"
	case "!!float":
		return "小数"
	case "!!seq":
		return "列表（YAML/JSON 数组）"
	case "!!map":
		return "映射（YAML/JSON 对象）"
	default:
		return "字符串"
	}
}

// templateKind 在默认配置里找同一个键，返回它的 YAML 标签（"!!bool" 等）。
// 找不到返回空串（交给自动判断）。
func templateKind(template []byte, segments []string) string {
	if len(template) == 0 {
		return ""
	}
	node := &yaml.Node{}
	if err := yaml.Unmarshal(template, node); err != nil {
		return ""
	}
	found, err := lookup(body(node), segments)
	if err != nil {
		return ""
	}
	if found.Kind != yaml.ScalarNode {
		return found.Tag
	}
	return found.Tag
}

func kindOf(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "map"
	case yaml.SequenceNode:
		return "list"
	default:
		return "scalar"
	}
}

func marshalNode(node *yaml.Node) (string, error) {
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return "", fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("encode config: %w", err)
	}
	return sb.String(), nil
}

func collectKeys(node *yaml.Node, prefix string, out *[]string) {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			*out = append(*out, path)
			collectKeys(node.Content[i+1], path, out)
		}
	case yaml.SequenceNode:
		for i, item := range node.Content {
			collectKeys(item, fmt.Sprintf("%s.%d", prefix, i), out)
		}
	}
}
