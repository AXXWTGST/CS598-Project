package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"cs598/tagindex/internal/filer"
	"cs598/tagindex/internal/hotness"
	"cs598/tagindex/internal/ssd"
)

type server struct {
	cacheRoot  string
	indexRoot  string
	hotStore   *hotness.Store
	cache      *ssd.Cache
	scoreOpts  hotness.ScoreOptions
	defaultTop int
	evictStale bool
}

func main() {
	addr := flag.String("addr", ":9091", "HTTP listen address")
	filerURL := flag.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	ssdRoot := flag.String("ssd", "/mnt/ssd/tagindex", "SSD root for index, hotness data, and cached files")
	indexRoot := flag.String("index", "", "index root directory; default is <ssd>/index")
	hotnessRoot := flag.String("hotness", "", "hotness store directory; default is <ssd>/hotness")
	cacheRoot := flag.String("cache", "", "hot file cache directory; default is <ssd>/cache")
	topN := flag.Int("top", 20, "default number of hot files to return or copy")
	cacheInterval := flag.Duration("cacheInterval", 0, "periodically copy top hot files into SSD cache; 0 disables periodic caching")
	evictStale := flag.Bool("evictStale", true, "mark and delete cached files that are no longer in the current top hot set")
	readWeight := flag.Float64("readWeight", 1, "hotness score weight per read")
	writeWeight := flag.Float64("writeWeight", 2, "hotness score weight per write")
	openWeight := flag.Float64("openWeight", 0.25, "hotness score weight per open")
	mbReadWeight := flag.Float64("mbReadWeight", 1, "hotness score weight per MiB read")
	mbWriteWeight := flag.Float64("mbWriteWeight", 2, "hotness score weight per MiB written")
	halfLife := flag.Duration("halfLife", 10*time.Minute, "recency half-life for hotness scores")
	flag.Parse()

	roots := resolveRoots(*ssdRoot, *indexRoot, *hotnessRoot, *cacheRoot)
	if err := ensureRoots(roots); err != nil {
		log.Fatalf("prepare SSD roots: %v", err)
	}

	scoreOpts := hotness.ScoreOptions{
		OpenWeight:       *openWeight,
		ReadWeight:       *readWeight,
		WriteWeight:      *writeWeight,
		BytesReadWeight:  *mbReadWeight / (1024 * 1024),
		BytesWriteWeight: *mbWriteWeight / (1024 * 1024),
		RecentHalfLife:   *halfLife,
	}
	s := &server{
		cacheRoot:  roots.cache,
		indexRoot:  roots.index,
		hotStore:   hotness.New(roots.hotness),
		cache:      ssd.NewCache(roots.cache, filer.New(*filerURL)),
		scoreOpts:  scoreOpts,
		defaultTop: *topN,
		evictStale: *evictStale,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/hot", s.handleHot)
	mux.HandleFunc("/api/cache", s.handleCache)
	mux.HandleFunc("/api/record", s.handleRecord)

	if *cacheInterval > 0 {
		go s.runPeriodicCache(*cacheInterval)
	}

	log.Printf("ssdserver listening on http://localhost%s", *addr)
	log.Printf("index root: %s", s.indexRoot)
	log.Printf("cache root: %s", s.cacheRoot)
	log.Printf("cache metadata: %s", s.cache.MetadataPath())
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type roots struct {
	index   string
	hotness string
	cache   string
}

func resolveRoots(ssdRoot, indexRoot, hotnessRoot, cacheRoot string) roots {
	if indexRoot == "" {
		indexRoot = filepath.Join(ssdRoot, "index")
	}
	if hotnessRoot == "" {
		hotnessRoot = filepath.Join(ssdRoot, "hotness")
	}
	if cacheRoot == "" {
		cacheRoot = filepath.Join(ssdRoot, "cache")
	}
	return roots{index: indexRoot, hotness: hotnessRoot, cache: cacheRoot}
}

func ensureRoots(roots roots) error {
	for _, root := range []string{roots.index, roots.hotness, roots.cache} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	meta, err := s.hotStore.LoadMeta()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{
		"indexRoot":   s.indexRoot,
		"cacheRoot":   s.cacheRoot,
		"cacheMeta":   s.cache.MetadataPath(),
		"evictStale":  s.evictStale,
		"hotnessRoot": s.hotStore.Root,
		"hotnessMeta": meta,
	})
}

func (s *server) handleHot(w http.ResponseWriter, r *http.Request) {
	ranked, err := s.hotFiles(topParam(r, s.defaultTop))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"files": ranked})
}

func (s *server) handleCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	ranked, err := s.hotFiles(topParam(r, s.defaultTop))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	copied, evicted := s.cache.CopyHotFilesWithEviction(ranked, s.evictStale)
	writeJSON(w, map[string]any{"copied": copied, "evicted": evicted})
}

func (s *server) handleRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	var event hotness.Event
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.hotStore.Record(event); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"recorded": true})
}

func (s *server) hotFiles(limit int) ([]hotness.RankedFile, error) {
	counters, err := s.hotStore.LoadCounters()
	if err != nil {
		return nil, err
	}
	return hotness.Rank(counters, s.scoreOpts, limit), nil
}

func (s *server) runPeriodicCache(interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.cacheOnce()
		<-ticker.C
	}
}

func (s *server) cacheOnce() {
	ranked, err := s.hotFiles(s.defaultTop)
	if err != nil {
		log.Printf("periodic cache rank hot files: %v", err)
		return
	}
	results, evicted := s.cache.CopyHotFilesWithEviction(ranked, s.evictStale)
	ok, failed := 0, 0
	for _, result := range results {
		if result.Error != "" {
			failed++
		} else {
			ok++
		}
	}
	log.Printf("periodic cache top=%d copied=%d failed=%d evicted=%d", s.defaultTop, ok, failed, len(evicted))
}

func topParam(r *http.Request, fallback int) int {
	value := r.URL.Query().Get("top")
	if value == "" {
		return fallback
	}
	top, err := strconv.Atoi(value)
	if err != nil || top < 0 {
		return fallback
	}
	return top
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
