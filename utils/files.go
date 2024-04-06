package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type File struct {
	info os.FileInfo
	path string
}

// Implements sort interface to sort by timestamp
type ByTime []File

func (b ByTime) Len() int {
	return len(b)
}

func (b ByTime) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b ByTime) Less(i, j int) bool {
	return b[i].info.ModTime().Unix() > b[j].info.ModTime().Unix()
}

// Gets most recent file from dir.
// Also allows you to search with specific match.
func GetRecentFile(path string, prefix string) (string, error) {
	// Build up files array
	files := ByTime{}
	walkfn := func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() && strings.HasPrefix(info.Name(), prefix) {
			files = append(files, File{info, path})
		}
		return nil
	}
	err := filepath.Walk(path, walkfn)
	if err != nil {
		return "", fmt.Errorf("failed to read dir.: %v", err)
	}

	if len(files) == 0 {
		return "", fmt.Errorf("no files found in dir")
	}
	sort.Sort(files)

	return files[0].path, nil
}
