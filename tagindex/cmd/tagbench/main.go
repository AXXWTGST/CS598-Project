package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cs598/tagindex/internal/filer"
)

type result struct {
	FilerURL     string   `json:"filer"`
	Under        string   `json:"under"`
	Expr         []string `json:"expr"`
	FilesScanned int      `json:"files_scanned"`
	Matches      int      `json:"matches"`
	ListMillis   int64    `json:"list_ms"`
	HeadMillis   int64    `json:"head_ms"`
	TotalMillis  int64    `json:"total_ms"`
	Concurrency  int      `json:"concurrency"`
	Errors       int64    `json:"errors"`
	MatchedPaths []string `json:"matched_paths,omitempty"`
}

func main() {
	filerURL := flag.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	under := flag.String("under", "/", "filer directory path to recursively scan")
	concurrency := flag.Int("concurrency", 16, "concurrent HEAD requests")
	jsonOut := flag.Bool("json", false, "print JSON result")
	showPaths := flag.Bool("paths", false, "print matched paths")
	flag.Parse()

	expr := flag.Args()
	if len(expr) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tagbench [flags] <tag-expr>")
		os.Exit(2)
	}
	if *concurrency <= 0 {
		*concurrency = 1
	}

	res, err := run(*filerURL, *under, expr, *concurrency, *showPaths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tagbench:", err)
		os.Exit(1)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}
	fmt.Printf("filer=%s under=%s expr=%q scanned=%d matches=%d list_ms=%d head_ms=%d total_ms=%d concurrency=%d errors=%d\n",
		res.FilerURL, res.Under, strings.Join(res.Expr, " "), res.FilesScanned, res.Matches, res.ListMillis, res.HeadMillis, res.TotalMillis, res.Concurrency, res.Errors)
	if *showPaths {
		for _, path := range res.MatchedPaths {
			fmt.Println(path)
		}
	}
}

func run(filerURL, under string, expr []string, concurrency int, showPaths bool) (*result, error) {
	totalStart := time.Now()
	client := filer.New(filerURL)

	listStart := time.Now()
	paths, err := client.ListFilesRecursive(under)
	if err != nil {
		return nil, err
	}
	listMillis := time.Since(listStart).Milliseconds()

	evaluator, err := parseExpr(expr)
	if err != nil {
		return nil, err
	}

	headStart := time.Now()
	jobs := make(chan string)
	matches := make(chan string, concurrency)
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
					matches <- path
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

	var matched []string
	matchCount := 0
	for path := range matches {
		matchCount++
		if showPaths {
			matched = append(matched, path)
		}
	}
	sort.Strings(matched)

	return &result{
		FilerURL:     filerURL,
		Under:        under,
		Expr:         expr,
		FilesScanned: len(paths),
		Matches:      matchCount,
		ListMillis:   listMillis,
		HeadMillis:   time.Since(headStart).Milliseconds(),
		TotalMillis:  time.Since(totalStart).Milliseconds(),
		Concurrency:  concurrency,
		Errors:       errors,
		MatchedPaths: matched,
	}, nil
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
