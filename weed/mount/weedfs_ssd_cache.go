package mount

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

type ssdCacheMetadata struct {
	UpdatedAt int64                    `json:"updated_at"`
	Entries   map[string]ssdCacheEntry `json:"entries"`
}

type ssdCacheEntry struct {
	Path      string  `json:"path"`
	CachePath string  `json:"cache_path"`
	Valid     bool    `json:"valid"`
	Bytes     int64   `json:"bytes"`
	Score     float64 `json:"score"`
	CachedAt  int64   `json:"cached_at,omitempty"`
	Error     string  `json:"error,omitempty"`
}

func (wfs *WFS) readFromSSDCache(path util.FullPath, offset int64, buff []byte) (int64, bool, error) {
	if len(buff) == 0 || strings.TrimSpace(wfs.option.SSDCacheMeta) == "" || strings.TrimSpace(wfs.option.SSDCacheRoot) == "" {
		return 0, false, nil
	}

	entry, ok, err := wfs.lookupSSDCache(path)
	if err != nil || !ok {
		return 0, ok, err
	}

	file, err := os.Open(entry.CachePath)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()

	n, err := file.ReadAt(buff, offset)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return int64(n), true, err
}

func (wfs *WFS) lookupSSDCache(path util.FullPath) (ssdCacheEntry, bool, error) {
	meta, err := readSSDCacheMetadata(wfs.option.SSDCacheMeta)
	if err != nil {
		return ssdCacheEntry{}, false, err
	}
	entry, ok := meta.Entries[string(path)]
	if !ok || !entry.Valid || entry.Error != "" {
		return ssdCacheEntry{}, false, nil
	}
	if !wfs.cachePathAllowed(entry.CachePath) {
		return ssdCacheEntry{}, false, nil
	}
	return entry, true, nil
}

func (wfs *WFS) invalidateSSDCache(path util.FullPath) {
	if strings.TrimSpace(wfs.option.SSDCacheMeta) == "" {
		return
	}
	unlock, err := lockSSDCacheMetadata(wfs.option.SSDCacheMeta)
	if err != nil {
		glog.Warningf("lock ssd cache metadata %s: %v", wfs.option.SSDCacheMeta, err)
		return
	}
	defer unlock()

	meta, err := readSSDCacheMetadata(wfs.option.SSDCacheMeta)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		glog.Warningf("read ssd cache metadata %s: %v", wfs.option.SSDCacheMeta, err)
		return
	}
	entry, ok := meta.Entries[string(path)]
	if !ok || !entry.Valid {
		return
	}
	entry.Valid = false
	entry.Error = "invalidated_by_write"
	meta.Entries[string(path)] = entry
	meta.UpdatedAt = time.Now().UnixNano()
	if err := writeSSDCacheMetadataLocked(wfs.option.SSDCacheMeta, meta); err != nil {
		glog.Warningf("write ssd cache metadata %s: %v", wfs.option.SSDCacheMeta, err)
	}
}

func (wfs *WFS) cachePathAllowed(path string) bool {
	root := filepath.Clean(wfs.option.SSDCacheRoot)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

func readSSDCacheMetadata(path string) (ssdCacheMetadata, error) {
	var meta ssdCacheMetadata
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	if meta.Entries == nil {
		meta.Entries = make(map[string]ssdCacheEntry)
	}
	return meta, nil
}

func writeSSDCacheMetadataLocked(path string, meta ssdCacheMetadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

func lockSSDCacheMetadata(metaPath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		return nil, err
	}
	lockPath := metaPath + ".lock"
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
