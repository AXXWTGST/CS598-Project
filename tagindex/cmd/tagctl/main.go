package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cs598/tagindex/internal/events"
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
	case "delete":
		if err := runUpdate("delete", os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl delete:", err)
			os.Exit(1)
		}
	case "query":
		if err := runQuery(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl query:", err)
			os.Exit(1)
		}
	case "rebuild-index":
		if err := runRebuildIndex(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl rebuild-index:", err)
			os.Exit(1)
		}
	case "replay-events":
		if err := runReplayEvents(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tagctl replay-events:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

// set tags on a file, replacing any existing tags. with -r, set tags on all files under a directory
func runSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ExitOnError)
	filerURL := fs.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	indexRoot := fs.String("index", defaultIndexRoot(), "tag index root directory")
	eventLog := fs.String("eventLog", defaultEventLog(), "tag event JSONL log path")
	mountRoot := fs.String("mountRoot", defaultMountRoot(), "local SeaweedFS FUSE mount root to translate local paths")
	version := fs.String("version", "1", "tag metadata version")
	recursive := fs.Bool("r", false, "recursively apply tags to all files under path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: tagctl set [flags] <path> <tag1,tag2,...>")
	}

	path := normalizeInputPath(fs.Arg(0), *mountRoot)
	tags := filer.SplitTags(fs.Arg(1))
	if len(tags) == 0 {
		return fmt.Errorf("at least one tag is required")
	}

	client := filer.New(*filerURL)
	store := index.New(*indexRoot)

	paths := []string{path}
	if *recursive {
		listedPaths, err := client.ListFilesRecursive(path)
		if err != nil {
			return err
		}
		paths = listedPaths
	}
	if len(paths) == 0 {
		return fmt.Errorf("no files found under %s", path)
	}

	for _, p := range paths {
		if err := client.SetTags(filer.EscapePath(p), tags, *version); err != nil {
			return err
		}
		if err := store.Set(p, tags); err != nil {
			return err
		}
		if err := events.AppendTagEvent(*eventLog, events.TagEvent{
			Ts:           nowUnixNano(),
			Source:       "tagctl",
			Op:           "set",
			Path:         p,
			Tags:         tags,
			IndexUpdated: true,
		}); err != nil {
			return err
		}
	}

	fmt.Printf("tagged %d file(s) with %s\n", len(paths), strings.Join(tags, ","))
	return nil
}

// add or delete tags on a file while keeping any existing tags. with -r, add or delete tags on all files under a directory
func runUpdate(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	filerURL := fs.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	indexRoot := fs.String("index", defaultIndexRoot(), "tag index root directory")
	eventLog := fs.String("eventLog", defaultEventLog(), "tag event JSONL log path")
	mountRoot := fs.String("mountRoot", defaultMountRoot(), "local SeaweedFS FUSE mount root to translate local paths")
	version := fs.String("version", "1", "tag metadata version")
	recursive := fs.Bool("r", false, "recursively apply tag changes to all files under path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: tagctl %s [flags] <path> <tag1,tag2,...>", command)
	}

	path := normalizeInputPath(fs.Arg(0), *mountRoot)
	changedTags := filer.SplitTags(fs.Arg(1))
	if len(changedTags) == 0 {
		return fmt.Errorf("at least one tag is required")
	}

	client := filer.New(*filerURL)
	store := index.New(*indexRoot)

	paths := []string{path}
	if *recursive {
		listedPaths, err := client.ListFilesRecursive(path)
		if err != nil {
			return err
		}
		paths = listedPaths
	}
	if len(paths) == 0 {
		return fmt.Errorf("no files found under %s", path)
	}

	for _, p := range paths {
		currentTags, err := client.GetTags(filer.EscapePath(p))
		if err != nil {
			return err
		}

		nextTags := mergeTags(currentTags, changedTags, command == "delete")
		if len(nextTags) == 0 {
			if err := client.ClearTags(filer.EscapePath(p)); err != nil {
				return err
			}
		} else if err := client.SetTags(filer.EscapePath(p), nextTags, *version); err != nil {
			return err
		}

		if err := store.Set(p, nextTags); err != nil {
			return err
		}
		op := "set"
		if len(nextTags) == 0 {
			op = "delete_all"
		}
		if err := events.AppendTagEvent(*eventLog, events.TagEvent{
			Ts:           nowUnixNano(),
			Source:       "tagctl",
			Op:           op,
			Path:         p,
			Tags:         nextTags,
			IndexUpdated: true,
		}); err != nil {
			return err
		}
	}

	fmt.Printf("%s tags on %d file(s): %s\n", command, len(paths), strings.Join(changedTags, ","))
	return nil
}

// query the index with a boolean expression of tags, optionally restricting results to a directory subtree with -under
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

// rebuild index entries by reading tags from SeaweedFS metadata, for one file or all files under a directory with -r
func runRebuildIndex(args []string) error {
	fs := flag.NewFlagSet("rebuild-index", flag.ExitOnError)
	filerURL := fs.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	indexRoot := fs.String("index", defaultIndexRoot(), "tag index root directory")
	mountRoot := fs.String("mountRoot", defaultMountRoot(), "local SeaweedFS FUSE mount root to translate local paths")
	recursive := fs.Bool("r", false, "recursively rebuild index for all files under path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: tagctl rebuild-index [-r] [flags] <path>")
	}

	path := normalizeInputPath(fs.Arg(0), *mountRoot)
	client := filer.New(*filerURL)
	store := index.New(*indexRoot)

	paths := []string{path}
	if *recursive {
		listedPaths, err := client.ListFilesRecursive(path)
		if err != nil {
			return err
		}
		paths = listedPaths
	}
	if len(paths) == 0 {
		return fmt.Errorf("no files found under %s", path)
	}

	for _, p := range paths {
		tags, err := client.GetTags(filer.EscapePath(p))
		if err != nil {
			return err
		}
		if err := store.Set(p, tags); err != nil {
			return err
		}
	}

	fmt.Printf("rebuilt index for %d file(s)\n", len(paths))
	return nil
}

func runReplayEvents(args []string) error {
	fs := flag.NewFlagSet("replay-events", flag.ExitOnError)
	indexRoot := fs.String("index", defaultIndexRoot(), "tag index root directory")
	eventLog := fs.String("eventLog", defaultEventLog(), "tag event JSONL log path")
	checkpoint := fs.String("checkpoint", defaultCheckpoint(), "event replay checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tagctl replay-events [flags]")
	}

	result, err := events.ReplayTagEvents(*eventLog, *checkpoint, *indexRoot)
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d event(s), offset %d -> %d\n", result.Applied, result.StartOffset, result.EndOffset)
	return nil
}

// helper to parse command-line arguments for query, which has a different format from set/add/delete/rebuild-index
func parseQueryArgs(args []string) (indexRoot, under string, expr []string, err error) {
	indexRoot = defaultIndexRoot()
	mountRoot := defaultMountRoot()
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
			under = normalizeInputPath(args[i], mountRoot)
		case strings.HasPrefix(arg, "-under="):
			under = normalizeInputPath(strings.TrimPrefix(arg, "-under="), mountRoot)
		case strings.HasPrefix(arg, "--under="):
			under = normalizeInputPath(strings.TrimPrefix(arg, "--under="), mountRoot)
		case arg == "-mountRoot" || arg == "--mountRoot":
			i++
			if i >= len(args) {
				return "", "", nil, fmt.Errorf("%s requires a value", arg)
			}
			mountRoot = args[i]
			if under != "" {
				under = normalizeInputPath(under, mountRoot)
			}
		case strings.HasPrefix(arg, "-mountRoot="):
			mountRoot = strings.TrimPrefix(arg, "-mountRoot=")
			if under != "" {
				under = normalizeInputPath(under, mountRoot)
			}
		case strings.HasPrefix(arg, "--mountRoot="):
			mountRoot = strings.TrimPrefix(arg, "--mountRoot=")
			if under != "" {
				under = normalizeInputPath(under, mountRoot)
			}
		case strings.HasPrefix(arg, "-"):
			return "", "", nil, fmt.Errorf("unknown flag %s", arg)
		default:
			expr = append(expr, arg)
		}
	}
	return indexRoot, under, expr, nil
}

// helper to merge existing tags with added or deleted tags for update operations
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

// timestamp for events, but not guarantee to be unique
func nowUnixNano() int64 {
	return time.Now().UnixNano()
}

func defaultMountRoot() string {
	if root := strings.TrimSpace(os.Getenv("TAGCTL_MOUNT_ROOT")); root != "" {
		return root
	}
	return ""
}

func defaultIndexRoot() string {
	if root := strings.TrimSpace(os.Getenv("TAGCTL_INDEX_ROOT")); root != "" {
		return root
	}
	return ".tagindex"
}

func defaultEventLog() string {
	if path := strings.TrimSpace(os.Getenv("TAGCTL_EVENT_LOG")); path != "" {
		return path
	}
	return filepath.Join(defaultIndexRoot(), "events", "tag_events.log")
}

func defaultCheckpoint() string {
	if path := strings.TrimSpace(os.Getenv("TAGCTL_CHECKPOINT")); path != "" {
		return path
	}
	return filepath.Join(defaultIndexRoot(), "events", "tag_events.offset")
}

// translate a local path from the FUSE mount into a SeaweedFS filer path by stripping the mount root prefix. if the path does not start with the mount root, return it as-is
func normalizeInputPath(path, mountRoot string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	mountRoot = strings.TrimSpace(mountRoot)
	if mountRoot == "" {
		return path
	}

	cleanPath := filepath.Clean(path)
	cleanMount := filepath.Clean(mountRoot)
	if cleanPath == cleanMount {
		return "/"
	}
	prefix := cleanMount + string(os.PathSeparator)
	if strings.HasPrefix(cleanPath, prefix) {
		filerPath := strings.TrimPrefix(cleanPath, cleanMount)
		if !strings.HasPrefix(filerPath, "/") {
			filerPath = "/" + filerPath
		}
		return filerPath
	}
	return path
}

// usage: tagctl replay-events [flags]
func usage() {
	fmt.Fprintln(os.Stderr, "tagctl manages SeaweedFS tags and the external tag index.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  tagctl set [-r] [flags] <path> <tag1,tag2,...>")
	fmt.Fprintln(os.Stderr, "      Replace the full tag list on one file, or on all files under a directory with -r.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  tagctl add [-r] [flags] <path> <tag1,tag2,...>")
	fmt.Fprintln(os.Stderr, "      Add tags while keeping existing tags.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  tagctl delete [-r] [flags] <path> <tag1,tag2,...>")
	fmt.Fprintln(os.Stderr, "      Remove tags while keeping any other tags.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  tagctl query [flags] <expr>")
	fmt.Fprintln(os.Stderr, "      Search the index. Expressions support NOT, AND, OR; NOT has highest precedence.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  tagctl rebuild-index [-r] [flags] <path>")
	fmt.Fprintln(os.Stderr, "      Rebuild index entries by reading tags from SeaweedFS metadata.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  tagctl replay-events [flags]")
	fmt.Fprintln(os.Stderr, "      Replay tag_events.log from the saved checkpoint into the index.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Common flags:")
	fmt.Fprintln(os.Stderr, "  -filer <url>          SeaweedFS filer URL for set/add/delete/rebuild-index")
	fmt.Fprintln(os.Stderr, "                        default: http://localhost:8888")
	fmt.Fprintln(os.Stderr, "  -index <dir>          tag index root")
	fmt.Fprintln(os.Stderr, "                        default: $TAGCTL_INDEX_ROOT or .tagindex")
	fmt.Fprintln(os.Stderr, "  -eventLog <file>      JSONL tag event log for set/add/delete/replay-events")
	fmt.Fprintln(os.Stderr, "                        default: $TAGCTL_EVENT_LOG or <index>/events/tag_events.log")
	fmt.Fprintln(os.Stderr, "  -checkpoint <file>    replay-events checkpoint file")
	fmt.Fprintln(os.Stderr, "                        default: $TAGCTL_CHECKPOINT or <index>/events/tag_events.offset")
	fmt.Fprintln(os.Stderr, "  -mountRoot <dir>      local FUSE mount root used to translate local paths")
	fmt.Fprintln(os.Stderr, "                        default: $TAGCTL_MOUNT_ROOT or empty")
	fmt.Fprintln(os.Stderr, "  -version <value>      tag metadata version for set/add/delete")
	fmt.Fprintln(os.Stderr, "                        default: 1")
	fmt.Fprintln(os.Stderr, "  -r                    recursively apply set/add/delete/rebuild-index to files under path")
	fmt.Fprintln(os.Stderr, "  -under <path>         restrict query results to a directory subtree")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  tagctl set /dataset/hello.txt cs598,email")
	fmt.Fprintln(os.Stderr, "  tagctl add ~/598Project/seaweed-mnt/dataset/hello.txt event_test3")
	fmt.Fprintln(os.Stderr, "  tagctl add -r /dataset/enron important")
	fmt.Fprintln(os.Stderr, "  tagctl delete /dataset/hello.txt email")
	fmt.Fprintln(os.Stderr, "  tagctl query cs598 AND NOT archived")
	fmt.Fprintln(os.Stderr, "  tagctl query -under /dataset/enron email OR important")
	fmt.Fprintln(os.Stderr, "  tagctl rebuild-index -r /dataset")
	fmt.Fprintln(os.Stderr, "  tagctl replay-events")
}
