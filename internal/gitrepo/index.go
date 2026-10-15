package gitrepo

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
)

// IndexEntry 是仓库索引中的轻量记录。
type IndexEntry struct{ Path, Slug, Branch string }

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
	return db, nil
}

// LoadIndex 读取索引；不存在时返回空索引。
func LoadIndex(root string) ([]IndexEntry, error) {
	db, err := openIndex(root)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT path,slug,branch FROM repos ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexEntry
	for rows.Next() {
		var e IndexEntry
		if err := rows.Scan(&e.Path, &e.Slug, &e.Branch); err != nil {
			return nil, err
		}
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
	stmt, err := tx.Prepare(`INSERT INTO repos(path,slug,branch) VALUES(?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		if _, err = stmt.Exec(e.Path, e.Slug, e.Branch); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
