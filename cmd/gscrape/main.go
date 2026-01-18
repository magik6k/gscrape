package main

import (
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	var (
		interval = flag.Duration("interval", 15*time.Second, "Scrape interval")
		outDir   = flag.String("output", "output", "Output directory")
		timeout  = flag.Duration("timeout", 30*time.Second, "HTTP request timeout")
	)
	flag.Parse()

	endpoints := flag.Args()
	if len(endpoints) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <endpoint1> <endpoint2> ...\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Example: %s http://10.2.4.19:12300 http://10.2.4.20:12300\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	scraper := &Scraper{
		client: &http.Client{
			Timeout: *timeout,
		},
		outDir:   *outDir,
		interval: *interval,
		stats:    make(map[string]*HostStats),
	}

	log.Printf("Starting scraper with %d endpoints, interval=%s, output=%s", len(endpoints), *interval, *outDir)

	// Initial scrape
	scraper.scrapeAll(ctx, endpoints)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Scraper stopped")
			return
		case <-ticker.C:
			scraper.scrapeAll(ctx, endpoints)
		}
	}
}

// HostStats tracks data rate statistics for a single host
type HostStats struct {
	mu      sync.Mutex
	samples []sample
}

type sample struct {
	timestamp time.Time
	bytes     int64
}

// Record adds a new sample and prunes old ones (older than 1 hour)
func (h *HostStats) Record(bytes int64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Hour)

	// Prune old samples
	validIdx := 0
	for _, s := range h.samples {
		if s.timestamp.After(cutoff) {
			h.samples[validIdx] = s
			validIdx++
		}
	}
	h.samples = h.samples[:validIdx]

	// Add new sample
	h.samples = append(h.samples, sample{timestamp: now, bytes: bytes})
}

// HourlyRate returns the moving average data rate in bytes per hour
func (h *HostStats) HourlyRate() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.samples) < 2 {
		return 0
	}

	// Calculate total bytes and time span
	var totalBytes int64
	for _, s := range h.samples {
		totalBytes += s.bytes
	}

	// Time span from first to last sample
	timeSpan := h.samples[len(h.samples)-1].timestamp.Sub(h.samples[0].timestamp)
	if timeSpan <= 0 {
		return 0
	}

	// Extrapolate to hourly rate
	hoursElapsed := timeSpan.Hours()
	return float64(totalBytes) / hoursElapsed
}

type Scraper struct {
	client   *http.Client
	outDir   string
	interval time.Duration

	statsMu sync.RWMutex
	stats   map[string]*HostStats
}

func (s *Scraper) getStats(host string) *HostStats {
	s.statsMu.RLock()
	st, ok := s.stats[host]
	s.statsMu.RUnlock()
	if ok {
		return st
	}

	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	// Double-check after acquiring write lock
	if st, ok := s.stats[host]; ok {
		return st
	}
	st = &HostStats{}
	s.stats[host] = st
	return st
}

func (s *Scraper) scrapeAll(ctx context.Context, endpoints []string) {
	var wg sync.WaitGroup
	wg.Add(len(endpoints))

	for _, endpoint := range endpoints {
		go func(ep string) {
			defer wg.Done()
			s.scrapeOne(ctx, ep)
		}(endpoint)
	}

	wg.Wait()
}

func (s *Scraper) scrapeOne(ctx context.Context, endpoint string) {
	start := time.Now()

	// Parse endpoint to extract host for directory naming
	parsed, err := url.Parse(endpoint)
	if err != nil {
		log.Printf("[%s] ERROR: invalid URL: %v", endpoint, err)
		return
	}

	// Build the goroutine debug URL
	goroutineURL := endpoint
	if !strings.Contains(endpoint, "/debug/pprof/goroutine") {
		goroutineURL = strings.TrimSuffix(endpoint, "/") + "/debug/pprof/goroutine?debug=2"
	}

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, goroutineURL, nil)
	if err != nil {
		log.Printf("[%s] ERROR: failed to create request: %v", endpoint, err)
		return
	}

	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("[%s] ERROR: request failed: %v", endpoint, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[%s] ERROR: unexpected status code: %d", endpoint, resp.StatusCode)
		return
	}

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[%s] ERROR: failed to read response: %v", endpoint, err)
		return
	}

	// Create output directory: output/<host>/
	hostDir := sanitizeHost(parsed.Host)
	outPath := filepath.Join(s.outDir, hostDir)
	if err := os.MkdirAll(outPath, 0755); err != nil {
		log.Printf("[%s] ERROR: failed to create output dir: %v", endpoint, err)
		return
	}

	// Write to gzipped file: output/<host>/<timestamp>.goroutines.txt.gz
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	filename := filepath.Join(outPath, fmt.Sprintf("%s.goroutines.txt.gz", timestamp))

	compressedSize, err := writeGzipped(filename, body)
	if err != nil {
		log.Printf("[%s] ERROR: failed to write file: %v", endpoint, err)
		return
	}

	// Record stats for this host
	hostStats := s.getStats(parsed.Host)
	hostStats.Record(compressedSize)
	hourlyRate := hostStats.HourlyRate()

	duration := time.Since(start)
	rawMB := float64(len(body)) / 1024 / 1024
	compMB := float64(compressedSize) / 1024 / 1024
	hourlyMB := hourlyRate / 1024 / 1024

	log.Printf("[%s] OK: %.3f MB (%.3f MB gz) in %s, ~%.1f MB/hr -> %s",
		parsed.Host, rawMB, compMB, duration.Round(time.Millisecond), hourlyMB, filename)
}

// writeGzipped writes data to a gzip-compressed file and returns the compressed size
func writeGzipped(filename string, data []byte) (int64, error) {
	f, err := os.Create(filename)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	gw, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return 0, err
	}

	if _, err := gw.Write(data); err != nil {
		gw.Close()
		return 0, err
	}

	if err := gw.Close(); err != nil {
		return 0, err
	}

	// Get the compressed file size
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}

	return info.Size(), nil
}

// sanitizeHost converts a host:port string into a safe directory name
func sanitizeHost(host string) string {
	// Replace colons with underscores for Windows compatibility
	return strings.ReplaceAll(host, ":", "_")
}
