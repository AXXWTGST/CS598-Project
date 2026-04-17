package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"cs598/tagindex/internal/filer"
	"cs598/tagindex/internal/index"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "set":
		if err := runSet(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl set:", err)
			os.Exit(1)
		}
	case "add":
		if err := runUpdate("add", os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl add:", err)
			os.Exit(1)
		}
	case "delete", "del", "remove", "rm":
		if err := runUpdate("delete", os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl delete:", err)
			os.Exit(1)
		}
	case "query":
		if err := runQuery(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl query:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func runSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	filerURL := fs.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	indexRoot := fs.String("index", "/mnt/f/seaweed/index", "tag index root directory")
	version := fs.String("version", "1", "tag metadata version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: tagctl set [flags] <path> <tag1,tag2,...>")
	}

	path := fs.Arg(0)
	tags := filer.SplitTags(fs.Arg(1))
	if len(tags) == 0 {
		return fmt.Errorf("at least one tag is required")
	}

	client := filer.New(*filerURL)
	if err := client.SetTags(filer.EscapePath(path), tags, *version); err != nil {
		return err
	}

	store := index.New(*indexRoot)
	if err := store.Set(path, tags); err != nil {
		return err
	}

	fmt.Printf("tagged %s with %s\n", path, strings.Join(tags, ","))
	return nil
}

func runUpdate(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	filerURL := fs.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	indexRoot := fs.String("index", "/mnt/f/seaweed/index", "tag index root directory")
	version := fs.String("version", "1", "tag metadata version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: tagctl %s [flags] <path> <tag1,tag2,...>", command)
	}

	path := fs.Arg(0)
	changedTags := filer.SplitTags(fs.Arg(1))
	if len(changedTags) == 0 {
		return fmt.Errorf("at least one tag is required")
	}

	client := filer.New(*filerURL)
	currentTags, err := client.GetTags(filer.EscapePath(path))
	if err != nil {
		return err
	}

	nextTags := mergeTags(currentTags, changedTags, command == "delete")
	if len(nextTags) == 0 {
		if err := client.ClearTags(filer.EscapePath(path)); err != nil {
			return err
		}
	} else if err := client.SetTags(filer.EscapePath(path), nextTags, *version); err != nil {
		return err
	}

	store := index.New(*indexRoot)
	if err := store.Set(path, nextTags); err != nil {
		return err
	}

	fmt.Printf("tags for %s: %s\n", path, strings.Join(nextTags, ","))
	return nil
}

func runQuery(args []string) error {
	indexRoot, under, expr, err := parseQueryArgs(args)
	if err != nil {
		return err
	}
	if len(expr) == 0 {
		return fmt.Errorf("usage: tagctl query [flags] <expr>")
	}

	store := index.New(indexRoot)
	paths, err := store.QueryExpr(expr, under)
	if err != nil {
		return err
	}
	for _, path := range paths {
		fmt.Println(path)
	}
	return nil
}

func parseQueryArgs(args []string) (indexRoot, under string, expr []string, err error) {
	indexRoot = "/mnt/f/seaweed/index"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-index" || arg == "--index":
			i++
			if i >= len(args) {
				return "", "", nil, fmt.Errorf("%s requires a value", arg)
			}
			indexRoot = args[i]
		case strings.HasPrefix(arg, "-index="):
			indexRoot = strings.TrimPrefix(arg, "-index=")
		case strings.HasPrefix(arg, "--index="):
			indexRoot = strings.TrimPrefix(arg, "--index=")
		case arg == "-under" || arg == "--under":
			i++
			if i >= len(args) {
				return "", "", nil, fmt.Errorf("%s requires a value", arg)
			}
			under = args[i]
		case strings.HasPrefix(arg, "-under="):
			under = strings.TrimPrefix(arg, "-under=")
		case strings.HasPrefix(arg, "--under="):
			under = strings.TrimPrefix(arg, "--under=")
		case strings.HasPrefix(arg, "-"):
			return "", "", nil, fmt.Errorf("unknown flag %s", arg)
		default:
			expr = append(expr, arg)
		}
	}
	return indexRoot, under, expr, nil
}

func mergeTags(currentTags, changedTags []string, deleteMode bool) []string {
	tags := make(map[string]struct{})
	for _, tag := range currentTags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			tags[tag] = struct{}{}
		}
	}
	for _, tag := range changedTags {
		if deleteMode {
			delete(tags, tag)
		} else {
			tags[tag] = struct{}{}
		}
	}

	out := make([]string, 0, len(tags))
	for tag := range tags {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  tagctl set [flags] <path> <tag1,tag2,...>")
	fmt.Fprintln(os.Stderr, "  tagctl add [flags] <path> <tag1,tag2,...>")
	fmt.Fprintln(os.Stderr, "  tagctl delete [flags] <path> <tag1,tag2,...>")
	fmt.Fprintln(os.Stderr, "  tagctl query [flags] <expr>")
}
