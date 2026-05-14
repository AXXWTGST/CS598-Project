package mount

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordHotnessEventUpdatesCountersAndRing(t *testing.T) {
	root := t.TempDir()
	events := []hotnessEvent{
		{Ts: 1, Op: hotnessOpOpen, Path: "/a.txt"},
		{Ts: 2, Op: hotnessOpRead, Path: "/a.txt", Bytes: 10},
		{Ts: 3, Op: hotnessOpWrite, Path: "/b.txt", Bytes: 7},
	}
	for _, event := range events {
		if err := recordHotnessEvent(root, 3, 256, event); err != nil {
			t.Fatalf("record %+v: %v", event, err)
		}
	}

	var meta hotnessMeta
	readJSON(t, filepath.Join(root, "ring.meta.json"), &meta)
	if meta.Head != 0 || meta.Tail != 0 || meta.Size != 3 {
		t.Fatalf("unexpected meta: %+v", meta)
	}

	var counters map[string]hotnessCounter
	readJSON(t, filepath.Join(root, "raw_counters.json"), &counters)
	if counters["/a.txt"].OpenCount != 1 || counters["/a.txt"].ReadCount != 1 || counters["/a.txt"].BytesRead != 10 {
		t.Fatalf("unexpected /a.txt counter: %+v", counters["/a.txt"])
	}
	if counters["/b.txt"].WriteCount != 1 || counters["/b.txt"].BytesWritten != 7 {
		t.Fatalf("unexpected /b.txt counter: %+v", counters["/b.txt"])
	}
}

func TestRecordHotnessEventOverwritesOldSlot(t *testing.T) {
	root := t.TempDir()
	events := []hotnessEvent{
		{Ts: 1, Op: hotnessOpOpen, Path: "/a.txt"},
		{Ts: 2, Op: hotnessOpRead, Path: "/a.txt", Bytes: 10},
		{Ts: 3, Op: hotnessOpWrite, Path: "/b.txt", Bytes: 7},
	}
	for _, event := range events {
		if err := recordHotnessEvent(root, 2, 256, event); err != nil {
			t.Fatalf("record %+v: %v", event, err)
		}
	}

	var meta hotnessMeta
	readJSON(t, filepath.Join(root, "ring.meta.json"), &meta)
	if meta.Head != 1 || meta.Tail != 1 || meta.Size != 2 {
		t.Fatalf("unexpected meta after overwrite: %+v", meta)
	}

	var counters map[string]hotnessCounter
	readJSON(t, filepath.Join(root, "raw_counters.json"), &counters)
	if counters["/a.txt"].OpenCount != 0 || counters["/a.txt"].ReadCount != 1 || counters["/a.txt"].BytesRead != 10 {
		t.Fatalf("old open event should be subtracted, got /a.txt counter: %+v", counters["/a.txt"])
	}
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}
