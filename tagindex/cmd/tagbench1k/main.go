package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cs598/tagindex/internal/filer"
	"cs598/tagindex/internal/index"
)

type queryCase struct {
	Name string   `json:"name"`
	Expr []string `json:"expr"`
}

type row struct {
	Query          string `json:"query"`
	Expr           string `json:"expr"`
	Repeat         int    `json:"repeat"`
	BaselineCount  int    `json:"baseline_count"`
	IndexCount     int    `json:"index_count"`
	BaselineListMS int64  `json:"baseline_list_ms"`
	BaselineHeadMS int64  `json:"baseline_head_ms"`
	BaselineMS     int64  `json:"baseline_ms"`
	IndexMS        int64  `json:"index_ms"`
	Errors         int64  `json:"errors"`
}

func main() {
	filerURL := flag.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	indexRoot := flag.String("index", defaultIndexRoot(), "tag index root directory")
	under := flag.String("under", "/1k", "filer directory path to benchmark")
	repeat := flag.Int("repeat", 5, "times to run each query")
	concurrency := flag.Int("concurrency", 16, "concurrent baseline HEAD requests")
	format := flag.String("format", "csv", "output format: csv or json")
	flag.Parse()

	if *repeat <= 0 {
		*repeat = 1
	}
	if *concurrency <= 0 {
		*concurrency = 1
	}

	cases := defaultQueries()
	results := make([]row, 0, len(cases)*(*repeat))
	for _, qc := range cases {
		for i := 1; i <= *repeat; i++ {
			r, err := runCase(*filerURL, *indexRoot, *under, *concurrency, qc, i)
			if err != nil {
				fmt.Fprintln(os.Stderr, "tagbench1k:", err)
				os.Exit(1)
			}
			results = append(results, r)
		}
	}

	switch strings.ToLower(*format) {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	default:
		writeCSV(results)
	}
}

func runCase(filerURL, indexRoot, under string, concurrency int, qc queryCase, repeat int) (row, error) {
	client := filer.New(filerURL)
	store := index.New(indexRoot)
	evaluator, err := parseExpr(qc.Expr)
	if err != nil {
		return row{}, err
	}

	baseStart := time.Now()
	listStart := time.Now()
	paths, err := client.ListFilesRecursive(under)
	if err != nil {
		return row{}, err
	}
	listMS := time.Since(listStart).Milliseconds()

	headStart := time.Now()
	baseCount, errors := scanFilerTags(client, paths, evaluator, concurrency)
	headMS := time.Since(headStart).Milliseconds()
	baseMS := time.Since(baseStart).Milliseconds()

	indexStart := time.Now()
	indexPaths, err := store.QueryExpr(qc.Expr, under)
	if err != nil {
		return row{}, err
	}
	indexMS := time.Since(indexStart).Milliseconds()

	return row{
		Query:          qc.Name,
		Expr:           strings.Join(qc.Expr, " "),
		Repeat:         repeat,
		BaselineCount:  baseCount,
		IndexCount:     len(indexPaths),
		BaselineListMS: listMS,
		BaselineHeadMS: headMS,
		BaselineMS:     baseMS,
		IndexMS:        indexMS,
		Errors:         errors,
	}, nil
}

func scanFilerTags(client *filer.Client, paths []string, evaluator evaluator, concurrency int) (int, int64) {
	jobs := make(chan string)
	matches := make(chan struct{}, concurrency)
	var errors int64
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				tags, err := client.GetTags(filer.EscapePath(path))
				if err != nil {
					atomic.AddInt64(&errors, 1)
					continue
				}
				if evaluator.eval(tags) {
					matches <- struct{}{}
				}
			}
		}()
	}

	go func() {
		for _, path := range paths {
			jobs <- path
		}
		close(jobs)
		wg.Wait()
		close(matches)
	}()

	count := 0
	for range matches {
		count++
	}
	return count, errors
}

func writeCSV(rows []row) {
	w := csv.NewWriter(os.Stdout)
	_ = w.Write([]string{
		"query",
		"expr",
		"repeat",
		"baseline_count",
		"index_count",
		"baseline_list_ms",
		"baseline_head_ms",
		"baseline_ms",
		"index_ms",
		"errors",
	})
	for _, r := range rows {
		_ = w.Write([]string{
			r.Query,
			r.Expr,
			fmt.Sprint(r.Repeat),
			fmt.Sprint(r.BaselineCount),
			fmt.Sprint(r.IndexCount),
			fmt.Sprint(r.BaselineListMS),
			fmt.Sprint(r.BaselineHeadMS),
			fmt.Sprint(r.BaselineMS),
			fmt.Sprint(r.IndexMS),
			fmt.Sprint(r.Errors),
		})
	}
	w.Flush()
}

func defaultQueries() []queryCase {
	return []queryCase{
		{Name: "single_common", Expr: []string{"random_tag_1"}},
		{Name: "single_other", Expr: []string{"random_tag_30"}},
		{Name: "and_selective", Expr: []string{"random_tag_1", "AND", "random_tag_2"}},
		{Name: "or_broad", Expr: []string{"random_tag_1", "OR", "random_tag_2"}},
		{Name: "and_not", Expr: []string{"random_tag_1", "AND", "NOT", "random_tag_2"}},
		{Name: "not_broad", Expr: []string{"NOT", "random_tag_1"}},
	}
}

func defaultIndexRoot() string {
	if root := strings.TrimSpace(os.Getenv("TAGCTL_INDEX_ROOT")); root != "" {
		return root
	}
	return "/mnt/i/seaweed-ssd/index"
}

type evaluator interface {
	eval(tags []string) bool
}

type tagNode string

func (n tagNode) eval(tags []string) bool {
	target := string(n)
	for _, tag := range tags {
		if tag == target {
			return true
		}
	}
	return false
}

type notNode struct {
	child evaluator
}

func (n notNode) eval(tags []string) bool {
	return !n.child.eval(tags)
}

type binaryNode struct {
	op          string
	left, right evaluator
}

func (n binaryNode) eval(tags []string) bool {
	switch n.op {
	case "and":
		return n.left.eval(tags) && n.right.eval(tags)
	case "or":
		return n.left.eval(tags) || n.right.eval(tags)
	default:
		return false
	}
}

type parser struct {
	tokens []string
	pos    int
}

func parseExpr(tokens []string) (evaluator, error) {
	p := &parser{tokens: tokens}
	expr, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.hasNext() {
		return nil, fmt.Errorf("unexpected token %q", p.peek())
	}
	return expr, nil
}

func (p *parser) parseOr() (evaluator, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.match("or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = binaryNode{op: "or", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (evaluator, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.match("and") {
		if p.match("not") {
			right, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			left = binaryNode{op: "and", left: left, right: notNode{child: right}}
			continue
		}
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = binaryNode{op: "and", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (evaluator, error) {
	if p.match("not") {
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return notNode{child: child}, nil
	}
	if !p.hasNext() {
		return nil, fmt.Errorf("expected tag")
	}
	tag := p.next()
	if isOperator(tag) {
		return nil, fmt.Errorf("expected tag, got %q", tag)
	}
	return tagNode(tag), nil
}

func (p *parser) match(token string) bool {
	if !p.hasNext() || !strings.EqualFold(p.peek(), token) {
		return false
	}
	p.pos++
	return true
}

func (p *parser) hasNext() bool {
	return p.pos < len(p.tokens)
}

func (p *parser) peek() string {
	return p.tokens[p.pos]
}

func (p *parser) next() string {
	token := p.tokens[p.pos]
	p.pos++
	return token
}

func isOperator(token string) bool {
	return strings.EqualFold(token, "and") ||
		strings.EqualFold(token, "or") ||
		strings.EqualFold(token, "not")
}
