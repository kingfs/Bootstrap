package gitrepo

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// IndexEntry 是仓库索引中的轻量记录。
type IndexEntry struct {
	Path, Slug, Branch string
	DefaultBranch      string
	Worktree           bool
}

func indexPath(root string) string { return filepath.Join(root, ".bootstrap", "repos.db") }

func openIndex(root string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(indexPath(root)), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", indexPath(root))
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS repos (path TEXT PRIMARY KEY, slug TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL DEFAULT (unixepoch()))`)
	if err != nil {
		db.Close()
		return nil, err
	}
	// 兼容旧数据库；仅写入路径允许升级 schema，只读 list 不修改数据库。
	rows, err := db.Query(`PRAGMA table_info(repos)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			db.Close()
			return nil, err
		}
		cols[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, col := range []string{"default_branch", "worktree"} {
		if !cols[col] {
			if _, err := db.Exec(`ALTER TABLE repos ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	return db, nil
}

// ErrNoIndex 表示需要先显式扫描，读取索引不能偷偷遍历用户目录。
var ErrNoIndex = errors.New("repository index missing; run `bs repo scan` first")

// LoadIndex 只读索引，不创建目录或数据库。
func LoadIndex(root string) ([]IndexEntry, error) {
	if _, err := os.Stat(indexPath(root)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoIndex
		}
		return nil, fmt.Errorf("stat repository index: %w", err)
	}
	absolute, err := filepath.Abs(indexPath(root))
	if err != nil {
		return nil, fmt.Errorf("resolve repository index: %w", err)
	}
	uriPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}).String())
	if err != nil {
		return nil, fmt.Errorf("open repository index: %w", err)
	}
	defer db.Close()
	// 旧索引仍可只读，扩展字段在下次 scan/get 时补齐。
	query := `SELECT path,slug,branch FROM repos ORDER BY path`
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('repos') WHERE name IN ('default_branch','worktree')`).Scan(&count); err != nil {
		return nil, err
	}
	if count == 2 {
		query = `SELECT path,slug,branch,default_branch,worktree FROM repos ORDER BY path`
	}
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexEntry
	for rows.Next() {
		var e IndexEntry
		dest := []any{&e.Path, &e.Slug, &e.Branch}
		var worktree string
		if count == 2 {
			dest = append(dest, &e.DefaultBranch, &worktree)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		e.Worktree = worktree == "true"
		out = append(out, e)
	}
	return out, rows.Err()
}

// SaveIndex 以事务方式重建索引。
func SaveIndex(root string, entries []IndexEntry) error {
	db, err := openIndex(root)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM repos`); err != nil {
		tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO repos(path,slug,branch,default_branch,worktree) VALUES(?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		if _, err = stmt.Exec(e.Path, e.Slug, e.Branch, e.DefaultBranch, fmt.Sprint(e.Worktree)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
