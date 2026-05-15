package mount

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

const (
	hotnessOpOpen  = "open"
	hotnessOpRead  = "read"
	hotnessOpWrite = "write"

	defaultHotnessCapacity = int64(100000)
	defaultHotnessSlotSize = int64(512)
)

type hotnessEvent struct {
	Ts    int64  `json:"ts"`
	Op    string `json:"op"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes,omitempty"`
}

type hotnessCounter struct {
	OpenCount    int64 `json:"open_count"`
	ReadCount    int64 `json:"read_count"`
	WriteCount   int64 `json:"write_count"`
	BytesRead    int64 `json:"bytes_read"`
	BytesWritten int64 `json:"bytes_written"`
	LastOpenTs   int64 `json:"last_open_ts,omitempty"`
	LastReadTs   int64 `json:"last_read_ts,omitempty"`
	LastWriteTs  int64 `json:"last_write_ts,omitempty"`
}

type hotnessMeta struct {
	Capacity int64 `json:"capacity"`
	SlotSize int64 `json:"slot_size"`
	Head     int64 `json:"head"`
	Tail     int64 `json:"tail"`
	Size     int64 `json:"size"`
}

func (wfs *WFS) recordHotnessOpen(path util.FullPath) {
	wfs.recordHotnessEvent(hotnessEvent{
		Ts:   time.Now().UnixNano(),
		Op:   hotnessOpOpen,
		Path: string(path),
	})
}

func (wfs *WFS) recordHotnessRead(path util.FullPath, bytesRead int64) {
	if bytesRead <= 0 {
		return
	}
	wfs.recordHotnessEvent(hotnessEvent{
		Ts:    time.Now().UnixNano(),
		Op:    hotnessOpRead,
		Path:  string(path),
		Bytes: bytesRead,
	})
}

func (wfs *WFS) recordHotnessWrite(path util.FullPath, bytesWritten int64) {
	if bytesWritten <= 0 {
		return
	}
	wfs.recordHotnessEvent(hotnessEvent{
		Ts:    time.Now().UnixNano(),
		Op:    hotnessOpWrite,
		Path:  string(path),
		Bytes: bytesWritten,
	})
}

func (wfs *WFS) recordHotnessEvent(event hotnessEvent) {
	if wfs.option.HotnessDir == "" {
		return
	}
	if err := recordHotnessEvent(wfs.option.HotnessDir, wfs.option.HotnessCapacity, wfs.option.HotnessSlotSize, event); err != nil {
		glog.Warningf("record hotness event %s %s: %v", event.Op, event.Path, err)
	}
}

func recordHotnessEvent(root string, capacity, slotSize int64, event hotnessEvent) error {
	event.Op = strings.ToLower(strings.TrimSpace(event.Op))
	event.Path = strings.TrimSpace(event.Path)
	if event.Path == "" {
		return fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(event.Path, "/") {
		event.Path = "/" + event.Path
	}
	if event.Ts == 0 {
		event.Ts = time.Now().UnixNano()
	}
	switch event.Op {
	case hotnessOpOpen:
		event.Bytes = 0
	case hotnessOpRead, hotnessOpWrite:
		if event.Bytes <= 0 {
			return nil
		}
	default:
		return fmt.Errorf("unsupported hotness op %q", event.Op)
	}
	if capacity <= 0 {
		capacity = defaultHotnessCapacity
	}
	if slotSize <= 0 {
		slotSize = defaultHotnessSlotSize
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	unlock, err := lockHotness(root)
	if err != nil {
		return err
	}
	defer unlock()

	meta, err := loadHotnessMeta(root, capacity, slotSize)
	if err != nil {
		return err
	}
	counters, err := loadHotnessCounters(root)
	if err != nil {
		return err
	}

	ring, err := os.OpenFile(hotnessRingPath(root), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer ring.Close()
	if err := ring.Truncate(meta.Capacity * meta.SlotSize); err != nil {
		return err
	}

	if meta.Size == meta.Capacity {
		old, ok, err := readHotnessSlot(ring, meta.Head, meta.SlotSize)
		if err != nil {
			return err
		}
		if ok {
			applyHotnessEvent(counters, old, -1)
		}
		meta.Tail = (meta.Tail + 1) % meta.Capacity
	} else {
		meta.Size++
	}

	if err := writeHotnessSlot(ring, meta.Head, meta.SlotSize, event); err != nil {
		return err
	}
	applyHotnessEvent(counters, event, 1)
	meta.Head = (meta.Head + 1) % meta.Capacity

	if err := saveHotnessCounters(root, counters); err != nil {
		return err
	}
	return saveHotnessMeta(root, meta)
}

func loadHotnessMeta(root string, capacity, slotSize int64) (hotnessMeta, error) {
	meta := hotnessMeta{Capacity: capacity, SlotSize: slotSize}
	data, err := os.ReadFile(hotnessMetaPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return meta, nil
	}
	if err != nil {
		return hotnessMeta{}, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return hotnessMeta{}, err
	}
	if meta.Capacity <= 0 || meta.SlotSize <= 0 {
		return hotnessMeta{}, fmt.Errorf("invalid hotness meta: capacity=%d slot_size=%d", meta.Capacity, meta.SlotSize)
	}
	if meta.Head < 0 || meta.Head >= meta.Capacity || meta.Tail < 0 || meta.Tail >= meta.Capacity || meta.Size < 0 || meta.Size > meta.Capacity {
		return hotnessMeta{}, fmt.Errorf("invalid hotness meta position: head=%d tail=%d size=%d capacity=%d", meta.Head, meta.Tail, meta.Size, meta.Capacity)
	}
	return meta, nil
}

func loadHotnessCounters(root string) (map[string]hotnessCounter, error) {
	counters := make(map[string]hotnessCounter)
	data, err := os.ReadFile(hotnessCountersPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return counters, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return counters, nil
	}
	if err := json.Unmarshal(data, &counters); err != nil {
		return nil, err
	}
	return counters, nil
}

func saveHotnessMeta(root string, meta hotnessMeta) error {
	return writeHotnessJSONAtomic(hotnessMetaPath(root), meta)
}

func saveHotnessCounters(root string, counters map[string]hotnessCounter) error {
	compact := make(map[string]hotnessCounter)
	for path, counter := range counters {
		if !counter.isZero() {
			compact[path] = counter
		}
	}
	return writeHotnessJSONAtomic(hotnessCountersPath(root), compact)
}

func readHotnessSlot(file *os.File, slot, slotSize int64) (hotnessEvent, bool, error) {
	buf := make([]byte, slotSize)
	n, err := file.ReadAt(buf, slot*slotSize)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return hotnessEvent{}, false, err
	}
	if n == 0 {
		return hotnessEvent{}, false, nil
	}
	buf = bytes.TrimRight(buf[:n], "\x00")
	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		return hotnessEvent{}, false, nil
	}
	var event hotnessEvent
	if err := json.Unmarshal(buf, &event); err != nil {
		return hotnessEvent{}, false, err
	}
	return event, true, nil
}

func writeHotnessSlot(file *os.File, slot, slotSize int64, event hotnessEvent) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if int64(len(encoded)+1) > slotSize {
		return fmt.Errorf("hotness event too large for slot: encoded=%d slot=%d path=%s", len(encoded)+1, slotSize, event.Path)
	}
	buf := make([]byte, slotSize)
	copy(buf, encoded)
	buf[len(encoded)] = '\n'
	_, err = file.WriteAt(buf, slot*slotSize)
	return err
}

func applyHotnessEvent(counters map[string]hotnessCounter, event hotnessEvent, sign int64) {
	counter := counters[event.Path]
	switch event.Op {
	case hotnessOpOpen:
		counter.OpenCount += sign
		if sign > 0 && event.Ts > counter.LastOpenTs {
			counter.LastOpenTs = event.Ts
		}
	case hotnessOpRead:
		counter.ReadCount += sign
		counter.BytesRead += sign * event.Bytes
		if sign > 0 && event.Ts > counter.LastReadTs {
			counter.LastReadTs = event.Ts
		}
	case hotnessOpWrite:
		counter.WriteCount += sign
		counter.BytesWritten += sign * event.Bytes
		if sign > 0 && event.Ts > counter.LastWriteTs {
			counter.LastWriteTs = event.Ts
		}
	}
	clampHotnessCounter(&counter)
	counters[event.Path] = counter
}

func clampHotnessCounter(counter *hotnessCounter) {
	if counter.OpenCount < 0 {
		counter.OpenCount = 0
	}
	if counter.ReadCount < 0 {
		counter.ReadCount = 0
	}
	if counter.WriteCount < 0 {
		counter.WriteCount = 0
	}
	if counter.BytesRead < 0 {
		counter.BytesRead = 0
	}
	if counter.BytesWritten < 0 {
		counter.BytesWritten = 0
	}
}

func (counter hotnessCounter) isZero() bool {
	return counter.OpenCount == 0 &&
		counter.ReadCount == 0 &&
		counter.WriteCount == 0 &&
		counter.BytesRead == 0 &&
		counter.BytesWritten == 0
}

func writeHotnessJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
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

func lockHotness(root string) (func(), error) {
	lockPath := filepath.Join(root, "hotness.lock")
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

func hotnessCountersPath(root string) string {
	return filepath.Join(root, "raw_counters.json")
}

func hotnessRingPath(root string) string {
	return filepath.Join(root, "ring.dat")
}

func hotnessMetaPath(root string) string {
	return filepath.Join(root, "ring.meta.json")
}
