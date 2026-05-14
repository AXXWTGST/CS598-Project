package ssd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cs598/tagindex/internal/filer"
	"cs598/tagindex/internal/hotness"
)

type Cache struct {
	Root  string
	Filer *filer.Client
}

type CopyResult struct {
	Path      string  `json:"path"`
	CachePath string  `json:"cache_path"`
	Score     float64 `json:"score"`
	Bytes     int64   `json:"bytes"`
	Error     string  `json:"error,omitempty"`
}

func NewCache(root string, client *filer.Client) *Cache {
	return &Cache{Root: root, Filer: client}
}

func (c *Cache) CopyHotFiles(files []hotness.RankedFile) []CopyResult {
	results := make([]CopyResult, 0, len(files))
	for _, file := range files {
		result := CopyResult{
			Path:      file.Path,
			CachePath: c.LocalPath(file.Path),
			Score:     file.Score,
		}
		bytes, err := c.CopyFile(file.Path)
		result.Bytes = bytes
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}
	return results
}

func (c *Cache) CopyFile(path string) (int64, error) {
	if c == nil || c.Filer == nil {
		return 0, fmt.Errorf("cache filer client is required")
	}
	if strings.TrimSpace(c.Root) == "" {
		return 0, fmt.Errorf("cache root is required")
	}

	dst := c.LocalPath(path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}

	src, err := c.Filer.Download(path)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	n, err := io.Copy(tmp, src)
	if err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return n, err
	}
	removeTmp = false
	return n, nil
}

func (c *Cache) LocalPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimLeft(filepath.Clean("/"+path), string(os.PathSeparator))
	return filepath.Join(c.Root, path)
}
