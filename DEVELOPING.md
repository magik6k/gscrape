# Developing gscrape

This guide covers everything you need to know to develop, test, and extend gscrape.

## Prerequisites

- Go 1.21 or later
- A Go application with pprof enabled (for testing scraping)
- ~2GB disk space for sample data

## Quick Start

```bash
# Clone and build
git clone https://github.com/magik6k/gscrape
cd gscrape
go build ./cmd/gscrape
go build ./cmd/gcount  
go build ./cmd/gindex
go build ./cmd/gweb

# Start scraping (requires running Go app with pprof)
./gscrape -interval 15s http://localhost:6060

# Build the index
./gindex -cmd index -input output -db gindex.db

# Launch web UI
./gweb -db gindex.db -addr :8080
```

## Project Structure

```
gscrape/
├── cmd/
│   ├── gscrape/main.go    # HTTP scraper
│   ├── gcount/main.go     # Format converter (debug=2 → debug=1)
│   ├── gindex/main.go     # Pebble DB indexer
│   └── gweb/main.go       # Web UI server
├── output/                 # Default scrape output (gitignored)
├── gindex.db/             # Default database path (gitignored)
├── go.mod
├── go.sum
├── README.md
├── DEVELOPING.md          # This file
├── AGENTS.md              # AI agent guidelines
├── LICENSE
└── screenshot.png
```

## Tool Deep Dives

### gscrape - The Scraper

**Location**: `cmd/gscrape/main.go` (~290 lines)

**Purpose**: Periodically fetch goroutine dumps from Go applications exposing pprof.

**Key Components**:

```go
// Scraper holds HTTP client and statistics
type Scraper struct {
    client   *http.Client
    outDir   string
    interval time.Duration
    stats    map[string]*HostStats  // Per-host data rate tracking
}

// HostStats tracks rolling 1-hour data rate
type HostStats struct {
    samples []sample  // (timestamp, bytes) pairs
}
```

**Data Flow**:
1. Parse endpoint URLs from command line args
2. Every interval, spawn goroutines to fetch each endpoint in parallel
3. Fetch `/debug/pprof/goroutine?debug=2` 
4. Save gzip-compressed to `output/<host>/<timestamp>.goroutines.txt.gz`
5. Log data rates (raw size, compressed size, hourly rate)

**Configuration**:
```bash
./gscrape [flags] <endpoint1> <endpoint2> ...

Flags:
  -interval duration    Scrape interval (default 15s)
  -output string        Output directory (default "output")
  -timeout duration     HTTP request timeout (default 30s)
```

**File Naming Convention**:
- Host directories: `10.2.4.19_12300` (colons → underscores)
- Files: `2026-01-17T14-33-01.goroutines.txt.gz`

---

### gcount - The Format Converter

**Location**: `cmd/gcount/main.go` (~300 lines)

**Purpose**: Convert verbose debug=2 format (one block per goroutine) to grouped debug=1 format (one block per unique stack).

**Key Components**:

```go
// goroutineGroup represents goroutines with identical stacks
type goroutineGroup struct {
    count int
    state string
    stack string       // Normalized stack trace
    waits []int        // Wait times in minutes
}
```

**Stack Normalization**:
```go
// Before: github.com/pkg.Func(0xc0001234, 0x5678)
// After:  github.com/pkg.Func(..., ...)

// Before: created by main.run in goroutine 1
// After:  created by main.run

// Regex patterns used:
offsetRe    = regexp.MustCompile(`\+0x[0-9a-fA-F]+\s*$`)
createdByRe = regexp.MustCompile(`(created by .+) in goroutine \d+`)
hexPtrRe    = regexp.MustCompile(`0x[0-9a-fA-F]+\??`)
```

**Usage**:
```bash
# Process all dumps in output/ directory
./gcount -input output -output goro-counts -workers 8
```

---

### gindex - The Indexer

**Location**: `cmd/gindex/main.go` (~880 lines)

**Purpose**: Build a searchable Pebble database from scraped dumps.

**Database Schema**:

| Key | Value | Description |
|-----|-------|-------------|
| `g:<host>:<goroID>` | gzip JSON | Goroutine time series |
| `c:<host>:<parentID>` | gzip JSON | Children goroutines list |
| `s:<host>` | gzip JSON | Pre-computed stats (timestamps, counts) |
| `f:<funcName>` | gzip JSON | Function occurrence index |
| `m:hosts` | JSON | List of all hosts |
| `m:funcs` | JSON | List of all function names |

**Key Data Structures**:

```go
type StackEntry struct {
    Timestamp int64  `json:"t"`           // Unix seconds
    State     string `json:"s"`           // "IO wait", "select", etc.
    Stack     string `json:"k"`           // Normalized stack
    CreatedBy int64  `json:"c,omitempty"` // Parent goroutine ID
}

type GoroutineTimeSeries struct {
    Entries []StackEntry `json:"e"`
}

type ChildInfo struct {
    ID        int64  `json:"i"`  // Child goroutine ID
    Funcs     string `json:"f"`  // Entry point functions
    FirstSeen int64  `json:"s"`  // Unix timestamp
    LastSeen  int64  `json:"e"`  // Unix timestamp
}
```

**Processing Pipeline**:

```
1. findHosts()           → List directories in input/
2. For each host:
   a. parseSnapshotFile() → Parse each .goroutines.txt.gz (parallel)
   b. Build goroSeries    → Map[goroID] → []StackEntry
   c. Build childrenIndex → Map[parentID] → []ChildInfo
   d. Build funcOccurrences → Map[funcName] → []occurrence (parallel)
   e. Write all to Pebble DB
3. Store metadata (hosts list, functions list)
```

**Parallel Processing**:
- File parsing: Worker pool reads/parses files concurrently
- Function indexing: Goroutine IDs split into chunks, processed in parallel

**Commands**:
```bash
# Build/rebuild index
./gindex -cmd index -input output -db gindex.db -workers 8

# Query functions by pattern
./gindex -cmd query -db gindex.db -func "handleRequest"

# List all indexed functions
./gindex -cmd list-funcs -db gindex.db -func "http"
```

---

### gweb - The Web UI

**Location**: `cmd/gweb/main.go` (~1200 lines)

**Purpose**: Serve a web interface for exploring goroutine data.

**Architecture**:
- Single Go file with embedded HTML/CSS/JS
- Read-only Pebble database access
- RESTful JSON API + single-page app

**API Endpoints**:

| Endpoint | Method | Parameters | Response |
|----------|--------|------------|----------|
| `/api/hosts` | GET | - | `["host1", "host2"]` |
| `/api/goroutine` | GET | `host`, `id` | `GoroutineTimeSeries` |
| `/api/search` | GET | `host`, `id` (optional) | `[{id, count, first, last}]` |
| `/api/stats` | GET | - | `[{host, timestamps, counts}]` |
| `/api/children` | GET | `host`, `id` | `[{id, funcs, first, last}]` |

**Web UI Structure** (embedded in `handleIndex()`):

```
Lines 262-555:   CSS styles
Lines 557-637:   HTML structure
Lines 639-1179:  JavaScript application
```

**JavaScript Application State**:
```javascript
let hosts = [];           // Available hosts
let currentData = null;   // Current goroutine's time series
let currentFrame = 0;     // Current position in timeline
let playing = false;      // Playback state
let playInterval = null;  // Playback timer
let previousStack = '';   // For diff highlighting
let goroChart = null;     // Overview chart instance
let viewerChart = null;   // Children chart instance
let statsData = null;     // Cached stats for all hosts
let childrenData = null;  // Current goroutine's children
let childrenVisible = false;
```

**Key Functions**:
- `init()` - Load hosts, check URL params, initialize charts
- `loadGoroutine()` - Fetch and display goroutine data
- `loadChildren()` - Fetch children and render chart
- `renderFrame()` - Display current stack with diff highlighting
- `renderViewerChart()` - Draw active children chart

**Chart.js Dependencies** (loaded from CDN):
- `chart.js` - Core charting library
- `chartjs-adapter-date-fns` - Time scale support
- `chartjs-plugin-annotation` - Vertical line markers

---

## Adding Features

### Adding a New Field to Goroutine Data

1. **Update StackEntry** in `cmd/gindex/main.go`:
```go
type StackEntry struct {
    Timestamp int64  `json:"t"`
    State     string `json:"s"`
    Stack     string `json:"k"`
    CreatedBy int64  `json:"c,omitempty"`
    NewField  string `json:"n,omitempty"`  // Add new field
}
```

2. **Extract data** in `parseGoroutineBlock()`:
```go
func parseGoroutineBlock(block string) *parsedGoroutine {
    // ... existing parsing ...
    
    // Extract new field
    var newField string
    if match := newFieldRe.FindStringSubmatch(block); match != nil {
        newField = match[1]
    }
    
    return &parsedGoroutine{
        state:     state,
        stack:     strings.Join(stackLines, "\n"),
        funcs:     funcs,
        createdBy: createdBy,
        newField:  newField,  // Add to struct
    }
}
```

3. **Store in database** in `processHost()`:
```go
goroSeries[goroID].Entries = append(goroSeries[goroID].Entries, StackEntry{
    Timestamp: ts,
    State:     g.state,
    Stack:     g.stack,
    CreatedBy: g.createdBy,
    NewField:  g.newField,  // Include new field
})
```

4. **Update gweb StackEntry** in `cmd/gweb/main.go`:
```go
type StackEntry struct {
    Timestamp int64  `json:"t"`
    State     string `json:"s"`
    Stack     string `json:"k"`
    CreatedBy int64  `json:"c,omitempty"`
    NewField  string `json:"n,omitempty"`  // Match gindex
}
```

5. **Display in UI** - Update JavaScript `renderFrame()`:
```javascript
function renderFrame() {
    const entry = currentData.e[currentFrame];
    // ... existing code ...
    
    // Display new field
    if (entry.n) {
        document.getElementById('newField').textContent = entry.n;
    }
}
```

6. **Rebuild index**:
```bash
rm -rf gindex.db && ./gindex -cmd index -input output -db gindex.db
```

### Adding a New API Endpoint

1. **Add handler** in `cmd/gweb/main.go`:
```go
func handleNewEndpoint(w http.ResponseWriter, r *http.Request) {
    param := r.URL.Query().Get("param")
    if param == "" {
        http.Error(w, "param required", http.StatusBadRequest)
        return
    }
    
    // Query database
    key := []byte("prefix:" + param)
    val, closer, err := db.Get(key)
    if err != nil {
        http.Error(w, "Not found", http.StatusNotFound)
        return
    }
    defer closer.Close()
    
    var result YourType
    if err := decompressJSON(val, &result); err != nil {
        http.Error(w, "Decode error", http.StatusInternalServerError)
        return
    }
    
    writeJSON(w, result)
}
```

2. **Register route** in `main()`:
```go
http.HandleFunc("/api/newendpoint", handleNewEndpoint)
```

3. **Call from JavaScript**:
```javascript
async function fetchNewData(param) {
    const resp = await fetch('/api/newendpoint?param=' + encodeURIComponent(param));
    if (!resp.ok) throw new Error('Failed to fetch');
    return await resp.json();
}
```

### Adding a New Database Index

1. **Define key pattern** - Choose a prefix like `x:` for your index

2. **Build during indexing** in `processHost()`:
```go
// After building goroSeries, create your index
yourIndex := make(map[string][]YourData)

for goroID, series := range goroSeries {
    for _, entry := range series.Entries {
        // Extract and index data
        key := extractKey(entry)
        yourIndex[key] = append(yourIndex[key], YourData{...})
    }
}

// Write to database
for key, data := range yourIndex {
    dbKey := []byte("x:" + key)
    if value, err := compressJSON(data); err == nil {
        db.Set(dbKey, value, pebble.NoSync)
    }
}
```

3. **Query in gweb** - Add handler as shown above

---

## Debugging

### Inspecting the Database

```go
// Add to gindex or create separate tool
func dumpKeys(dbPath string) {
    db, _ := pebble.Open(dbPath, &pebble.Options{ReadOnly: true})
    defer db.Close()
    
    iter, _ := db.NewIter(nil)
    defer iter.Close()
    
    for iter.First(); iter.Valid(); iter.Next() {
        fmt.Printf("%s: %d bytes\n", iter.Key(), len(iter.Value()))
    }
}
```

### Debugging the Web UI

1. Open browser DevTools (F12)
2. Check Network tab for API responses
3. Check Console for JavaScript errors
4. Add `console.log()` statements in embedded JS

### Common Issues

**"Goroutine not found"**
- Check host name matches exactly (including port)
- Verify goroutine ID exists in index
- Check URL encoding of parameters

**Chart not rendering**
- Verify Chart.js CDN is accessible
- Check browser console for errors
- Ensure canvas element exists before creating chart

**Index rebuild hangs**
- Check available memory (indexer loads all data per host)
- Reduce worker count with `-workers 2`
- Process hosts one at a time for debugging

---

## Testing

### Manual Testing Workflow

```bash
# 1. Start a test Go application with pprof
go run testapp.go  # Must import net/http/pprof

# 2. Scrape for a while
./gscrape -interval 5s http://localhost:6060 &
sleep 60
kill %1

# 3. Build index
./gindex -cmd index -input output -db test.db

# 4. Run web UI
./gweb -db test.db -addr :8080

# 5. Open http://localhost:8080 and verify functionality
```

### Sample Test Application

```go
package main

import (
    "net/http"
    _ "net/http/pprof"
    "sync"
    "time"
)

func main() {
    // Spawn some goroutines
    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            worker(id)
        }(i)
    }
    
    http.ListenAndServe(":6060", nil)
}

func worker(id int) {
    for {
        doWork(id)
        time.Sleep(time.Second)
    }
}

func doWork(id int) {
    // Simulate work
    time.Sleep(100 * time.Millisecond)
}
```

---

## Performance Tuning

### Scraper

- Increase `-timeout` for slow networks
- Adjust `-interval` based on dump size (larger dumps = longer intervals)
- Monitor disk space; dumps accumulate quickly

### Indexer

- Use `-workers` matching CPU cores
- For large datasets, increase system memory
- SSD storage significantly speeds up Pebble writes

### Web UI

- Stats are pre-computed, so overview chart loads fast
- Goroutine data loaded on-demand to minimize memory
- Children chart computed client-side from cached data

---

## Code Style

- Single-file tools for simplicity
- Minimal external dependencies
- Embedded web UI (no separate frontend build)
- Gzip compression for all stored data
- JSON for all API responses
- Unix timestamps (seconds) in database
- ISO strings in UI for readability

---

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make changes following the patterns above
4. Test manually with sample data
5. Submit a pull request

For significant changes, open an issue first to discuss the approach.
