package gitrepo

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IndexEntry 是仓库索引中的轻量记录；每行仅保存路径、slug 和分支。
type IndexEntry struct{ Path, Slug, Branch string }

func indexPath(root string) string { return filepath.Join(root, ".bootstrap", "repos.tsv") }

// LoadIndex 读取索引；不存在时返回空索引。
func LoadIndex(root string) ([]IndexEntry, error) {
	f, err := os.Open(indexPath(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []IndexEntry
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Split(s.Text(), "\t")
		if len(p) >= 1 && p[0] != "" {
			e := IndexEntry{Path: p[0]}
			if len(p) > 1 {
				e.Slug = p[1]
			}
			if len(p) > 2 {
				e.Branch = p[2]
			}
			out = append(out, e)
		}
	}
	return out, s.Err()
}

// SaveIndex 原子写入紧凑索引。
func SaveIndex(root string, entries []IndexEntry) error {
	dir := filepath.Dir(indexPath(root))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	tmp, err := os.CreateTemp(dir, "repos-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	for _, e := range entries {
		if _, err = fmt.Fprintf(tmp, "%s\t%s\t%s\n", e.Path, e.Slug, e.Branch); err != nil {
			tmp.Close()
			return err
		}
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, indexPath(root))
}

func UpsertIndex(root string, entries []IndexEntry) error { return SaveIndex(root, entries) }
