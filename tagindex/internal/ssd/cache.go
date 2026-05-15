package ssd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cs598/tagindex/internal/filer"
	"cs598/tagindex/internal/hotness"
)

type Cache struct {
	Root  string
	Filer *filer.Client
}

type Metadata struct {
	UpdatedAt int64                 `json:"updated_at"`
	Entries   map[string]CacheEntry `json:"entries"`
}

type CacheEntry struct {
	Path      string  `json:"path"`
	CachePath string  `json:"cache_path"`
	Valid     bool    `json:"valid"`
	Bytes     int64   `json:"bytes"`
	Score     float64 `json:"score"`
	CachedAt  int64   `json:"cached_at,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type CopyResult struct {
	Path      string  `json:"path"`
	CachePath string  `json:"cache_path"`
	Score     float64 `json:"score"`
	Bytes     int64   `json:"bytes"`
	Error     string  `json:"error,omitempty"`
}

type EvictResult struct {
	Path      string `json:"path"`
	CachePath string `json:"cache_path"`
	Deleted   bool   `json:"deleted"`
	Error     string `json:"error,omitempty"`
}

func NewCache(root string, client *filer.Client) *Cache {
	return &Cache{Root: root, Filer: client}
}

func (c *Cache) CopyHotFiles(files []hotness.RankedFile) []CopyResult {
	results, _ := c.CopyHotFilesWithEviction(files, false)
	return results
}

func (c *Cache) CopyHotFilesWithEviction(files []hotness.RankedFile, evictStale bool) ([]CopyResult, []EvictResult) {
	results := make([]CopyResult, 0, len(files))
	updates := make(map[string]CacheEntry)
	keep := make(map[string]struct{}, len(files))
	now := time.Now().UnixNano()

	for _, file := range files {
		keep[file.Path] = struct{}{}
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
		updates[file.Path] = CacheEntry{
			Path:      file.Path,
			CachePath: result.CachePath,
			Valid:     err == nil,
			Bytes:     bytes,
			Score:     file.Score,
			CachedAt:  now,
			Error:     result.Error,
		}
		results = append(results, result)
	}
	evicted, err := c.ApplyMetadataUpdates(updates, now, keep, evictStale)
	if err != nil {
		results = append(results, CopyResult{
			Path:  "__metadata__",
			Error: err.Error(),
		})
	}
	return results, evicted
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

func (c *Cache) MetadataPath() string {
	return filepath.Join(c.Root, "cache.meta.json")
}

func (c *Cache) LoadMetadata() (Metadata, error) {
	var meta Metadata
	data, err := os.ReadFile(c.MetadataPath())
	if os.IsNotExist(err) {
		meta.Entries = make(map[string]CacheEntry)
		return meta, nil
	}
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	if meta.Entries == nil {
		meta.Entries = make(map[string]CacheEntry)
	}
	return meta, nil
}

func (c *Cache) SaveMetadata(meta Metadata) error {
	if strings.TrimSpace(c.Root) == "" {
		return fmt.Errorf("cache root is required")
	}
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		return err
	}
	unlock, err := c.lockMetadata()
	if err != nil {
		return err
	}
	defer unlock()

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(c.Root, "cache.meta.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, c.MetadataPath()); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

func (c *Cache) ApplyMetadataUpdates(updates map[string]CacheEntry, updatedAt int64, keep map[string]struct{}, evictStale bool) ([]EvictResult, error) {
	if strings.TrimSpace(c.Root) == "" {
		return nil, fmt.Errorf("cache root is required")
	}
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		return nil, err
	}
	unlock, err := c.lockMetadata()
	if err != nil {
		return nil, err
	}
	defer unlock()

	meta, err := c.loadMetadataNoLock()
	if err != nil {
		return nil, err
	}
	for path, entry := range updates {
		meta.Entries[path] = entry
	}
	evicted := make([]EvictResult, 0)
	if evictStale {
		for path, entry := range meta.Entries {
			if _, ok := keep[path]; ok {
				continue
			}
			if !entry.Valid && entry.Error == "evicted_stale" {
				continue
			}
			entry.Valid = false
			entry.Error = "evicted_stale"
			meta.Entries[path] = entry

			result := EvictResult{Path: path, CachePath: entry.CachePath}
			if c.cachePathAllowed(entry.CachePath) {
				if err := os.Remove(entry.CachePath); err != nil && !os.IsNotExist(err) {
					result.Error = err.Error()
				} else {
					result.Deleted = true
				}
			}
			evicted = append(evicted, result)
		}
	}
	meta.UpdatedAt = updatedAt
	return evicted, c.writeMetadataLocked(meta)
}

func (c *Cache) cachePathAllowed(path string) bool {
	root := filepath.Clean(c.Root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

func (c *Cache) loadMetadataNoLock() (Metadata, error) {
	var meta Metadata
	data, err := os.ReadFile(c.MetadataPath())
	if os.IsNotExist(err) {
		meta.Entries = make(map[string]CacheEntry)
		return meta, nil
	}
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	if meta.Entries == nil {
		meta.Entries = make(map[string]CacheEntry)
	}
	return meta, nil
}

func (c *Cache) writeMetadataLocked(meta Metadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(c.Root, "cache.meta.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, c.MetadataPath()); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

func (c *Cache) lockMetadata() (func(), error) {
	lockPath := c.MetadataPath() + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
