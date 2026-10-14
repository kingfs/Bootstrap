package detect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// CacheTTL 是探测结果的默认有效期。
//
// 缓存只服务于"显示"：10 分钟内重复执行 `bs tool list` 不必反复起子进程。
// 任何"因为看起来已安装所以要跳过安装"的判断都必须绕过缓存。
const CacheTTL = 10 * time.Minute

// Cache 把探测结果持久化到一个小 JSON 文件。
//
// 放在 cache 目录而不是 state 目录：它是可随时删除的加速数据，
// 删掉只会让下一次探测变慢，不会影响任何语义。
type Cache struct {
	Path string
	TTL  time.Duration
	Now  func() time.Time
}

type cacheFile struct {
	Version int               `json:"version"`
	Results map[string]Result `json:"results"`
}

func (c *Cache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return CacheTTL
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Load 读取未过期的缓存项。文件不存在或损坏时返回空表（缓存不值得报错）。
func (c *Cache) Load() (map[string]Result, error) {
	if c == nil || c.Path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read probe cache: %w", err)
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse probe cache: %w", err)
	}
	cutoff := c.now().Add(-c.ttl())
	out := make(map[string]Result, len(cf.Results))
	for id, r := range cf.Results {
		if r.CheckedAt.After(cutoff) {
			out[id] = r
		}
	}
	return out, nil
}

// Merge 把新结果合并进缓存文件（保留未过期的旧项）。
//
// 写入用 WriteFileIfChanged：内容没变就不会触碰文件 mtime，
// 这也让"重复执行 bs tool list 不产生任何写盘"成为可断言的性质。
func (c *Cache) Merge(fresh map[string]Result) error {
	if c == nil || c.Path == "" {
		return nil
	}
	existing, _ := c.Load()
	if existing == nil {
		existing = map[string]Result{}
	}
	for id, r := range fresh {
		r.Cached = false
		existing[id] = r
	}
	if err := fsx.EnsureDir(dirOf(c.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cacheFile{Version: 1, Results: existing}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode probe cache: %w", err)
	}
	if _, err := fsx.WriteFileIfChanged(c.Path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write probe cache: %w", err)
	}
	return nil
}

// InvalidateTools 只丢弃指定工具的缓存项（装/卸之后用它，不必把整份缓存删掉）。
//
// 过期项不会被写回：Load 本身就会过滤掉它们，顺带做了一次清理。
func (c *Cache) InvalidateTools(ids ...string) error {
	if c == nil || c.Path == "" || len(ids) == 0 {
		return nil
	}
	existing, err := c.Load()
	if err != nil || existing == nil {
		// 读不出来（或本来就没有）时退回整体删除，结果一样是"下次重新探测"。
		return c.Invalidate()
	}
	changed := false
	for _, id := range ids {
		if _, ok := existing[id]; ok {
			delete(existing, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := fsx.EnsureDir(dirOf(c.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cacheFile{Version: 1, Results: existing}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode probe cache: %w", err)
	}
	if _, err := fsx.WriteFileIfChanged(c.Path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write probe cache: %w", err)
	}
	return nil
}

// Invalidate 删除缓存文件（`--refresh` 之外的显式失效入口）。
func (c *Cache) Invalidate() error {
	if c == nil || c.Path == "" {
		return nil
	}
	if err := os.Remove(c.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
