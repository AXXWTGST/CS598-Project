package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cs598/tagindex/internal/events"
	"cs598/tagindex/internal/filer"
	"cs598/tagindex/internal/index"
)

type server struct {
	filer         *filer.Client
	indexRoot     string
	version       string
	eventLog      string
	checkpoint    string
	mu            sync.Mutex
	lastReplay    events.ReplayResult
	lastReplayErr string
	lastReplayAt  time.Time
}

type tagRequest struct {
	Op        string   `json:"op"`
	Path      string   `json:"path"`
	Tags      []string `json:"tags"`
	Recursive bool     `json:"recursive"`
}

type listEntry struct {
	FullPath    string `json:"FullPath"`
	IsDirectory bool   `json:"IsDirectory"`
	FileSize    uint64 `json:"FileSize"`
	Mime        string `json:"Mime"`
}

func main() {
	addr := flag.String("addr", ":9090", "HTTP listen address")
	filerURL := flag.String("filer", "http://localhost:8888", "SeaweedFS filer URL")
	indexRoot := flag.String("index", defaultIndexRoot(), "tag index root directory")
	version := flag.String("version", "1", "tag metadata version")
	eventLog := flag.String("eventLog", "", "optional tag event JSONL log to watch and replay into the index")
	checkpoint := flag.String("checkpoint", defaultCheckpoint(), "tag event replay checkpoint path")
	eventPoll := flag.Duration("eventPoll", time.Second, "tag event replay polling interval")
	flag.Parse()

	s := &server{
		filer:      filer.New(*filerURL),
		indexRoot:  *indexRoot,
		version:    *version,
		eventLog:   *eventLog,
		checkpoint: *checkpoint,
	}
	if s.eventLog != "" {
		go s.watchEvents(*eventPoll)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/list", s.handleList)
	mux.HandleFunc("/api/tags", s.handleTags)
	mux.HandleFunc("/api/query", s.handleQuery)
	mux.HandleFunc("/api/status", s.handleStatus)

	log.Printf("tagUI listening on http://localhost%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *server) watchEvents(interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.replayEventsOnce()
		<-ticker.C
	}
}

func (s *server) replayEventsOnce() {
	result, err := events.ReplayTagEvents(s.eventLog, s.checkpoint, s.indexRoot)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastReplayAt = time.Now()
	if result != nil {
		s.lastReplay = *result
	}
	if err != nil {
		s.lastReplayErr = err.Error()
		log.Printf("replay tag events: %v", err)
		return
	}
	s.lastReplayErr = ""
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	page, err := s.filer.ListDirectory(path, r.URL.Query().Get("lastFileName"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	entries := make([]listEntry, 0, len(page.Entries))
	for _, entry := range page.Entries {
		entries = append(entries, listEntry{
			FullPath:    entry.FullPath,
			IsDirectory: entry.IsDirectory(),
			FileSize:    entry.FileSize,
			Mime:        entry.Mime,
		})
	}
	writeJSON(w, map[string]any{
		"Entries":               entries,
		"LastFileName":          page.LastFileName,
		"ShouldDisplayLoadMore": page.ShouldDisplayLoadMore,
	})
}

func (s *server) handleTags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		path := r.URL.Query().Get("path")
		if path == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("path is required"))
			return
		}
		tags, err := s.filer.GetTags(filer.EscapePath(path))
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, map[string]any{"path": path, "tags": tags})
	case http.MethodPost:
		var req tagRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		count, err := s.applyTags(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"updated": count})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
	}
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	expr := strings.Fields(r.URL.Query().Get("expr"))
	if len(expr) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("expr is required"))
		return
	}
	paths, err := index.New(s.indexRoot).QueryExpr(expr, r.URL.Query().Get("under"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"paths": paths})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{
		"eventLog":     s.eventLog,
		"checkpoint":   s.checkpoint,
		"lastReplay":   s.lastReplay,
		"lastReplayAt": s.lastReplayAt,
		"lastError":    s.lastReplayErr,
	})
}

func (s *server) applyTags(req tagRequest) (int, error) {
	req.Op = strings.ToLower(strings.TrimSpace(req.Op))
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		return 0, fmt.Errorf("path is required")
	}
	if len(req.Tags) == 0 {
		return 0, fmt.Errorf("at least one tag is required")
	}

	paths := []string{req.Path}
	if req.Recursive {
		listed, err := s.filer.ListFilesRecursive(req.Path)
		if err != nil {
			return 0, err
		}
		paths = listed
	}
	if len(paths) == 0 {
		return 0, fmt.Errorf("no files found under %s", req.Path)
	}

	store := index.New(s.indexRoot)
	for _, path := range paths {
		nextTags := normalizeTags(req.Tags)
		if req.Op == "add" || req.Op == "delete" || req.Op == "remove" {
			current, err := s.filer.GetTags(filer.EscapePath(path))
			if err != nil {
				return 0, err
			}
			nextTags = mergeTags(current, req.Tags, req.Op != "add")
		}

		switch req.Op {
		case "set", "add":
			if len(nextTags) == 0 {
				return 0, fmt.Errorf("at least one tag is required")
			}
			if err := s.filer.SetTags(filer.EscapePath(path), nextTags, s.version); err != nil {
				return 0, err
			}
		case "delete", "remove":
			if len(nextTags) == 0 {
				if err := s.filer.ClearTags(filer.EscapePath(path)); err != nil {
					return 0, err
				}
			} else if err := s.filer.SetTags(filer.EscapePath(path), nextTags, s.version); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("unsupported op %q", req.Op)
		}

		if err := store.Set(path, nextTags); err != nil {
			return 0, err
		}
		op := "set"
		if len(nextTags) == 0 {
			op = "delete_all"
		}
		if err := events.AppendTagEvent(s.eventLog, events.TagEvent{
			Ts:           time.Now().UnixNano(),
			Source:       "tagUI",
			Op:           op,
			Path:         path,
			Tags:         nextTags,
			IndexUpdated: true,
		}); err != nil {
			return 0, err
		}
	}
	return len(paths), nil
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
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
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

func normalizeTags(tags []string) []string {
	return mergeTags(nil, tags, false)
}

func defaultIndexRoot() string {
	if root := strings.TrimSpace(os.Getenv("TAGCTL_INDEX_ROOT")); root != "" {
		return root
	}
	return "/mnt/f/seaweed/index"
}

func defaultCheckpoint() string {
	if path := strings.TrimSpace(os.Getenv("TAGCTL_CHECKPOINT")); path != "" {
		return path
	}
	return filepath.Join(defaultIndexRoot(), "events", "tag_events.offset")
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

const indexHTML = `<!DOCTYPE html>
<html>
<head>
  <title>TagIndex Filer</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <style>
    body { font-family: Arial, sans-serif; padding: 20px 20px 90px; color: #222; }
    .toolbar { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; margin-bottom: 12px; }
    .input-group { display: flex; }
    .input-group span { border: 1px solid #ccc; border-right: 0; padding: 7px 10px; background: #eee; }
    input { flex: 1; border: 1px solid #ccc; padding: 7px 10px; font-size: 14px; }
    button { border: 1px solid #aaa; background: #eee; padding: 7px 10px; cursor: pointer; border-radius: 3px; }
    button:hover { background: #ddd; }
    table { border-collapse: collapse; width: 100%; }
    th, td { border-top: 1px solid #ddd; padding: 7px; text-align: left; }
    tbody tr:hover { background: #f5f5f5; }
    .path-link { cursor: pointer; color: #2166a5; }
    .tag-cell { color: #555; font-family: monospace; }
    .context-menu {
      position: fixed; display: none; z-index: 1000; min-width: 190px;
      background: #fff; border: 1px solid #bbb; box-shadow: 0 2px 6px rgba(0,0,0,.2);
    }
    .context-menu button {
      display: block; width: 100%; border: 0; background: #fff; padding: 7px 10px; text-align: left;
    }
    .context-menu button:hover { background: #eee; }
    #status { position: fixed; bottom: 0; left: 0; right: 0; padding: 10px 20px; background: #f7f7f7; border-top: 1px solid #ddd; }
  </style>
</head>
<body>
  <div class="toolbar">
    <div>
      <div class="input-group">
        <span>Path</span>
        <input id="path" value="/">
        <button onclick="loadPath()">Open</button>
      </div>
    </div>
    <div>
      <div class="input-group">
        <span>Query</span>
        <input id="query" placeholder="tagA AND NOT tagB">
        <button onclick="runQuery()">Search</button>
      </div>
    </div>
  </div>
  <table>
    <thead><tr><th>Name</th><th>Size</th><th>Mime</th><th>Tags</th></tr></thead>
    <tbody id="entries"></tbody>
  </table>
  <div id="menu" class="context-menu">
    <button onclick="tagOp('set')">Set tags</button>
    <button onclick="tagOp('add')">Add tags</button>
    <button onclick="tagOp('delete')">Delete tags</button>
  </div>
  <div id="status">Ready.</div>
<script>
let currentPath = "/";
let selected = null;

document.body.addEventListener("click", () => document.getElementById("menu").style.display = "none");

async function loadPath(path) {
  currentPath = path || document.getElementById("path").value || "/";
  document.getElementById("path").value = currentPath;
  const res = await fetch("/api/list?path=" + encodeURIComponent(currentPath));
  const data = await res.json();
  if (data.error) return setStatus(data.error);
  const tbody = document.getElementById("entries");
  tbody.innerHTML = "";
  if (currentPath !== "/") addParentRow(parentPath(currentPath));
  for (const e of data.Entries || []) addRow(e, baseName(e.FullPath), e.IsDirectory);
  setStatus("Loaded " + currentPath);
}

function addParentRow(path) {
  const tr = document.createElement("tr");
  tr.innerHTML = "<td></td><td></td><td></td><td class='tag-cell'></td>";
  const a = document.createElement("span");
  a.className = "path-link";
  a.textContent = "📁 ..";
  a.onclick = () => loadPath(path);
  tr.children[0].appendChild(a);
  tr.children[2].textContent = "parent directory";
  document.getElementById("entries").appendChild(tr);
}

function addRow(e, name, dir) {
  const tr = document.createElement("tr");
  tr.oncontextmenu = ev => showMenu(ev, e, dir);
  tr.innerHTML = "<td></td><td></td><td></td><td class='tag-cell'></td>";
  const a = document.createElement("span");
  a.className = "path-link";
  a.textContent = dir ? "📁 " + name : "📄 " + name;
  a.onclick = () => dir ? loadPath(e.FullPath) : showTags(e.FullPath, tr.children[3]);
  tr.children[0].appendChild(a);
  tr.children[1].textContent = dir ? "" : (e.FileSize || 0);
  tr.children[2].textContent = e.Mime || "";
  document.getElementById("entries").appendChild(tr);
  if (!dir) showTags(e.FullPath, tr.children[3]);
}

async function showTags(path, cell) {
  const res = await fetch("/api/tags?path=" + encodeURIComponent(path));
  const data = await res.json();
  cell.textContent = data.tags ? data.tags.join(",") : "";
}

function showMenu(ev, entry, dir) {
  ev.preventDefault();
  selected = {path: entry.FullPath, dir};
  const menu = document.getElementById("menu");
  menu.style.left = ev.clientX + "px";
  menu.style.top = ev.clientY + "px";
  menu.style.display = "block";
}

async function tagOp(op) {
  if (!selected) return;
  const tags = prompt(op + " tags for " + selected.path + (selected.dir ? " recursively" : ""), "");
  if (!tags) return;
  const res = await fetch("/api/tags", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({op, path: selected.path, recursive: selected.dir, tags: tags.split(/[,\s]+/).filter(Boolean)})
  });
  const data = await res.json();
  if (data.error) return setStatus(data.error);
  setStatus(op + " updated " + data.updated + " file(s)");
  loadPath(currentPath);
}

async function runQuery() {
  const expr = document.getElementById("query").value;
  const res = await fetch("/api/query?expr=" + encodeURIComponent(expr) + "&under=" + encodeURIComponent(currentPath));
  const data = await res.json();
  if (data.error) return setStatus(data.error);
  const tbody = document.getElementById("entries");
  tbody.innerHTML = "";
  for (const path of data.paths || []) addRow({FullPath: path, FileSize: "", Mime: "query result"}, baseName(path), false);
  setStatus("Query returned " + (data.paths || []).length + " file(s)");
}

function baseName(path) { const p = path.replace(/\/$/, "").split("/"); return p[p.length - 1] || "/"; }
function parentPath(path) { const p = path.replace(/\/$/, "").split("/"); p.pop(); return p.join("/") || "/"; }
function setStatus(msg) { document.getElementById("status").textContent = msg; }
loadPath("/");
</script>
</body>
</html>`
