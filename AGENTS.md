# AI Agent Guidelines for gscrape

This document provides guidance for AI agents (Claude, GPT, Copilot, etc.) working on the gscrape codebase.

## Project Overview

gscrape is a toolkit for scraping, indexing, and analyzing Go pprof goroutine dumps over time. It consists of four command-line tools that work together in a pipeline:

```
Target Go Apps → gscrape (scraper) → gindex (indexer) → gweb (web UI)
                                   ↘ gcount (converter)
```

## Architecture

### Tool Responsibilities

| Tool | Purpose | Input | Output |
|------|---------|-------|--------|
| `gscrape` | Periodic HTTP scraping of pprof endpoints | HTTP endpoints | Gzipped dump files |
| `gcount` | Convert debug=2 format to grouped debug=1 | Gzipped dumps | Grouped text files |
| `gindex` | Build searchable Pebble DB index | Gzipped dumps | Pebble database |
| `gweb` | Web UI for visualization and analysis | Pebble database | HTTP server |

### Data Flow

1. **Scraping**: `gscrape` fetches `/debug/pprof/goroutine?debug=2` from targets every N seconds
2. **Storage**: Raw dumps saved as `output/<host>/<timestamp>.goroutines.txt.gz`
3. **Indexing**: `gindex` parses all dumps, builds time series per goroutine, tracks parent-child relationships
4. **Serving**: `gweb` reads the Pebble DB and serves a single-page web application

### Database Schema (Pebble)

All values are gzip-compressed JSON unless noted otherwise.

| Key Pattern | Value Type | Description |
|-------------|------------|-------------|
| `g:<host>:<goroutineID>` | `GoroutineTimeSeries` | Time series of stack traces for one goroutine |
| `c:<host>:<parentID>` | `[]ChildInfo` | List of children goroutines spawned by parent |
| `s:<host>` | `{timestamps, counts}` | Pre-computed goroutine counts per snapshot |
| `f:<funcName>` | `FuncIndex` | Which goroutines contained this function |
| `m:hosts` | `[]string` (plain JSON) | List of all indexed hosts |
| `m:funcs` | `[]string` (plain JSON) | List of all indexed function names |

## Code Organization

```
cmd/
├── gscrape/main.go   # ~290 lines - HTTP scraper with rate stats
├── gcount/main.go    # ~300 lines - Format converter with regex parsing
├── gindex/main.go    # ~880 lines - Parallel indexer with Pebble DB
└── gweb/main.go      # ~1200 lines - Web server with embedded HTML/CSS/JS
```

### Key Data Structures

```go
// Stored in database for each goroutine
type StackEntry struct {
    Timestamp int64  `json:"t"`           // Unix timestamp
    State     string `json:"s"`           // e.g., "IO wait", "select"
    Stack     string `json:"k"`           // Normalized stack trace
    CreatedBy int64  `json:"c,omitempty"` // Parent goroutine ID
}

type GoroutineTimeSeries struct {
    Entries []StackEntry `json:"e"`
}

// Children index entry
type ChildInfo struct {
    ID        int64  `json:"i"`
    Funcs     string `json:"f"` // Bottom two functions (entry point)
    FirstSeen int64  `json:"s"`
    LastSeen  int64  `json:"e"`
}
```

## Common Tasks

### Adding a New API Endpoint

1. Add handler function in `cmd/gweb/main.go`:
```go
func handleNewEndpoint(w http.ResponseWriter, r *http.Request) {
    // Parse query params
    param := r.URL.Query().Get("param")
    
    // Read from database
    val, closer, err := db.Get([]byte("key:" + param))
    if err != nil {
        http.Error(w, "Not found", http.StatusNotFound)
        return
    }
    defer closer.Close()
    
    // Decompress and return
    var result SomeType
    if err := decompressJSON(val, &result); err != nil {
        http.Error(w, "Decode error", http.StatusInternalServerError)
        return
    }
    writeJSON(w, result)
}
```

2. Register in `main()`:
```go
http.HandleFunc("/api/newendpoint", handleNewEndpoint)
```

3. Call from JavaScript in the embedded HTML.

### Adding a New Database Index

1. Define the key pattern and value structure in `cmd/gindex/main.go`
2. Add population logic in `processHost()` function
3. Write to DB using `compressJSON()` and `db.Set()`
4. Add query logic in `cmd/gweb/main.go` if needed

### Modifying the Web UI

The entire web UI is embedded as a string literal in `cmd/gweb/main.go` within `handleIndex()`. It includes:

- **CSS**: Lines ~262-555 (inline `<style>` block)
- **HTML**: Lines ~557-637 (body content)
- **JavaScript**: Lines ~639-1179 (inline `<script>` block)

Key JavaScript globals:
- `currentData` - Current goroutine's time series
- `currentFrame` - Current position in timeline
- `childrenData` - Children of current goroutine
- `statsData` - Cached host statistics for charts
- `goroChart` / `viewerChart` - Chart.js instances

### Adding New Parsed Data from Goroutine Dumps

1. Update regex patterns in `cmd/gindex/main.go` (around line 483-494)
2. Modify `parseGoroutineBlock()` to extract new data
3. Add field to `StackEntry` struct if per-snapshot
4. Update database write logic in `processHost()`

## Important Patterns

### Compression

All large data is gzip-compressed before storage:
```go
// Compress
value, err := compressJSON(data)
db.Set(key, value, pebble.Sync)

// Decompress
var result Type
decompressJSON(val, &result)
```

### Parallel Processing

The indexer uses worker pools for CPU-bound tasks:
```go
resultCh := make(chan Result, numWorkers)
var wg sync.WaitGroup

for i := 0; i < numWorkers; i++ {
    wg.Add(1)
    go func(chunk []Item) {
        defer wg.Done()
        // Process chunk
        resultCh <- localResult
    }(chunks[i])
}

go func() {
    wg.Wait()
    close(resultCh)
}()

// Collect results
for r := range resultCh {
    merge(r)
}
```

### Stack Trace Normalization

Stack traces are normalized for grouping by removing:
- Memory addresses (`0x12345` → `...`)
- Offset suffixes (`+0x123`)
- Goroutine IDs from "created by" lines

See `cleanStackLine()` in gcount and `parseGoroutineBlock()` in gindex.

## Testing Changes

```bash
# Build all tools
go build ./cmd/gscrape
go build ./cmd/gcount
go build ./cmd/gindex
go build ./cmd/gweb

# Rebuild index (destructive - removes existing)
rm -rf gindex.db && ./gindex -cmd index -input output -db gindex.db

# Run web UI
./gweb -db gindex.db -addr :8080
```

## Performance Considerations

- **Index size**: ~70MB for 5 hosts with ~1400 snapshots each
- **Index time**: ~2 minutes for full rebuild with parallel workers
- **Memory**: Indexer loads all snapshots per host into memory
- **Web UI**: Stats are pre-computed; goroutine data loaded on-demand

## External Dependencies

- `github.com/cockroachdb/pebble` - Key-value storage (used by gindex, gweb)
- Chart.js + chartjs-adapter-date-fns + chartjs-plugin-annotation (CDN, used by gweb)

## Common Gotchas

1. **Host directory names**: Colons replaced with underscores (`10.2.4.19:12300` → `10.2.4.19_12300`)
2. **Timestamp format**: Unix seconds in database, ISO strings in UI
3. **Stack reversal**: UI displays stacks bottom-up (entry point at top) for stable diffs
4. **Parent detection**: Must scan all entries, not just first, as parent info may appear later
5. **Chart.js annotations**: Require separate plugin import for vertical line markers
