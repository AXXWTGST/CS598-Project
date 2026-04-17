package index

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Store struct {
	Root string
}

func New(root string) *Store {
	return &Store{Root: root}
}

func (s *Store) Set(path string, tags []string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "by-tag"), 0o755); err != nil {
		return err
	}

	if err := s.removePathFromAllTags(path); err != nil {
		return err
	}

	for _, tag := range normalizeTags(tags) {
		if err := s.addPath(tag, path); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Query(tag, under string) ([]string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, fmt.Errorf("tag is required")
	}

	paths, err := s.pathsForTag(tag)
	if err != nil {
		return nil, err
	}

	return filterAndSort(paths, under), nil
}

func (s *Store) QueryExpr(expr []string, under string) ([]string, error) {
	if len(expr) == 0 {
		return nil, fmt.Errorf("query expression is required")
	}

	parser := exprParser{store: s, tokens: expr}
	paths, err := parser.parseOr()
	if err != nil {
		return nil, err
	}
	if parser.hasNext() {
		return nil, fmt.Errorf("unexpected token %q", parser.peek())
	}

	return filterAndSort(paths, under), nil
}

func (s *Store) pathsForTag(tag string) (map[string]struct{}, error) {
	return readLines(filepath.Join(s.Root, "by-tag", tag))
}

func (s *Store) allPaths() (map[string]struct{}, error) {
	paths := make(map[string]struct{})
	byTagDir := filepath.Join(s.Root, "by-tag")
	entries, err := os.ReadDir(byTagDir)
	if os.IsNotExist(err) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		tagPaths, err := readLines(filepath.Join(byTagDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for path := range tagPaths {
			paths[path] = struct{}{}
		}
	}
	return paths, nil
}

func filterAndSort(paths map[string]struct{}, under string) []string {
	under = normalizeUnder(under)
	results := make([]string, 0, len(paths))
	for path := range paths {
		if under == "" || path == under || strings.HasPrefix(path, under+"/") {
			results = append(results, path)
		}
	}
	sort.Strings(results)
	return results
}

type exprParser struct {
	store  *Store
	tokens []string
	pos    int
}

func (p *exprParser) parseOr() (map[string]struct{}, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.match("or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = union(left, right)
	}
	return left, nil
}

func (p *exprParser) parseAnd() (map[string]struct{}, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.match("and") {
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = intersect(left, right)
	}
	return left, nil
}

func (p *exprParser) parseUnary() (map[string]struct{}, error) {
	if p.match("not") {
		paths, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		allPaths, err := p.store.allPaths()
		if err != nil {
			return nil, err
		}
		return difference(allPaths, paths), nil
	}

	if !p.hasNext() {
		return nil, fmt.Errorf("expected tag")
	}
	tag := p.next()
	if isOperator(tag) {
		return nil, fmt.Errorf("expected tag, got %q", tag)
	}
	return p.store.pathsForTag(tag)
}

func (p *exprParser) match(token string) bool {
	if !p.hasNext() || !strings.EqualFold(p.peek(), token) {
		return false
	}
	p.pos++
	return true
}

func (p *exprParser) hasNext() bool {
	return p.pos < len(p.tokens)
}

func (p *exprParser) peek() string {
	return p.tokens[p.pos]
}

func (p *exprParser) next() string {
	token := p.tokens[p.pos]
	p.pos++
	return token
}

func isOperator(token string) bool {
	return strings.EqualFold(token, "and") ||
		strings.EqualFold(token, "or") ||
		strings.EqualFold(token, "not")
}

func union(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for path := range a {
		out[path] = struct{}{}
	}
	for path := range b {
		out[path] = struct{}{}
	}
	return out
}

func intersect(a, b map[string]struct{}) map[string]struct{} {
	if len(a) > len(b) {
		a, b = b, a
	}
	out := make(map[string]struct{})
	for path := range a {
		if _, ok := b[path]; ok {
			out[path] = struct{}{}
		}
	}
	return out
}

func difference(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a))
	for path := range a {
		if _, ok := b[path]; !ok {
			out[path] = struct{}{}
		}
	}
	return out
}

func (s *Store) addPath(tag, path string) error {
	indexPath := filepath.Join(s.Root, "by-tag", tag)
	paths, err := readLines(indexPath)
	if err != nil {
		return err
	}
	paths[path] = struct{}{}
	return writeLines(indexPath, paths)
}

func (s *Store) removePathFromAllTags(path string) error {
	byTagDir := filepath.Join(s.Root, "by-tag")
	entries, err := os.ReadDir(byTagDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		indexPath := filepath.Join(byTagDir, entry.Name())
		paths, err := readLines(indexPath)
		if err != nil {
			return err
		}
		if _, ok := paths[path]; !ok {
			continue
		}
		delete(paths, path)
		if err := writeLines(indexPath, paths); err != nil {
			return err
		}
	}
	return nil
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

func normalizeUnder(under string) string {
	under = strings.TrimSpace(under)
	if under == "" || under == "/" {
		return ""
	}
	if !strings.HasPrefix(under, "/") {
		under = "/" + under
	}
	return strings.TrimRight(under, "/")
}

func readLines(path string) (map[string]struct{}, error) {
	lines := make(map[string]struct{})
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return lines, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines[line] = struct{}{}
		}
	}
	return lines, scanner.Err()
}

func writeLines(path string, lines map[string]struct{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	sorted := make([]string, 0, len(lines))
	for line := range lines {
		sorted = append(sorted, line)
	}
	sort.Strings(sorted)

	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	for _, line := range sorted {
		if _, err := fmt.Fprintln(writer, line); err != nil {
			file.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
