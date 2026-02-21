package filter

import (
	"path/filepath"
	"strings"
)

var IgnoredDirs = []string{
	".git",
	".idea",
	".vscode",
	"dist",
	"build",
	"bin",
}

var IgnoredExtensions = []string{
	".tmp",
	".swp",
	".log",
}

func ShouldIgnore(path string) bool {
	clean := filepath.Clean(path)

	for _, dir := range IgnoredDirs {
		if strings.Contains(clean, string(filepath.Separator)+dir+string(filepath.Separator)) {
			return true
		}
	}

	for _, ext := range IgnoredExtensions {
		if strings.HasSuffix(clean, ext) {
			return true
		}
	}

	return false
}
