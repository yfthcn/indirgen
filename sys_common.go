package main

import (
	"os"
	"path/filepath"
	"strings"
)

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func parentDir(dir string) string {
	p := filepath.Dir(dir)
	if p == dir {
		return dir
	}
	return p
}

// isWithin reports whether p is root itself or below it. Both paths must be
// absolute (or both relative); a sibling like "/lib2" is not within "/lib".
func isWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
