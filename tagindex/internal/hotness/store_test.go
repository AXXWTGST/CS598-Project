package hotness

import (
	"testing"
	"time"
)

func TestRecordUpdatesCountersAndRingMeta(t *testing.T) {
	store := NewWithOptions(t.TempDir(), 3, 256)

	events := []Event{
		{Ts: 1, Op: OpOpen, Path: "/a.txt"},
		{Ts: 2, Op: OpRead, Path: "/a.txt", Bytes: 10},
		{Ts: 3, Op: OpWrite, Path: "/b.txt", Bytes: 7},
	}
	for _, event := range events {
		if err := store.Record(event); err != nil {
			t.Fatalf("record %+v: %v", event, err)
		}
	}

	meta, err := store.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if meta.Head != 0 || meta.Tail != 0 || meta.Size != 3 {
		t.Fatalf("unexpected meta: %+v", meta)
	}

	counters, err := store.LoadCounters()
	if err != nil {
		t.Fatal(err)
	}
	a := counters["/a.txt"]
	if a.OpenCount != 1 || a.ReadCount != 1 || a.BytesRead != 10 || a.LastOpenTs != 1 || a.LastReadTs != 2 {
		t.Fatalf("unexpected /a.txt counter: %+v", a)
	}
	b := counters["/b.txt"]
	if b.WriteCount != 1 || b.BytesWritten != 7 || b.LastWriteTs != 3 {
		t.Fatalf("unexpected /b.txt counter: %+v", b)
	}
}

func TestRecordOverwritesOldSlotAndSubtractsCounter(t *testing.T) {
	store := NewWithOptions(t.TempDir(), 2, 256)

	events := []Event{
		{Ts: 1, Op: OpOpen, Path: "/a.txt"},
		{Ts: 2, Op: OpRead, Path: "/a.txt", Bytes: 10},
		{Ts: 3, Op: OpWrite, Path: "/b.txt", Bytes: 7},
	}
	for _, event := range events {
		if err := store.Record(event); err != nil {
			t.Fatalf("record %+v: %v", event, err)
		}
	}

	meta, err := store.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if meta.Head != 1 || meta.Tail != 1 || meta.Size != 2 {
		t.Fatalf("unexpected meta after overwrite: %+v", meta)
	}

	counters, err := store.LoadCounters()
	if err != nil {
		t.Fatal(err)
	}
	a := counters["/a.txt"]
	if a.OpenCount != 0 || a.ReadCount != 1 || a.BytesRead != 10 {
		t.Fatalf("old open event should have been subtracted, got /a.txt counter: %+v", a)
	}
	b := counters["/b.txt"]
	if b.WriteCount != 1 || b.BytesWritten != 7 {
		t.Fatalf("unexpected /b.txt counter: %+v", b)
	}
}

func TestRecordNormalizesRelativePathAndRejectsBadOp(t *testing.T) {
	store := NewWithOptions(t.TempDir(), 2, 256)

	if err := store.Record(Event{Ts: 1, Op: OpRead, Path: "relative.txt", Bytes: 5}); err != nil {
		t.Fatal(err)
	}
	counters, err := store.LoadCounters()
	if err != nil {
		t.Fatal(err)
	}
	if counters["/relative.txt"].BytesRead != 5 {
		t.Fatalf("relative path was not normalized: %+v", counters)
	}

	if err := store.Record(Event{Ts: 2, Op: "stat", Path: "/relative.txt"}); err == nil {
		t.Fatal("expected unsupported op error")
	}
}

func TestRankOrdersByWeightedHotness(t *testing.T) {
	now := int64(100 * time.Second)
	counters := map[string]Counter{
		"/read.txt": {
			ReadCount:  4,
			BytesRead:  2 * 1024 * 1024,
			LastReadTs: now,
		},
		"/write.txt": {
			WriteCount:    3,
			BytesWritten:  3 * 1024 * 1024,
			LastWriteTs:   now,
			LastCloseTs:   now,
			LastOpenTs:    now,
			LastReadTs:    now,
			OpenCount:     1,
			CloseCount:    1,
		},
		"/cold.txt": {
			ReadCount:  10,
			LastReadTs: now - int64(30*time.Second),
		},
	}

	ranked := Rank(counters, ScoreOptions{
		ReadWeight:       1,
		WriteWeight:      2,
		BytesReadWeight:  1.0 / (1024 * 1024),
		BytesWriteWeight: 2.0 / (1024 * 1024),
		RecentHalfLife:   10 * time.Second,
		NowUnixNano:      now,
	}, 2)

	if len(ranked) != 2 {
		t.Fatalf("expected two ranked files, got %d", len(ranked))
	}
	if ranked[0].Path != "/write.txt" || ranked[1].Path != "/read.txt" {
		t.Fatalf("unexpected rank order: %+v", ranked)
	}
}
