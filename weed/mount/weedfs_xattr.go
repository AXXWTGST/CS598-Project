//go:build !freebsd

package mount

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	sys "golang.org/x/sys/unix"
)

const (
	// https://man7.org/linux/man-pages/man7/xattr.7.html#:~:text=The%20VFS%20imposes%20limitations%20that,in%20listxattr(2)).
	MAX_XATTR_NAME_SIZE  = 255
	MAX_XATTR_VALUE_SIZE = 65536
	XATTR_PREFIX         = "xattr-" // same as filer
	TAG_XATTR_NAME       = "user.tags"
	TAG_VERSION_XATTR    = "user.tag_version"
	SEAWEED_TAGS_KEY     = "Seaweed-Tags"
	SEAWEED_TAG_VER_KEY  = "Seaweed-Tag-Version"
)

type tagEvent struct {
	Ts           int64    `json:"ts"`
	Source       string   `json:"source,omitempty"`
	Op           string   `json:"op"`
	Path         string   `json:"path"`
	Tags         []string `json:"tags,omitempty"`
	IndexUpdated bool     `json:"index_updated"`
}

// GetXAttr reads an extended attribute, and should return the
// number of bytes. If the buffer is too small, return ERANGE,
// with the required buffer size.
func (wfs *WFS) GetXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string, dest []byte) (size uint32, code fuse.Status) {

	if wfs.option.DisableXAttr {
		return 0, fuse.Status(syscall.ENOTSUP)
	}

	//validate attr name
	if len(attr) > MAX_XATTR_NAME_SIZE {
		if runtime.GOOS == "darwin" {
			return 0, fuse.EPERM
		} else {
			return 0, fuse.ERANGE
		}
	}
	if len(attr) == 0 {
		return 0, fuse.EINVAL
	}

	_, _, entry, status := wfs.maybeReadEntry(header.NodeId)
	if status != fuse.OK {
		return 0, status
	}
	if entry == nil {
		return 0, fuse.ENOENT
	}
	if entry.Extended == nil {
		return 0, fuse.ENOATTR
	}
	data, found := entry.Extended[xattrStorageKey(attr)]
	if !found {
		return 0, fuse.ENOATTR
	}
	if len(dest) < len(data) {
		return uint32(len(data)), fuse.ERANGE
	}
	copy(dest, data)

	return uint32(len(data)), fuse.OK
}

// SetXAttr writes an extended attribute.
// https://man7.org/linux/man-pages/man2/setxattr.2.html
//
//	 By default (i.e., flags is zero), the extended attribute will be
//	created if it does not exist, or the value will be replaced if
//	the attribute already exists.  To modify these semantics, one of
//	the following values can be specified in flags:
//
//	XATTR_CREATE
//	       Perform a pure create, which fails if the named attribute
//	       exists already.
//
//	XATTR_REPLACE
//	       Perform a pure replace operation, which fails if the named
//	       attribute does not already exist.
func (wfs *WFS) SetXAttr(cancel <-chan struct{}, input *fuse.SetXAttrIn, attr string, data []byte) fuse.Status {

	if wfs.option.DisableXAttr {
		return fuse.Status(syscall.ENOTSUP)
	}

	if wfs.IsOverQuotaWithUncommitted() {
		return fuse.Status(syscall.ENOSPC)
	}

	//validate attr name
	if len(attr) > MAX_XATTR_NAME_SIZE {
		if runtime.GOOS == "darwin" {
			return fuse.EPERM
		} else {
			return fuse.ERANGE
		}
	}
	if len(attr) == 0 {
		return fuse.EINVAL
	}
	//validate attr value
	if len(data) > MAX_XATTR_VALUE_SIZE {
		if runtime.GOOS == "darwin" {
			return fuse.Status(syscall.E2BIG)
		} else {
			return fuse.ERANGE
		}
	}

	path, fh, entry, status := wfs.maybeReadEntry(input.NodeId)
	if status != fuse.OK {
		return status
	}
	if entry == nil {
		return fuse.ENOENT
	}
	if fh != nil {
		fh.entryLock.Lock()
		defer fh.entryLock.Unlock()
	}

	if entry.Extended == nil {
		entry.Extended = make(map[string][]byte)
	}
	storageKey := xattrStorageKey(attr)
	oldData, _ := entry.Extended[storageKey]
	updated := false
	switch input.Flags {
	case sys.XATTR_CREATE:
		if len(oldData) > 0 {
			break
		}
		fallthrough
	case sys.XATTR_REPLACE:
		fallthrough
	default:
		entry.Extended[storageKey] = data
		updated = true
	}

	if fh != nil {
		fh.dirtyMetadata = true
		if updated {
			wfs.appendTagEventIfNeeded(path, attr, data, false)
		}
		return fuse.OK
	}

	status = wfs.saveEntry(path, entry)
	if status == fuse.OK && updated {
		wfs.appendTagEventIfNeeded(path, attr, data, false)
	}
	return status

}

// ListXAttr lists extended attributes as '\0' delimited byte
// slice, and return the number of bytes. If the buffer is too
// small, return ERANGE, with the required buffer size.
func (wfs *WFS) ListXAttr(cancel <-chan struct{}, header *fuse.InHeader, dest []byte) (n uint32, code fuse.Status) {

	if wfs.option.DisableXAttr {
		return 0, fuse.Status(syscall.ENOTSUP)
	}

	_, _, entry, status := wfs.maybeReadEntry(header.NodeId)
	if status != fuse.OK {
		return 0, status
	}
	if entry == nil {
		return 0, fuse.ENOENT
	}
	if entry.Extended == nil {
		return 0, fuse.OK
	}

	var data []byte
	for k := range entry.Extended {
		if strings.HasPrefix(k, XATTR_PREFIX) {
			data = append(data, k[len(XATTR_PREFIX):]...)
			data = append(data, 0)
		}
	}
	if _, found := entry.Extended[SEAWEED_TAGS_KEY]; found {
		data = append(data, TAG_XATTR_NAME...)
		data = append(data, 0)
	}
	if _, found := entry.Extended[SEAWEED_TAG_VER_KEY]; found {
		data = append(data, TAG_VERSION_XATTR...)
		data = append(data, 0)
	}
	if len(dest) < len(data) {
		return uint32(len(data)), fuse.ERANGE
	}

	copy(dest, data)

	return uint32(len(data)), fuse.OK
}

// RemoveXAttr removes an extended attribute.
func (wfs *WFS) RemoveXAttr(cancel <-chan struct{}, header *fuse.InHeader, attr string) fuse.Status {

	if wfs.option.DisableXAttr {
		return fuse.Status(syscall.ENOTSUP)
	}

	if len(attr) == 0 {
		return fuse.EINVAL
	}
	path, fh, entry, status := wfs.maybeReadEntry(header.NodeId)
	if status != fuse.OK {
		return status
	}
	if entry == nil {
		return fuse.OK
	}
	if fh != nil {
		fh.entryLock.Lock()
		defer fh.entryLock.Unlock()
	}

	if entry.Extended == nil {
		return fuse.ENOATTR
	}
	storageKey := xattrStorageKey(attr)
	_, found := entry.Extended[storageKey]

	if !found {
		return fuse.ENOATTR
	}

	delete(entry.Extended, storageKey)

	status = wfs.saveEntry(path, entry)
	if status == fuse.OK {
		wfs.appendTagEventIfNeeded(path, attr, nil, true)
	}
	return status
}

func xattrStorageKey(attr string) string {
	switch attr {
	case TAG_XATTR_NAME:
		return SEAWEED_TAGS_KEY
	case TAG_VERSION_XATTR:
		return SEAWEED_TAG_VER_KEY
	default:
		return XATTR_PREFIX + attr
	}
}

func (wfs *WFS) appendTagEventIfNeeded(path interface{}, attr string, data []byte, deleted bool) {
	if attr != TAG_XATTR_NAME || wfs.option.TagEventLog == "" {
		return
	}
	op := "set"
	var tags []string
	if deleted {
		op = "delete_all"
	} else {
		tags = splitTagEventValue(string(data))
	}
	event := tagEvent{
		Ts:           time.Now().UnixNano(),
		Source:       "fuse_xattr",
		Op:           op,
		Path:         stringPath(path),
		Tags:         tags,
		IndexUpdated: false,
	}
	if err := appendTagEvent(wfs.option.TagEventLog, event); err != nil {
		// xattr has already been applied. Keep the filesystem operation successful,
		// but surface the tracking failure in logs so freshness can be diagnosed.
		println("failed to append tag event:", err.Error())
	}
}

func appendTagEvent(logPath string, event tagEvent) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	unlock, err := lockTagEventLog(logPath)
	if err != nil {
		return err
	}
	defer unlock()

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(event); err != nil {
		return err
	}
	return file.Sync()
}

func lockTagEventLog(logPath string) (func(), error) {
	lockPath := logPath + ".lock"
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

func splitTagEventValue(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' '
	})
	tags := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			tags = append(tags, field)
		}
	}
	return tags
}

func stringPath(path interface{}) string {
	switch p := path.(type) {
	case string:
		return p
	case []byte:
		return string(p)
	default:
		return fmt.Sprint(p)
	}
}
