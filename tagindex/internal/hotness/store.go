package hotness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultRoot     = "/mnt/f/seaweed/index/hotness"
	DefaultCapacity = 100000
	DefaultSlotSize = 512

	OpOpen  = "open"
	OpRead  = "read"
	OpWrite = "write"
	OpClose = "close"
)

type Event struct {
	Ts    int64  `json:"ts"`
	Op    string `json:"op"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes,omitempty"`
}

type Counter struct {
	OpenCount    int64 `json:"open_count"`
	ReadCount    int64 `json:"read_count"`
	WriteCount   int64 `json:"write_count"`
	CloseCount   int64 `json:"close_count"`
	BytesRead    int64 `json:"bytes_read"`
	BytesWritten int64 `json:"bytes_written"`
	LastOpenTs   int64 `json:"last_open_ts,omitempty"`
	LastReadTs   int64 `json:"last_read_ts,omitempty"`
	LastWriteTs  int64 `json:"last_write_ts,omitempty"`
	LastCloseTs  int64 `json:"last_close_ts,omitempty"`
}

type ScoreOptions struct {
	OpenWeight       float64
	ReadWeight       float64
	WriteWeight      float64
	BytesReadWeight  float64
	BytesWriteWeight float64
	RecentHalfLife   time.Duration
	NowUnixNano      int64
}

type RankedFile struct {
	Path    string  `json:"path"`
	Score   float64 `json:"score"`
	Counter Counter `json:"counter"`
}

type Meta struct {
	Capacity int64 `json:"capacity"`
	SlotSize int64 `json:"slot_size"`
	Head     int64 `json:"head"`
	Tail     int64 `json:"tail"`
	Size     int64 `json:"size"`
}

type Store struct {
	Root     string
	Capacity int64
	SlotSize int64
}

func New(root string) *Store {
	if strings.TrimSpace(root) == "" {
		root = DefaultRoot
	}
	return &Store{
		Root:     root,
		Capacity: DefaultCapacity,
		SlotSize: DefaultSlotSize,
	}
}

func NewWithOptions(root string, capacity, slotSize int64) *Store {
	store := New(root)
	if capacity > 0 {
		store.Capacity = capacity
	}
	if slotSize > 0 {
		store.SlotSize = slotSize
	}
	return store
}

func (s *Store) Record(event Event) error {
	if err := event.normalize(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return err
	}

	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	meta, err := s.loadMeta()
	if err != nil {
		return err
	}
	counters, err := s.loadCounters()
	if err != nil {
		return err
	}

	ring, err := os.OpenFile(s.ringPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer ring.Close()
	if err := ring.Truncate(meta.Capacity * meta.SlotSize); err != nil {
		return err
	}

	if meta.Size == meta.Capacity {
		old, ok, err := readSlot(ring, meta.Head, meta.SlotSize)
		if err != nil {
			return err
		}
		if ok {
			applyEvent(counters, old, -1)
		}
		meta.Tail = (meta.Tail + 1) % meta.Capacity
	} else {
		meta.Size++
	}

	if err := writeSlot(ring, meta.Head, meta.SlotSize, event); err != nil {
		return err
	}
	applyEvent(counters, event, 1)
	meta.Head = (meta.Head + 1) % meta.Capacity

	if err := s.saveCounters(counters); err != nil {
		return err
	}
	if err := s.saveMeta(meta); err != nil {
		return err
	}
	return nil
}

func (s *Store) LoadCounters() (map[string]Counter, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.loadCounters()
}

func (s *Store) LoadMeta() (Meta, error) {
	unlock, err := s.lock()
	if err != nil {
		return Meta{}, err
	}
	defer unlock()
	return s.loadMeta()
}

func (s *Store) loadMeta() (Meta, error) {
	meta := Meta{
		Capacity: s.Capacity,
		SlotSize: s.SlotSize,
	}
	data, err := os.ReadFile(s.metaPath())
	if errors.Is(err, os.ErrNotExist) {
		return meta, nil
	}
	if err != nil {
		return Meta{}, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, err
	}
	if meta.Capacity <= 0 || meta.SlotSize <= 0 {
		return Meta{}, fmt.Errorf("invalid hotness ring meta: capacity=%d slot_size=%d", meta.Capacity, meta.SlotSize)
	}
	if meta.Head < 0 || meta.Head >= meta.Capacity || meta.Tail < 0 || meta.Tail >= meta.Capacity || meta.Size < 0 || meta.Size > meta.Capacity {
		return Meta{}, fmt.Errorf("invalid hotness ring position: head=%d tail=%d size=%d capacity=%d", meta.Head, meta.Tail, meta.Size, meta.Capacity)
	}
	return meta, nil
}

func (s *Store) loadCounters() (map[string]Counter, error) {
	counters := make(map[string]Counter)
	data, err := os.ReadFile(s.countersPath())
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

func (s *Store) saveMeta(meta Meta) error {
	return writeJSONAtomic(s.metaPath(), meta)
}

func (s *Store) saveCounters(counters map[string]Counter) error {
	compact := make(map[string]Counter)
	for path, counter := range counters {
		if !counter.isZero() {
			compact[path] = counter
		}
	}
	return writeJSONAtomic(s.countersPath(), compact)
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(s.Root, "hotness.lock"), os.O_CREATE|os.O_RDWR, 0o644)
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

func (s *Store) countersPath() string {
	return filepath.Join(s.Root, "raw_counters.json")
}

func (s *Store) ringPath() string {
	return filepath.Join(s.Root, "ring.dat")
}

func (s *Store) metaPath() string {
	return filepath.Join(s.Root, "ring.meta.json")
}

func (event *Event) normalize() error {
	event.Op = strings.ToLower(strings.TrimSpace(event.Op))
	event.Path = strings.TrimSpace(event.Path)
	if event.Ts == 0 {
		event.Ts = time.Now().UnixNano()
	}
	if event.Path == "" {
		return fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(event.Path, "/") {
		event.Path = "/" + event.Path
	}
	switch event.Op {
	case OpOpen, OpRead, OpWrite, OpClose:
	default:
		return fmt.Errorf("unsupported hotness op %q", event.Op)
	}
	if event.Bytes < 0 {
		return fmt.Errorf("bytes must be non-negative")
	}
	if event.Op != OpRead && event.Op != OpWrite {
		event.Bytes = 0
	}
	return nil
}

func readSlot(file *os.File, slot, slotSize int64) (Event, bool, error) {
	buf := make([]byte, slotSize)
	n, err := file.ReadAt(buf, slot*slotSize)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Event{}, false, err
	}
	if n == 0 {
		return Event{}, false, nil
	}
	buf = bytes.TrimRight(buf[:n], "\x00")
	buf = bytes.TrimSpace(buf)
	if len(buf) == 0 {
		return Event{}, false, nil
	}
	var event Event
	if err := json.Unmarshal(buf, &event); err != nil {
		return Event{}, false, err
	}
	if err := event.normalize(); err != nil {
		return Event{}, false, err
	}
	return event, true, nil
}

func writeSlot(file *os.File, slot, slotSize int64, event Event) error {
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

func applyEvent(counters map[string]Counter, event Event, sign int64) {
	counter := counters[event.Path]
	switch event.Op {
	case OpOpen:
		counter.OpenCount += sign
		if sign > 0 && event.Ts > counter.LastOpenTs {
			counter.LastOpenTs = event.Ts
		}
	case OpRead:
		counter.ReadCount += sign
		counter.BytesRead += sign * event.Bytes
		if sign > 0 && event.Ts > counter.LastReadTs {
			counter.LastReadTs = event.Ts
		}
	case OpWrite:
		counter.WriteCount += sign
		counter.BytesWritten += sign * event.Bytes
		if sign > 0 && event.Ts > counter.LastWriteTs {
			counter.LastWriteTs = event.Ts
		}
	case OpClose:
		counter.CloseCount += sign
		if sign > 0 && event.Ts > counter.LastCloseTs {
			counter.LastCloseTs = event.Ts
		}
	}
	clampCounter(&counter)
	counters[event.Path] = counter
}

func clampCounter(counter *Counter) {
	if counter.OpenCount < 0 {
		counter.OpenCount = 0
	}
	if counter.ReadCount < 0 {
		counter.ReadCount = 0
	}
	if counter.WriteCount < 0 {
		counter.WriteCount = 0
	}
	if counter.CloseCount < 0 {
		counter.CloseCount = 0
	}
	if counter.BytesRead < 0 {
		counter.BytesRead = 0
	}
	if counter.BytesWritten < 0 {
		counter.BytesWritten = 0
	}
}

func (counter Counter) isZero() bool {
	return counter.OpenCount == 0 &&
		counter.ReadCount == 0 &&
		counter.WriteCount == 0 &&
		counter.CloseCount == 0 &&
		counter.BytesRead == 0 &&
		counter.BytesWritten == 0
}

func writeJSONAtomic(path string, value any) error {
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

func SortedPaths(counters map[string]Counter) []string {
	paths := make([]string, 0, len(counters))
	for path := range counters {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func DefaultScoreOptions() ScoreOptions {
	return ScoreOptions{
		OpenWeight:       0.25,
		ReadWeight:       1,
		WriteWeight:      2,
		BytesReadWeight:  1.0 / (1024 * 1024),
		BytesWriteWeight: 2.0 / (1024 * 1024),
		RecentHalfLife:   10 * time.Minute,
	}
}

func Rank(counters map[string]Counter, opts ScoreOptions, limit int) []RankedFile {
	opts = opts.withDefaults()
	ranked := make([]RankedFile, 0, len(counters))
	for path, counter := range counters {
		score := Score(counter, opts)
		if score <= 0 {
			continue
		}
		ranked = append(ranked, RankedFile{
			Path:    path,
			Score:   score,
			Counter: counter,
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			return ranked[i].Path < ranked[j].Path
		}
		return ranked[i].Score > ranked[j].Score
	})
	if limit > 0 && len(ranked) > limit {
		return ranked[:limit]
	}
	return ranked
}

func Score(counter Counter, opts ScoreOptions) float64 {
	opts = opts.withDefaults()
	base := opts.OpenWeight*float64(counter.OpenCount) +
		opts.ReadWeight*float64(counter.ReadCount) +
		opts.WriteWeight*float64(counter.WriteCount) +
		opts.BytesReadWeight*float64(counter.BytesRead) +
		opts.BytesWriteWeight*float64(counter.BytesWritten)
	return base * recencyMultiplier(counter, opts)
}

func (opts ScoreOptions) withDefaults() ScoreOptions {
	defaults := DefaultScoreOptions()
	if opts.OpenWeight == 0 {
		opts.OpenWeight = defaults.OpenWeight
	}
	if opts.ReadWeight == 0 {
		opts.ReadWeight = defaults.ReadWeight
	}
	if opts.WriteWeight == 0 {
		opts.WriteWeight = defaults.WriteWeight
	}
	if opts.BytesReadWeight == 0 {
		opts.BytesReadWeight = defaults.BytesReadWeight
	}
	if opts.BytesWriteWeight == 0 {
		opts.BytesWriteWeight = defaults.BytesWriteWeight
	}
	if opts.RecentHalfLife == 0 {
		opts.RecentHalfLife = defaults.RecentHalfLife
	}
	if opts.NowUnixNano == 0 {
		opts.NowUnixNano = time.Now().UnixNano()
	}
	return opts
}

func recencyMultiplier(counter Counter, opts ScoreOptions) float64 {
	last := counter.LastReadTs
	if counter.LastWriteTs > last {
		last = counter.LastWriteTs
	}
	if counter.LastOpenTs > last {
		last = counter.LastOpenTs
	}
	if last <= 0 || opts.RecentHalfLife <= 0 || opts.NowUnixNano <= last {
		return 1
	}
	age := time.Duration(opts.NowUnixNano - last)
	halves := float64(age) / float64(opts.RecentHalfLife)
	return 1 / (1 + halves)
}
