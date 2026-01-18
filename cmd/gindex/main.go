package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
)

/*
Database schema:

Key prefixes:
- "g:<host>:<goroutineID>" -> GoroutineTimeSeries (gzip-compressed JSON)
  Contains: [{timestamp, state, stack}, ...]

- "f:<funcName>" -> FuncIndex (gzip-compressed JSON)
  Contains: [{host, goroutineID, firstSeen, lastSeen}, ...]

- "m:hosts" -> []string (list of all hosts)
- "m:funcs" -> []string (list of all function names)
*/

func main() {
	var (
		inputDir = flag.String("input", "output", "Input directory containing scraped goroutine dumps")
		dbPath   = flag.String("db", "gindex.db", "Path to Pebble database")
		workers  = flag.Int("workers", runtime.NumCPU(), "Number of worker goroutines")
		cmd      = flag.String("cmd", "index", "Command: index, query, list-funcs")
		funcName = flag.String("func", "", "Function name to query (for query command)")
		host     = flag.String("host", "", "Host to filter (optional)")
	)
	flag.Parse()

	switch *cmd {
	case "index":
		runIndex(*inputDir, *dbPath, *workers)
	case "query":
		if *funcName == "" {
			log.Fatal("--func is required for query command")
		}
		runQuery(*dbPath, *funcName, *host)
	case "list-funcs":
		runListFuncs(*dbPath, *funcName)
	default:
		log.Fatalf("Unknown command: %s", *cmd)
	}
}

// ========== Data structures ==========

type StackEntry struct {
	Timestamp int64  `json:"t"`           // Unix timestamp
	State     string `json:"s"`           // e.g., "IO wait", "select"
	Stack     string `json:"k"`           // Normalized stack trace
	CreatedBy int64  `json:"c,omitempty"` // Parent goroutine ID (from "created by ... in goroutine N")
}

type GoroutineTimeSeries struct {
	Entries []StackEntry `json:"e"`
}

type FuncOccurrence struct {
	Host        string `json:"h"`
	GoroutineID int64  `json:"g"`
	FirstSeen   int64  `json:"f"` // Unix timestamp
	LastSeen    int64  `json:"l"` // Unix timestamp
}

type FuncIndex struct {
	Occurrences []FuncOccurrence `json:"o"`
}

// ========== Indexing ==========

func runIndex(inputDir, dbPath string, numWorkers int) {
	// Remove existing DB
	os.RemoveAll(dbPath)

	// Open Pebble with Zstd compression at all levels, quiet logger
	opts := &pebble.Options{
		Levels: []pebble.LevelOptions{
			{Compression: pebble.ZstdCompression},
			{Compression: pebble.ZstdCompression},
			{Compression: pebble.ZstdCompression},
			{Compression: pebble.ZstdCompression},
			{Compression: pebble.ZstdCompression},
			{Compression: pebble.ZstdCompression},
			{Compression: pebble.ZstdCompression},
		},
		Logger: &quietLogger{},
	}
	db, err := pebble.Open(dbPath, opts)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Find all hosts
	hosts, err := findHosts(inputDir)
	if err != nil {
		log.Fatalf("Failed to find hosts: %v", err)
	}
	log.Printf("Found %d hosts", len(hosts))

	// Store hosts metadata
	hostsJSON, _ := json.Marshal(hosts)
	if err := db.Set([]byte("m:hosts"), hostsJSON, pebble.Sync); err != nil {
		log.Fatalf("Failed to store hosts: %v", err)
	}

	// Process each host
	allFuncs := make(map[string]struct{})
	var funcsMu sync.Mutex

	for _, host := range hosts {
		log.Printf("Processing host: %s", host)
		funcs := processHost(db, inputDir, host, numWorkers)
		funcsMu.Lock()
		for f := range funcs {
			allFuncs[f] = struct{}{}
		}
		funcsMu.Unlock()
	}

	// Store function list
	funcList := make([]string, 0, len(allFuncs))
	for f := range allFuncs {
		funcList = append(funcList, f)
	}
	sort.Strings(funcList)
	funcsJSON, _ := json.Marshal(funcList)
	if err := db.Set([]byte("m:funcs"), funcsJSON, pebble.Sync); err != nil {
		log.Fatalf("Failed to store funcs: %v", err)
	}

	log.Printf("Indexing complete. %d unique functions indexed.", len(funcList))
}

func findHosts(inputDir string) ([]string, error) {
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return nil, err
	}
	var hosts []string
	for _, e := range entries {
		if e.IsDir() {
			hosts = append(hosts, e.Name())
		}
	}
	return hosts, nil
}

func processHost(db *pebble.DB, inputDir, host string, numWorkers int) map[string]struct{} {
	hostDir := filepath.Join(inputDir, host)

	// Find all snapshot files
	files, err := filepath.Glob(filepath.Join(hostDir, "*.goroutines.txt.gz"))
	if err != nil {
		log.Printf("Error finding files for %s: %v", host, err)
		return nil
	}

	sort.Strings(files) // Sort by timestamp

	// Parse all files and collect goroutine data
	type parseResult struct {
		timestamp time.Time
		goros     map[int64]*parsedGoroutine
	}

	results := make(chan parseResult, len(files))
	fileCh := make(chan string, len(files))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range fileCh {
				ts, goros := parseSnapshotFile(file)
				if goros != nil {
					results <- parseResult{timestamp: ts, goros: goros}
				}
			}
		}()
	}

	// Queue work
	for _, f := range files {
		fileCh <- f
	}
	close(fileCh)

	// Wait for workers and close results
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	var allResults []parseResult
	for r := range results {
		allResults = append(allResults, r)
	}

	// Sort by timestamp
	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].timestamp.Before(allResults[j].timestamp)
	})

	log.Printf("  Parsed %d snapshots for %s", len(allResults), host)

	// Build goroutine time series and stats
	// First pass: collect stats (fast) and build goroSeries
	goroSeries := make(map[int64]*GoroutineTimeSeries)

	// Stats: timestamp -> goroutine count
	statsTimestamps := make([]int64, 0, len(allResults))
	statsCounts := make([]int, 0, len(allResults))

	for _, r := range allResults {
		ts := r.timestamp.Unix()

		// Record stats
		statsTimestamps = append(statsTimestamps, ts)
		statsCounts = append(statsCounts, len(r.goros))

		for goroID, g := range r.goros {
			// Add to time series
			if goroSeries[goroID] == nil {
				goroSeries[goroID] = &GoroutineTimeSeries{}
			}
			goroSeries[goroID].Entries = append(goroSeries[goroID].Entries, StackEntry{
				Timestamp: ts,
				State:     g.state,
				Stack:     g.stack,
				CreatedBy: g.createdBy,
			})
		}
	}

	// Second pass: build function occurrences in parallel
	// Split goroutine IDs into chunks for parallel processing
	goroIDs := make([]int64, 0, len(goroSeries))
	for id := range goroSeries {
		goroIDs = append(goroIDs, id)
	}

	type funcOccResult struct {
		fn     string
		goroID int64
		occ    *FuncOccurrence
	}

	resultCh := make(chan []funcOccResult, numWorkers)
	chunkSize := (len(goroIDs) + numWorkers - 1) / numWorkers

	var funcWg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		start := i * chunkSize
		if start >= len(goroIDs) {
			break
		}
		end := start + chunkSize
		if end > len(goroIDs) {
			end = len(goroIDs)
		}

		funcWg.Add(1)
		go func(ids []int64) {
			defer funcWg.Done()
			localResults := make([]funcOccResult, 0, 1000)
			localFuncMap := make(map[string]map[int64]*FuncOccurrence)

			for _, goroID := range ids {
				series := goroSeries[goroID]
				if len(series.Entries) == 0 {
					continue
				}

				// Collect all unique functions across all entries for this goroutine
				allFuncs := make(map[string]struct{})
				var firstTs, lastTs int64
				firstTs = series.Entries[0].Timestamp
				lastTs = series.Entries[len(series.Entries)-1].Timestamp

				for _, entry := range series.Entries {
					// Extract function names from stack
					funcs := extractFuncsFromStack(entry.Stack)
					for _, fn := range funcs {
						allFuncs[fn] = struct{}{}
					}
				}

				// Build occurrences
				for fn := range allFuncs {
					if localFuncMap[fn] == nil {
						localFuncMap[fn] = make(map[int64]*FuncOccurrence)
					}
					localFuncMap[fn][goroID] = &FuncOccurrence{
						Host:        host,
						GoroutineID: goroID,
						FirstSeen:   firstTs,
						LastSeen:    lastTs,
					}
				}
			}

			// Convert to results
			for fn, goroMap := range localFuncMap {
				for goroID, occ := range goroMap {
					localResults = append(localResults, funcOccResult{fn: fn, goroID: goroID, occ: occ})
				}
			}
			resultCh <- localResults
		}(goroIDs[start:end])
	}

	go func() {
		funcWg.Wait()
		close(resultCh)
	}()

	// Merge results
	funcOccurrences := make(map[string]map[int64]*FuncOccurrence)
	for results := range resultCh {
		for _, r := range results {
			if funcOccurrences[r.fn] == nil {
				funcOccurrences[r.fn] = make(map[int64]*FuncOccurrence)
			}
			funcOccurrences[r.fn][r.goroID] = r.occ
		}
	}

	// Write goroutine time series to DB
	batch := db.NewBatch()
	batchSize := 0
	for goroID, series := range goroSeries {
		key := fmt.Sprintf("g:%s:%d", host, goroID)
		value, err := compressJSON(series)
		if err != nil {
			log.Printf("Error compressing goroutine series: %v", err)
			continue
		}
		if err := batch.Set([]byte(key), value, nil); err != nil {
			log.Printf("Error writing goroutine series: %v", err)
		}
		batchSize++
		if batchSize >= 1000 {
			batch.Commit(pebble.Sync)
			batch = db.NewBatch()
			batchSize = 0
		}
	}
	if batchSize > 0 {
		batch.Commit(pebble.Sync)
	}

	log.Printf("  Wrote %d goroutine time series for %s", len(goroSeries), host)

	// Build and store children index
	// Map: parentGoroID -> []ChildInfo
	type ChildInfo struct {
		ID        int64  `json:"i"`
		Funcs     string `json:"f"` // First two function names
		FirstSeen int64  `json:"s"`
		LastSeen  int64  `json:"e"`
	}
	childrenIndex := make(map[int64][]ChildInfo)

	for goroID, series := range goroSeries {
		if len(series.Entries) == 0 {
			continue
		}

		// Find parent ID - check all entries since first entry might not have it
		var parentID int64
		var funcs string
		for _, entry := range series.Entries {
			if entry.CreatedBy != 0 {
				parentID = entry.CreatedBy
				// Use the stack from the entry where we found the parent
				funcs = extractFirstTwoFuncs(entry.Stack)
				break
			}
		}

		if parentID == 0 {
			continue
		}

		childrenIndex[parentID] = append(childrenIndex[parentID], ChildInfo{
			ID:        goroID,
			Funcs:     funcs,
			FirstSeen: series.Entries[0].Timestamp,
			LastSeen:  series.Entries[len(series.Entries)-1].Timestamp,
		})
	}

	// Write children index
	for parentID, children := range childrenIndex {
		key := fmt.Sprintf("c:%s:%d", host, parentID)
		if value, err := compressJSON(children); err == nil {
			if err := db.Set([]byte(key), value, pebble.NoSync); err != nil {
				log.Printf("Error writing children index: %v", err)
			}
		}
	}
	log.Printf("  Indexed children for %d parent goroutines for %s", len(childrenIndex), host)

	// Store stats for this host
	statsData := struct {
		Timestamps []int64 `json:"t"`
		Counts     []int   `json:"c"`
	}{
		Timestamps: statsTimestamps,
		Counts:     statsCounts,
	}
	if statsValue, err := compressJSON(&statsData); err == nil {
		if err := db.Set([]byte("s:"+host), statsValue, pebble.Sync); err != nil {
			log.Printf("Error writing stats: %v", err)
		}
	}

	// Merge function occurrences into existing index
	allFuncs := make(map[string]struct{})
	for funcName, goroMap := range funcOccurrences {
		allFuncs[funcName] = struct{}{}

		key := []byte("f:" + funcName)

		// Read existing
		var existing FuncIndex
		if val, closer, err := db.Get(key); err == nil {
			decompressJSON(val, &existing)
			closer.Close()
		}

		// Add new occurrences
		for _, occ := range goroMap {
			existing.Occurrences = append(existing.Occurrences, *occ)
		}

		// Write back
		value, err := compressJSON(&existing)
		if err != nil {
			log.Printf("Error compressing func index: %v", err)
			continue
		}
		if err := db.Set(key, value, pebble.NoSync); err != nil {
			log.Printf("Error writing func index: %v", err)
		}
	}

	log.Printf("  Indexed %d functions for %s", len(funcOccurrences), host)

	return allFuncs
}

type parsedGoroutine struct {
	state     string
	stack     string
	funcs     []string // function names in this stack
	createdBy int64    // parent goroutine ID
}

var (
	goroHeaderRe = regexp.MustCompile(`(?m)^goroutine (\d+) \[([^\],]+)`)
	// Match function names like:
	// - github.com/pkg.Func
	// - github.com/pkg.(*Type).Method
	// - github.com/pkg.Type.Method
	funcNameRe        = regexp.MustCompile(`^([a-zA-Z0-9_./\-@]+(?:\.\(\*?[a-zA-Z0-9_]+\))?(?:\.[a-zA-Z0-9_]+)+)`)
	hexPtrRe          = regexp.MustCompile(`0x[0-9a-fA-F]+\??`)
	offsetRe          = regexp.MustCompile(`\+0x[0-9a-fA-F]+\s*$`)
	createdByRe       = regexp.MustCompile(`(created by .+) in goroutine \d+`)
	createdByGoroIDRe = regexp.MustCompile(`in goroutine (\d+)\s*$`)
)

func parseSnapshotFile(path string) (time.Time, map[int64]*parsedGoroutine) {
	// Extract timestamp from filename: 2026-01-17T14-33-01.goroutines.txt.gz
	base := filepath.Base(path)
	tsStr := strings.TrimSuffix(base, ".goroutines.txt.gz")
	ts, err := time.Parse("2006-01-02T15-04-05", tsStr)
	if err != nil {
		log.Printf("Failed to parse timestamp from %s: %v", path, err)
		return time.Time{}, nil
	}

	// Read and decompress
	f, err := os.Open(path)
	if err != nil {
		log.Printf("Failed to open %s: %v", path, err)
		return time.Time{}, nil
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		log.Printf("Failed to create gzip reader for %s: %v", path, err)
		return time.Time{}, nil
	}
	defer gr.Close()

	data, err := io.ReadAll(gr)
	if err != nil {
		log.Printf("Failed to read %s: %v", path, err)
		return time.Time{}, nil
	}

	return ts, parseGoroutines(string(data))
}

func parseGoroutines(data string) map[int64]*parsedGoroutine {
	result := make(map[int64]*parsedGoroutine)

	// Find all goroutine blocks
	headerIndices := goroHeaderRe.FindAllStringIndex(data, -1)
	if len(headerIndices) == 0 {
		return result
	}

	for i, match := range headerIndices {
		start := match[0]
		end := len(data)
		if i+1 < len(headerIndices) {
			end = headerIndices[i+1][0]
		}

		block := data[start:end]
		g := parseGoroutineBlock(block)
		if g != nil {
			// Extract goroutine ID from header
			headerMatch := goroHeaderRe.FindStringSubmatch(block)
			if headerMatch != nil {
				var goroID int64
				fmt.Sscanf(headerMatch[1], "%d", &goroID)
				result[goroID] = g
			}
		}
	}

	return result
}

func parseGoroutineBlock(block string) *parsedGoroutine {
	lines := strings.Split(block, "\n")
	if len(lines) < 1 {
		return nil
	}

	// Parse header
	headerMatch := goroHeaderRe.FindStringSubmatch(lines[0])
	if headerMatch == nil {
		return nil
	}

	state := headerMatch[2]

	// Extract stack and function names
	var stackLines []string
	funcsMap := make(map[string]struct{})
	var createdBy int64

	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Extract parent goroutine ID from "created by ... in goroutine N"
		if createdBy == 0 {
			if match := createdByGoroIDRe.FindStringSubmatch(line); match != nil {
				fmt.Sscanf(match[1], "%d", &createdBy)
			}
		}

		// Normalize the line
		normalized := offsetRe.ReplaceAllString(line, "")
		normalized = createdByRe.ReplaceAllString(normalized, "$1")
		normalized = hexPtrRe.ReplaceAllString(normalized, "...")

		stackLines = append(stackLines, normalized)

		// Extract function name (first line of each frame pair)
		if !strings.HasPrefix(line, "/") && !strings.HasPrefix(line, "\t/") && !strings.Contains(line, ".go:") {
			// This is likely a function line
			fnMatch := funcNameRe.FindString(line)
			if fnMatch != "" {
				// Clean up the function name
				fn := cleanFuncName(fnMatch)
				if fn != "" {
					funcsMap[fn] = struct{}{}
				}
			}
		}
	}

	funcs := make([]string, 0, len(funcsMap))
	for fn := range funcsMap {
		funcs = append(funcs, fn)
	}

	return &parsedGoroutine{
		state:     state,
		stack:     strings.Join(stackLines, "\n"),
		funcs:     funcs,
		createdBy: createdBy,
	}
}

// extractFuncsFromStack extracts all function names from a normalized stack trace
func extractFuncsFromStack(stack string) []string {
	lines := strings.Split(stack, "\n")
	var funcs []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, ".go:") {
			continue
		}
		if strings.HasPrefix(line, "created by") {
			continue
		}
		// Extract function name
		fn := line
		if idx := strings.Index(fn, "("); idx > 0 {
			// Check if it's a method receiver like (*Type)
			beforeParen := fn[:idx]
			if len(beforeParen) > 0 {
				lastChar := beforeParen[len(beforeParen)-1]
				if lastChar != '.' && lastChar != '*' {
					fn = beforeParen
				}
			}
		}
		// Clean the function name (remove path)
		fn = cleanFuncName(fn)
		if fn != "" {
			funcs = append(funcs, fn)
		}
	}
	return funcs
}

// extractBottomTwoFuncs extracts the bottom two function names from a stack trace (entry point)
// This shows where the goroutine started, not what it's currently doing
func extractFirstTwoFuncs(stack string) string {
	lines := strings.Split(stack, "\n")

	// Collect all function lines first
	var allFuncs []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip file lines (contain .go:)
		if line == "" || strings.Contains(line, ".go:") {
			continue
		}
		// Skip "created by" lines
		if strings.HasPrefix(line, "created by") {
			continue
		}

		fn := line

		// Find the last slash to get the package portion first
		lastSlash := strings.LastIndex(fn, "/")
		if lastSlash >= 0 {
			fn = fn[lastSlash+1:]
		}

		// Remove the trailing arguments - find the last '(' that starts the args
		if lastParen := strings.LastIndex(fn, "("); lastParen > 0 {
			beforeParen := fn[:lastParen]
			if len(beforeParen) > 0 {
				lastChar := beforeParen[len(beforeParen)-1]
				if lastChar != '.' && lastChar != '*' {
					fn = beforeParen
				}
			}
		}

		allFuncs = append(allFuncs, fn)
	}

	// Take the last two (bottom of stack = entry point)
	if len(allFuncs) == 0 {
		return ""
	}
	if len(allFuncs) == 1 {
		return allFuncs[0]
	}
	// Return bottom two in order: second-to-last -> last (entry point)
	return allFuncs[len(allFuncs)-2] + " -> " + allFuncs[len(allFuncs)-1]
}

func cleanFuncName(fn string) string {
	// Remove trailing function arguments (...) but preserve method receivers like (*Type)
	// Find the last ) that's part of a method receiver pattern .\(*...\)
	// Then find any trailing ( that starts function arguments

	// Look for the pattern ).MethodName( which indicates end of receiver + method + args
	// Or just .FuncName( for regular functions
	lastDot := strings.LastIndex(fn, ".")
	if lastDot > 0 {
		// Check if there's a ( after the last dot (function arguments)
		afterDot := fn[lastDot:]
		if parenIdx := strings.Index(afterDot, "("); parenIdx > 0 {
			// This is function arguments, not a method receiver
			fn = fn[:lastDot+parenIdx]
		}
	}
	return fn
}

func compressJSON(v interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(jsonData); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompressJSON(data []byte, v interface{}) error {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gr.Close()

	jsonData, err := io.ReadAll(gr)
	if err != nil {
		return err
	}

	return json.Unmarshal(jsonData, v)
}

// ========== Querying ==========

func runQuery(dbPath, funcPattern, hostFilter string) {
	db, err := pebble.Open(dbPath, &pebble.Options{ReadOnly: true, Logger: &quietLogger{}})
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Find matching functions
	var funcs []string
	if val, closer, err := db.Get([]byte("m:funcs")); err == nil {
		json.Unmarshal(val, &funcs)
		closer.Close()
	}

	var matchingFuncs []string
	pattern := strings.ToLower(funcPattern)
	for _, fn := range funcs {
		if strings.Contains(strings.ToLower(fn), pattern) {
			matchingFuncs = append(matchingFuncs, fn)
		}
	}

	if len(matchingFuncs) == 0 {
		fmt.Println("No matching functions found")
		return
	}

	fmt.Printf("Found %d matching functions:\n\n", len(matchingFuncs))

	for _, fn := range matchingFuncs {
		key := []byte("f:" + fn)
		val, closer, err := db.Get(key)
		if err != nil {
			continue
		}

		var idx FuncIndex
		if err := decompressJSON(val, &idx); err != nil {
			closer.Close()
			continue
		}
		closer.Close()

		// Filter by host if specified
		var filtered []FuncOccurrence
		for _, occ := range idx.Occurrences {
			if hostFilter == "" || strings.Contains(occ.Host, hostFilter) {
				filtered = append(filtered, occ)
			}
		}

		if len(filtered) == 0 {
			continue
		}

		// Sort by first seen
		sort.Slice(filtered, func(i, j int) bool {
			return filtered[i].FirstSeen < filtered[j].FirstSeen
		})

		fmt.Printf("=== %s ===\n", fn)
		fmt.Printf("Goroutines: %d\n\n", len(filtered))

		fmt.Printf("%-20s %12s %24s %24s %12s\n", "Host", "Goroutine", "First Seen", "Last Seen", "Duration")
		fmt.Printf("%s\n", strings.Repeat("-", 96))

		for _, occ := range filtered {
			firstSeen := time.Unix(occ.FirstSeen, 0).Format("2006-01-02 15:04:05")
			lastSeen := time.Unix(occ.LastSeen, 0).Format("2006-01-02 15:04:05")
			duration := time.Duration(occ.LastSeen-occ.FirstSeen) * time.Second
			fmt.Printf("%-20s %12d %24s %24s %12s\n", occ.Host, occ.GoroutineID, firstSeen, lastSeen, duration)
		}
		fmt.Println()
	}
}

func runListFuncs(dbPath, pattern string) {
	db, err := pebble.Open(dbPath, &pebble.Options{ReadOnly: true, Logger: &quietLogger{}})
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	var funcs []string
	if val, closer, err := db.Get([]byte("m:funcs")); err == nil {
		json.Unmarshal(val, &funcs)
		closer.Close()
	}

	lowerPattern := strings.ToLower(pattern)
	count := 0
	for _, fn := range funcs {
		if pattern == "" || strings.Contains(strings.ToLower(fn), lowerPattern) {
			fmt.Println(fn)
			count++
		}
	}
	fmt.Printf("\n%d functions\n", count)
}

// ========== Utility ==========

type quietLogger struct{}

func (q *quietLogger) Infof(format string, args ...interface{})  {}
func (q *quietLogger) Errorf(format string, args ...interface{}) {}
func (q *quietLogger) Fatalf(format string, args ...interface{}) { log.Fatalf(format, args...) }

func int64ToBytes(n int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(n))
	return b
}

func bytesToInt64(b []byte) int64 {
	return int64(binary.BigEndian.Uint64(b))
}
