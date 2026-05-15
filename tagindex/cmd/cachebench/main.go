package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type result struct {
	Root        string `json:"root"`
	Ops         int    `json:"ops"`
	HotFiles    int    `json:"hot_files"`
	TotalFiles  int    `json:"total_files"`
	HotProb     string `json:"hot_prob"`
	ReadProb    string `json:"read_prob"`
	Seed        int64  `json:"seed"`
	WriteBytes  int    `json:"write_bytes"`
	SyncWrites  bool   `json:"sync_writes"`
	Reads       int    `json:"reads"`
	Writes      int    `json:"writes"`
	HotOps      int    `json:"hot_ops"`
	ColdOps     int    `json:"cold_ops"`
	Errors      int    `json:"errors"`
	TotalMillis int64  `json:"total_ms"`
	Read        stats  `json:"read"`
	Write       stats  `json:"write"`
	All         stats  `json:"all"`
}

type stats struct {
	Count    int     `json:"count"`
	AvgMS    float64 `json:"avg_ms"`
	P50MS    float64 `json:"p50_ms"`
	P95MS    float64 `json:"p95_ms"`
	P99MS    float64 `json:"p99_ms"`
	MaxMS    float64 `json:"max_ms"`
	Bytes    int64   `json:"bytes"`
	Errors   int     `json:"errors"`
	BytesAvg float64 `json:"bytes_avg"`
}

type opRecord struct {
	latency time.Duration
	bytes   int64
	err     bool
}

func main() {
	root := flag.String("root", "/home/axx_0213/598Project/seaweed-mnt/1k", "mounted dataset root to benchmark")
	ops := flag.Int("ops", 10000, "number of operations")
	hot := flag.Int("hot", 100, "number of files in the hot set")
	hotProb := flag.Float64("hotProb", 0.8, "probability that an operation targets the hot set")
	readProb := flag.Float64("readProb", 0.8, "probability that an operation is a read")
	writeBytes := flag.Int("writeBytes", 128, "bytes to append for each write")
	seed := flag.Int64("seed", 598, "random seed")
	syncWrites := flag.Bool("syncWrites", false, "call fsync after each write")
	format := flag.String("format", "csv", "output format: csv or json")
	printHot := flag.Bool("printHot", false, "print hot file paths before the summary")
	flag.Parse()

	if *ops <= 0 {
		exitf("-ops must be positive")
	}
	if *hot <= 0 {
		exitf("-hot must be positive")
	}
	if *hotProb < 0 || *hotProb > 1 {
		exitf("-hotProb must be between 0 and 1")
	}
	if *readProb < 0 || *readProb > 1 {
		exitf("-readProb must be between 0 and 1")
	}
	if *writeBytes <= 0 {
		exitf("-writeBytes must be positive")
	}

	files, err := listFiles(*root)
	if err != nil {
		exitf("%v", err)
	}
	if len(files) == 0 {
		exitf("no files found under %s", *root)
	}
	if *hot > len(files) {
		*hot = len(files)
	}

	hotFiles := files[:*hot]
	coldFiles := files[*hot:]
	if len(coldFiles) == 0 {
		coldFiles = hotFiles
	}

	if *printHot {
		for _, path := range hotFiles {
			fmt.Println(path)
		}
	}

	res := run(*root, files, hotFiles, coldFiles, *ops, *hotProb, *readProb, *writeBytes, *seed, *syncWrites)
	switch strings.ToLower(*format) {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	default:
		writeCSV(res)
	}
}

func run(root string, files, hotFiles, coldFiles []string, ops int, hotProb, readProb float64, writeBytes int, seed int64, syncWrites bool) result {
	rng := rand.New(rand.NewSource(seed))
	writePayload := makePayload(writeBytes)
	readRecords := make([]opRecord, 0, ops)
	writeRecords := make([]opRecord, 0, ops)
	allRecords := make([]opRecord, 0, ops)

	reads, writes, hotOps, coldOps := 0, 0, 0, 0
	start := time.Now()
	for i := 0; i < ops; i++ {
		pool := coldFiles
		if rng.Float64() < hotProb {
			pool = hotFiles
			hotOps++
		} else {
			coldOps++
		}
		path := pool[rng.Intn(len(pool))]

		opStart := time.Now()
		var n int64
		var err error
		if rng.Float64() < readProb {
			reads++
			n, err = readFile(path)
			rec := opRecord{latency: time.Since(opStart), bytes: n, err: err != nil}
			readRecords = append(readRecords, rec)
			allRecords = append(allRecords, rec)
		} else {
			writes++
			n, err = appendFile(path, writePayload, syncWrites)
			rec := opRecord{latency: time.Since(opStart), bytes: n, err: err != nil}
			writeRecords = append(writeRecords, rec)
			allRecords = append(allRecords, rec)
		}
	}

	readStats := summarize(readRecords)
	writeStats := summarize(writeRecords)
	allStats := summarize(allRecords)
	return result{
		Root:        root,
		Ops:         ops,
		HotFiles:    len(hotFiles),
		TotalFiles:  len(files),
		HotProb:     fmt.Sprintf("%.3f", hotProb),
		ReadProb:    fmt.Sprintf("%.3f", readProb),
		Seed:        seed,
		WriteBytes:  writeBytes,
		SyncWrites:  syncWrites,
		Reads:       reads,
		Writes:      writes,
		HotOps:      hotOps,
		ColdOps:     coldOps,
		Errors:      readStats.Errors + writeStats.Errors,
		TotalMillis: time.Since(start).Milliseconds(),
		Read:        readStats,
		Write:       writeStats,
		All:         allStats,
	}
}

func listFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func readFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	return int64(len(data)), err
}

func appendFile(path string, payload []byte, syncWrites bool) (int64, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	n, err := file.Write(payload)
	if err != nil {
		return int64(n), err
	}
	if syncWrites {
		if err := file.Sync(); err != nil {
			return int64(n), err
		}
	}
	return int64(n), nil
}

func makePayload(size int) []byte {
	line := fmt.Sprintf("\ncachebench %s ", time.Now().UTC().Format(time.RFC3339Nano))
	payload := make([]byte, 0, size)
	for len(payload) < size {
		payload = append(payload, line...)
	}
	return payload[:size]
}

func summarize(records []opRecord) stats {
	if len(records) == 0 {
		return stats{}
	}
	latencies := make([]float64, 0, len(records))
	var totalLatency float64
	var totalBytes int64
	var errors int
	for _, rec := range records {
		ms := float64(rec.latency.Microseconds()) / 1000
		latencies = append(latencies, ms)
		totalLatency += ms
		totalBytes += rec.bytes
		if rec.err {
			errors++
		}
	}
	sort.Float64s(latencies)
	return stats{
		Count:    len(records),
		AvgMS:    totalLatency / float64(len(records)),
		P50MS:    percentile(latencies, 0.50),
		P95MS:    percentile(latencies, 0.95),
		P99MS:    percentile(latencies, 0.99),
		MaxMS:    latencies[len(latencies)-1],
		Bytes:    totalBytes,
		Errors:   errors,
		BytesAvg: float64(totalBytes) / float64(len(records)),
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p*float64(len(sorted)-1) + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func writeCSV(res result) {
	w := csv.NewWriter(os.Stdout)
	_ = w.Write([]string{
		"root",
		"ops",
		"hot_files",
		"total_files",
		"hot_prob",
		"read_prob",
		"seed",
		"write_bytes",
		"sync_writes",
		"reads",
		"writes",
		"hot_ops",
		"cold_ops",
		"errors",
		"total_ms",
		"read_avg_ms",
		"read_p50_ms",
		"read_p95_ms",
		"read_p99_ms",
		"write_avg_ms",
		"write_p50_ms",
		"write_p95_ms",
		"write_p99_ms",
		"all_avg_ms",
		"all_p50_ms",
		"all_p95_ms",
		"all_p99_ms",
	})
	_ = w.Write([]string{
		res.Root,
		fmt.Sprint(res.Ops),
		fmt.Sprint(res.HotFiles),
		fmt.Sprint(res.TotalFiles),
		res.HotProb,
		res.ReadProb,
		fmt.Sprint(res.Seed),
		fmt.Sprint(res.WriteBytes),
		fmt.Sprint(res.SyncWrites),
		fmt.Sprint(res.Reads),
		fmt.Sprint(res.Writes),
		fmt.Sprint(res.HotOps),
		fmt.Sprint(res.ColdOps),
		fmt.Sprint(res.Errors),
		fmt.Sprint(res.TotalMillis),
		fmt.Sprintf("%.3f", res.Read.AvgMS),
		fmt.Sprintf("%.3f", res.Read.P50MS),
		fmt.Sprintf("%.3f", res.Read.P95MS),
		fmt.Sprintf("%.3f", res.Read.P99MS),
		fmt.Sprintf("%.3f", res.Write.AvgMS),
		fmt.Sprintf("%.3f", res.Write.P50MS),
		fmt.Sprintf("%.3f", res.Write.P95MS),
		fmt.Sprintf("%.3f", res.Write.P99MS),
		fmt.Sprintf("%.3f", res.All.AvgMS),
		fmt.Sprintf("%.3f", res.All.P50MS),
		fmt.Sprintf("%.3f", res.All.P95MS),
		fmt.Sprintf("%.3f", res.All.P99MS),
	})
	w.Flush()
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cachebench: "+format+"\n", args...)
	os.Exit(1)
}
