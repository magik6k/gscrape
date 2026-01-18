package main

import (
	"bufio"
	"compress/gzip"
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
)

func main() {
	var (
		inputDir  = flag.String("input", "output", "Input directory containing scraped goroutine dumps")
		outputDir = flag.String("output", "goro-counts", "Output directory for grouped counts")
		workers   = flag.Int("workers", runtime.NumCPU(), "Number of worker goroutines")
	)
	flag.Parse()

	// Find all .goroutines.txt.gz files
	var files []string
	err := filepath.Walk(*inputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".goroutines.txt.gz") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("Failed to walk input directory: %v", err)
	}

	if len(files) == 0 {
		log.Println("No .goroutines.txt.gz files found")
		return
	}

	log.Printf("Found %d files to process with %d workers", len(files), *workers)

	// Create work channel and start workers
	workCh := make(chan workItem, len(files))
	var wg sync.WaitGroup

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(workCh)
		}()
	}

	// Queue work
	for _, f := range files {
		// Extract host directory and filename
		// Input:  output/<host>/<timestamp>.goroutines.txt.gz
		// Output: goro-counts/<host>/<timestamp>.goroutines.txt
		rel, err := filepath.Rel(*inputDir, f)
		if err != nil {
			log.Printf("Failed to get relative path for %s: %v", f, err)
			continue
		}

		outPath := filepath.Join(*outputDir, strings.TrimSuffix(rel, ".gz"))
		workCh <- workItem{
			inputPath:  f,
			outputPath: outPath,
		}
	}
	close(workCh)

	wg.Wait()
	log.Println("Done")
}

type workItem struct {
	inputPath  string
	outputPath string
}

func worker(ch <-chan workItem) {
	for item := range ch {
		if err := processFile(item.inputPath, item.outputPath); err != nil {
			log.Printf("[%s] ERROR: %v", item.inputPath, err)
		} else {
			log.Printf("[%s] -> %s", item.inputPath, item.outputPath)
		}
	}
}

func processFile(inputPath, outputPath string) error {
	// Read and decompress input
	f, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()

	data, err := io.ReadAll(gr)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	// Parse and group goroutines
	grouped := parseAndGroup(string(data))

	// Format output like debug=1
	output := formatDebug1(grouped)

	// Write output
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	if err := os.WriteFile(outputPath, []byte(output), 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return nil
}

// goroutineGroup represents a group of goroutines with the same stack trace
type goroutineGroup struct {
	count int
	state string
	stack string
	waits []int // wait times in minutes (if available)
}

// parseAndGroup parses debug=2 output and groups goroutines by stack trace
func parseAndGroup(data string) map[string]*goroutineGroup {
	groups := make(map[string]*goroutineGroup)

	// Split into individual goroutine blocks
	// Each goroutine starts with "goroutine N [state]:" or "goroutine N [state, M minutes]:"
	goroutineHeaderRe := regexp.MustCompile(`(?m)^goroutine \d+ \[([^\]]+)\]:`)

	// Find all goroutine blocks
	matches := goroutineHeaderRe.FindAllStringIndex(data, -1)
	if len(matches) == 0 {
		return groups
	}

	for i, match := range matches {
		start := match[0]
		end := len(data)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}

		block := strings.TrimSpace(data[start:end])
		parseGoroutineBlock(block, groups)
	}

	return groups
}

// parseGoroutineBlock parses a single goroutine block and adds it to the groups
func parseGoroutineBlock(block string, groups map[string]*goroutineGroup) {
	lines := strings.Split(block, "\n")
	if len(lines) < 1 {
		return
	}

	// Parse header: "goroutine N [state]:" or "goroutine N [state, M minutes]:"
	headerRe := regexp.MustCompile(`^goroutine \d+ \[([^\],]+)(?:,\s*(\d+)\s*minutes?)?\]:`)
	match := headerRe.FindStringSubmatch(lines[0])
	if match == nil {
		return
	}

	state := match[1]
	waitMinutes := 0
	if match[2] != "" {
		fmt.Sscanf(match[2], "%d", &waitMinutes)
	}

	// Extract stack trace (skip header line)
	// For grouping, we use the stack trace without addresses
	var stackLines []string
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Remove addresses like "0x12345" and "+0x123" for grouping
		// Keep function names and file:line info
		cleaned := cleanStackLine(line)
		stackLines = append(stackLines, cleaned)
	}

	stack := strings.Join(stackLines, "\n")
	key := state + "\n" + stack

	if g, ok := groups[key]; ok {
		g.count++
		if waitMinutes > 0 {
			g.waits = append(g.waits, waitMinutes)
		}
	} else {
		waits := []int{}
		if waitMinutes > 0 {
			waits = append(waits, waitMinutes)
		}
		groups[key] = &goroutineGroup{
			count: 1,
			state: state,
			stack: stack,
			waits: waits,
		}
	}
}

// Precompile regexes for performance
var (
	// Match offset at end of line like +0x123
	offsetRe = regexp.MustCompile(`\+0x[0-9a-fA-F]+\s*$`)
	// Match "created by ... in goroutine N" - normalize goroutine number
	createdByRe = regexp.MustCompile(`(created by .+) in goroutine \d+`)
	// Match hex pointer values like 0x12345 or 0x12345? (with optional ?)
	hexPtrRe = regexp.MustCompile(`0x[0-9a-fA-F]+\??`)
)

// cleanStackLine removes memory addresses for grouping purposes
func cleanStackLine(line string) string {
	// Remove offset at end of file:line like +0x123
	line = offsetRe.ReplaceAllString(line, "")

	// Normalize "created by ... in goroutine N" to remove goroutine number
	line = createdByRe.ReplaceAllString(line, "$1")

	// Replace all hex pointer values with "..."
	// This normalizes (0xc00123, 0x456) -> (..., ...) and {0xc00123, 0x1} -> {..., ...}
	line = hexPtrRe.ReplaceAllString(line, "...")

	return line
}

// formatDebug1 formats the grouped goroutines like pprof debug=1 output
func formatDebug1(groups map[string]*goroutineGroup) string {
	// Sort by count (descending), then by state
	type sortedGroup struct {
		key   string
		group *goroutineGroup
	}

	var sorted []sortedGroup
	for k, g := range groups {
		sorted = append(sorted, sortedGroup{k, g})
	}

	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].group.count != sorted[j].group.count {
			return sorted[i].group.count > sorted[j].group.count
		}
		return sorted[i].group.state < sorted[j].group.state
	})

	var buf strings.Builder

	// Write total count header
	total := 0
	for _, sg := range sorted {
		total += sg.group.count
	}
	buf.WriteString(fmt.Sprintf("goroutine profile: total %d\n", total))

	for _, sg := range sorted {
		g := sg.group

		// Format like debug=1:
		// N [state]:
		// #	function
		// #	file:line
		buf.WriteString(fmt.Sprintf("\n%d [%s]\n", g.count, g.state))

		// Write stack - alternating function and file:line pairs
		scanner := bufio.NewScanner(strings.NewReader(g.stack))
		for scanner.Scan() {
			line := scanner.Text()
			buf.WriteString("#\t")
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}

	return buf.String()
}
