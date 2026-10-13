package shellsnip

import (
	"fmt"
	"os"
)

func isNotExist(err error) bool { return os.IsNotExist(err) }

func readDirIfExists(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user snippet dir %s: %w", dir, err)
	}
	return entries, nil
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
