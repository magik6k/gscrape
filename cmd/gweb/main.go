package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/cockroachdb/pebble"
)

var db *pebble.DB

func main() {
	var (
		dbPath = flag.String("db", "gindex.db", "Path to Pebble database")
		addr   = flag.String("addr", ":8080", "Listen address")
	)
	flag.Parse()

	var err error
	db, err = pebble.Open(*dbPath, &pebble.Options{ReadOnly: true, Logger: &quietLogger{}})
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/hosts", handleHosts)
	http.HandleFunc("/api/goroutine", handleGoroutine)
	http.HandleFunc("/api/search", handleSearch)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/api/children", handleChildren)

	log.Printf("Starting web server on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// ========== Data structures (same as gindex) ==========

type StackEntry struct {
	Timestamp int64  `json:"t"`
	State     string `json:"s"`
	Stack     string `json:"k"`
	CreatedBy int64  `json:"c,omitempty"`
}

type GoroutineTimeSeries struct {
	Entries []StackEntry `json:"e"`
}

// ========== API Handlers ==========

func handleHosts(w http.ResponseWriter, r *http.Request) {
	var hosts []string
	if val, closer, err := db.Get([]byte("m:hosts")); err == nil {
		json.Unmarshal(val, &hosts)
		closer.Close()
	}
	writeJSON(w, hosts)
}

func handleGoroutine(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	goroID := r.URL.Query().Get("id")

	if host == "" || goroID == "" {
		http.Error(w, "host and id parameters required", http.StatusBadRequest)
		return
	}

	key := fmt.Sprintf("g:%s:%s", host, goroID)
	val, closer, err := db.Get([]byte(key))
	if err != nil {
		http.Error(w, "Goroutine not found", http.StatusNotFound)
		return
	}
	defer closer.Close()

	var series GoroutineTimeSeries
	if err := decompressJSON(val, &series); err != nil {
		http.Error(w, "Failed to decode data", http.StatusInternalServerError)
		return
	}

	writeJSON(w, series)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	// Get all hosts
	var hosts []string
	if val, closer, err := db.Get([]byte("m:hosts")); err == nil {
		json.Unmarshal(val, &hosts)
		closer.Close()
	}

	type HostStats struct {
		Host       string  `json:"host"`
		Timestamps []int64 `json:"timestamps"`
		Counts     []int   `json:"counts"`
	}

	var allStats []HostStats

	for _, host := range hosts {
		// Read pre-computed stats
		key := []byte("s:" + host)
		val, closer, err := db.Get(key)
		if err != nil {
			continue
		}

		var statsData struct {
			Timestamps []int64 `json:"t"`
			Counts     []int   `json:"c"`
		}
		if err := decompressJSON(val, &statsData); err != nil {
			closer.Close()
			continue
		}
		closer.Close()

		allStats = append(allStats, HostStats{
			Host:       host,
			Timestamps: statsData.Timestamps,
			Counts:     statsData.Counts,
		})
	}

	writeJSON(w, allStats)
}

func handleChildren(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	parentID := r.URL.Query().Get("id")

	if host == "" || parentID == "" {
		http.Error(w, "host and id parameters required", http.StatusBadRequest)
		return
	}

	// Read pre-computed children index
	key := fmt.Sprintf("c:%s:%s", host, parentID)
	val, closer, err := db.Get([]byte(key))
	if err != nil {
		// No children
		writeJSON(w, []struct{}{})
		return
	}
	defer closer.Close()

	// Decompress and return
	type ChildInfo struct {
		ID        int64  `json:"id"`
		Funcs     string `json:"funcs"`
		FirstSeen int64  `json:"first"`
		LastSeen  int64  `json:"last"`
	}

	var storedChildren []struct {
		ID        int64  `json:"i"`
		Funcs     string `json:"f"`
		FirstSeen int64  `json:"s"`
		LastSeen  int64  `json:"e"`
	}

	if err := decompressJSON(val, &storedChildren); err != nil {
		http.Error(w, "Failed to decode data", http.StatusInternalServerError)
		return
	}

	// Convert to API format
	children := make([]ChildInfo, len(storedChildren))
	for i, c := range storedChildren {
		children[i] = ChildInfo{
			ID:        c.ID,
			Funcs:     c.Funcs,
			FirstSeen: c.FirstSeen,
			LastSeen:  c.LastSeen,
		}
	}

	writeJSON(w, children)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	goroID := r.URL.Query().Get("id")

	if host == "" {
		http.Error(w, "host parameter required", http.StatusBadRequest)
		return
	}

	// Search for goroutines matching the ID prefix
	prefix := fmt.Sprintf("g:%s:", host)

	type match struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
		First int64  `json:"first"`
		Last  int64  `json:"last"`
	}
	var matches []match

	iter, _ := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte(prefix),
		UpperBound: []byte(prefix + "\xff"),
	})
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		key := string(iter.Key())
		id := strings.TrimPrefix(key, prefix)

		// If searching for specific ID, filter
		if goroID != "" && !strings.Contains(id, goroID) {
			continue
		}

		var series GoroutineTimeSeries
		if err := decompressJSON(iter.Value(), &series); err != nil {
			continue
		}

		if len(series.Entries) > 0 {
			matches = append(matches, match{
				ID:    id,
				Count: len(series.Entries),
				First: series.Entries[0].Timestamp,
				Last:  series.Entries[len(series.Entries)-1].Timestamp,
			})
		}

		// Limit results
		if len(matches) >= 100 {
			break
		}
	}

	writeJSON(w, matches)
}

// ========== HTML UI ==========

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Goroutine Timelapse</title>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns"></script>
    <script src="https://cdn.jsdelivr.net/npm/chartjs-plugin-annotation"></script>
    <style>
        * { box-sizing: border-box; }
        html, body { 
            height: 100%;
            margin: 0;
        }
        body { 
            font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace; 
            padding: 20px; 
            background: #1e1e1e; 
            color: #d4d4d4;
            display: flex;
            flex-direction: column;
        }
        .header { 
            display: flex; 
            gap: 10px; 
            align-items: center; 
            margin-bottom: 20px;
            flex-wrap: wrap;
        }
        select, input, button { 
            padding: 8px 12px; 
            font-size: 14px; 
            background: #3c3c3c;
            color: #d4d4d4;
            border: 1px solid #555;
            border-radius: 4px;
        }
        button { 
            cursor: pointer; 
            background: #0e639c;
            border-color: #0e639c;
        }
        button:hover { background: #1177bb; }
        button:disabled { opacity: 0.5; cursor: not-allowed; }
        .controls {
            display: flex;
            align-items: center;
            gap: 10px;
            margin-bottom: 10px;
            background: #252526;
            padding: 10px;
            border-radius: 4px;
        }
        .slider-container { flex: 1; }
        input[type="range"] { 
            width: 100%; 
            cursor: pointer;
        }
        .time-info {
            display: flex;
            justify-content: space-between;
            font-size: 12px;
            color: #888;
            margin-top: 5px;
        }
        .current-time {
            font-size: 16px;
            font-weight: bold;
            color: #4ec9b0;
            min-width: 200px;
        }
        .state {
            padding: 4px 8px;
            border-radius: 4px;
            font-size: 12px;
            background: #264f78;
        }
        .stack-container {
            background: #252526;
            border-radius: 4px;
            overflow: hidden;
        }
        .parent-link {
            margin-left: 10px;
            color: #569cd6;
            cursor: pointer;
            text-decoration: underline;
        }
        .parent-link:hover {
            color: #9cdcfe;
        }
        .stack-header {
            padding: 10px;
            background: #333;
            border-bottom: 1px solid #444;
            display: flex;
            justify-content: space-between;
            align-items: center;
        }
        .stack {
            padding: 15px;
            overflow-x: auto;
            white-space: pre;
            font-size: 13px;
            line-height: 1.5;
            max-height: 70vh;
            overflow-y: auto;
        }
        .stack-line { display: block; }
        .stack-line:hover { background: #333; }
        .stack-line.func { color: #dcdcaa; }
        .stack-line.file { color: #808080; }
        .stack-line.changed { background: #3d2b2b; }
        .stack-line.new { background: #2b3d2b; }
        .search-results {
            max-height: 300px;
            overflow-y: auto;
            background: #252526;
            border-radius: 4px;
            margin-bottom: 20px;
        }
        .search-result {
            padding: 8px 12px;
            cursor: pointer;
            border-bottom: 1px solid #333;
            display: flex;
            justify-content: space-between;
        }
        .search-result:hover { background: #333; }
        .search-result.selected { background: #0e639c; }
        .frame-counter {
            font-size: 14px;
            color: #888;
        }
        .playback-speed {
            display: flex;
            align-items: center;
            gap: 5px;
            font-size: 12px;
        }
        #loading {
            position: fixed;
            top: 50%;
            left: 50%;
            transform: translate(-50%, -50%);
            background: #333;
            padding: 20px;
            border-radius: 8px;
            display: none;
        }
        .chart-container {
            background: #252526;
            border-radius: 4px;
            padding: 15px;
            margin-bottom: 20px;
        }
        .chart-container h3 {
            margin: 0 0 15px 0;
            color: #4ec9b0;
            font-size: 16px;
        }
        .chart-wrapper {
            position: relative;
            height: 300px;
        }
        .tab-bar {
            display: flex;
            gap: 5px;
            margin-bottom: 20px;
            flex-shrink: 0;
        }
        .tab {
            padding: 8px 16px;
            background: #3c3c3c;
            border: none;
            color: #d4d4d4;
            cursor: pointer;
            border-radius: 4px 4px 0 0;
        }
        .tab.active {
            background: #0e639c;
        }
        .tab:hover:not(.active) {
            background: #4c4c4c;
        }
        .children-container {
            background: #252526;
            border-radius: 4px;
            margin-top: 15px;
            overflow: hidden;
            display: flex;
            flex-direction: column;
            flex: 1;
            min-height: 300px;
        }
        .children-header {
            padding: 10px;
            background: #333;
            border-bottom: 1px solid #444;
            display: flex;
            justify-content: space-between;
            align-items: center;
            cursor: pointer;
        }
        .children-header:hover {
            background: #3a3a3a;
        }
        .children-header h4 {
            margin: 0;
            color: #4ec9b0;
            font-size: 14px;
        }
        .children-toggle {
            color: #888;
            font-size: 12px;
        }
        .children-list {
            flex: 1;
            min-height: 250px;
            overflow-y: auto;
        }
        .child-item {
            padding: 8px 12px;
            border-bottom: 1px solid #333;
            display: grid;
            grid-template-columns: 80px 1fr 180px 100px;
            gap: 10px;
            align-items: center;
            font-size: 12px;
        }
        .child-item:hover {
            background: #333;
        }
        .child-id {
            color: #569cd6;
            cursor: pointer;
            text-decoration: underline;
        }
        .child-id:hover {
            color: #9cdcfe;
        }
        .child-funcs {
            color: #dcdcaa;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        .child-time {
            color: #888;
        }
        .child-duration {
            color: #4ec9b0;
            text-align: right;
        }
        .viewer-chart-container {
            background: #252526;
            border-radius: 4px;
            padding: 10px;
            margin-bottom: 10px;
        }
        .viewer-chart-wrapper {
            position: relative;
            height: 120px;
        }
        #chartTab {
            flex-shrink: 0;
        }
        #viewerTab {
            display: flex;
            flex-direction: column;
            flex: 1;
            min-height: 0;
        }
        #viewer {
            display: flex;
            flex-direction: column;
            flex: 1;
            min-height: 0;
        }
    </style>
</head>
<body>
    <div id="loading">Loading...</div>
    
    <div class="tab-bar">
        <button class="tab active" onclick="showTab('chart')">Overview</button>
        <button class="tab" onclick="showTab('viewer')">Goroutine Viewer</button>
    </div>

    <div id="chartTab">
        <div class="chart-container">
            <h3>Active Goroutines Over Time</h3>
            <div class="chart-wrapper">
                <canvas id="goroChart"></canvas>
            </div>
        </div>
    </div>

    <div id="viewerTab" style="display:none">
        <div class="header">
            <select id="hostSelect">
                <option value="">Select Host...</option>
            </select>
            <input type="text" id="goroSearch" placeholder="Goroutine ID..." style="width: 150px">
            <button onclick="searchGoroutines()">Search</button>
            <button onclick="loadGoroutine()">Load</button>
        </div>

        <div id="searchResults" class="search-results" style="display:none"></div>

    <div id="viewer" style="display:none">
        <div class="viewer-chart-container">
            <div class="viewer-chart-wrapper">
                <canvas id="viewerChart"></canvas>
            </div>
        </div>

        <div class="controls">
            <button id="playBtn" onclick="togglePlay()">▶ Play</button>
            <div class="slider-container">
                <input type="range" id="timeSlider" min="0" max="100" value="0" oninput="onSliderChange()">
                <div class="time-info">
                    <span id="startTime">--</span>
                    <span id="endTime">--</span>
                </div>
            </div>
            <div class="playback-speed">
                <label>Speed:</label>
                <select id="speedSelect">
                    <option value="2000">0.5x</option>
                    <option value="1000" selected>1x</option>
                    <option value="500">2x</option>
                    <option value="200">5x</option>
                    <option value="100">10x</option>
                </select>
            </div>
        </div>

        <div class="stack-container">
            <div class="stack-header">
                <div>
                    <span class="current-time" id="currentTime">--</span>
                    <span class="state" id="currentState">--</span>
                    <span id="parentLink"></span>
                </div>
                <span class="frame-counter" id="frameCounter">-- / --</span>
            </div>
            <div class="stack" id="stackView"></div>
        </div>

        <div class="children-container" id="childrenContainer" style="display:none">
            <div class="children-header" onclick="toggleChildren()">
                <h4>Children Goroutines (<span id="childrenCount">0</span>)</h4>
                <span class="children-toggle" id="childrenToggle">▼ Show</span>
            </div>
            <div class="children-list" id="childrenList" style="display:none"></div>
        </div>
    </div>
    </div>

    <script>
        let hosts = [];
        let currentData = null;
        let currentFrame = 0;
        let playing = false;
        let playInterval = null;
        let previousStack = '';
        let goroChart = null;
        let viewerChart = null;
        let statsData = null;
        let childrenData = null;
        let childrenVisible = false;

        // Tab switching
        function showTab(tab) {
            document.querySelectorAll('.tab').forEach(t => t.classList.remove('active'));
            document.querySelector('.tab[onclick="showTab(\'' + tab + '\')"]').classList.add('active');
            
            document.getElementById('chartTab').style.display = tab === 'chart' ? 'block' : 'none';
            document.getElementById('viewerTab').style.display = tab === 'viewer' ? 'flex' : 'none';
        }

        // Chart colors for different hosts
        const chartColors = [
            'rgb(78, 201, 176)',   // teal
            'rgb(86, 156, 214)',   // blue
            'rgb(220, 220, 170)',  // yellow
            'rgb(206, 145, 120)',  // orange
            'rgb(197, 134, 192)',  // purple
            'rgb(244, 135, 113)',  // red
            'rgb(129, 212, 250)',  // light blue
            'rgb(165, 214, 167)',  // light green
        ];

        // Fetch stats data (used by both charts)
        async function fetchStats() {
            if (statsData) return statsData;
            const resp = await fetch('/api/stats');
            statsData = await resp.json();
            return statsData;
        }

        // Load and render the chart
        async function loadChart() {
            showLoading(true);
            const stats = await fetchStats();
            showLoading(false);

            const datasets = stats.map((hostData, i) => {
                const data = hostData.timestamps.map((ts, j) => ({
                    x: new Date(ts * 1000),
                    y: hostData.counts[j]
                }));

                return {
                    label: hostData.host,
                    data: data,
                    borderColor: chartColors[i % chartColors.length],
                    backgroundColor: chartColors[i % chartColors.length].replace('rgb', 'rgba').replace(')', ', 0.1)'),
                    borderWidth: 1.5,
                    pointRadius: 0,
                    tension: 0.1,
                    fill: false
                };
            });

            const ctx = document.getElementById('goroChart').getContext('2d');
            
            if (goroChart) {
                goroChart.destroy();
            }

            goroChart = new Chart(ctx, {
                type: 'line',
                data: { datasets },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    interaction: {
                        mode: 'index',
                        intersect: false
                    },
                    plugins: {
                        legend: {
                            position: 'top',
                            labels: {
                                color: '#d4d4d4',
                                usePointStyle: true,
                                pointStyle: 'line'
                            }
                        },
                        tooltip: {
                            backgroundColor: '#333',
                            titleColor: '#d4d4d4',
                            bodyColor: '#d4d4d4',
                            borderColor: '#555',
                            borderWidth: 1
                        }
                    },
                    scales: {
                        x: {
                            type: 'time',
                            time: {
                                displayFormats: {
                                    minute: 'HH:mm',
                                    hour: 'HH:mm'
                                }
                            },
                            grid: {
                                color: '#333'
                            },
                            ticks: {
                                color: '#888'
                            }
                        },
                        y: {
                            beginAtZero: false,
                            grid: {
                                color: '#333'
                            },
                            ticks: {
                                color: '#888'
                            },
                            title: {
                                display: true,
                                text: 'Active Goroutines',
                                color: '#888'
                            }
                        }
                    }
                }
            });
        }

        // Initialize
        async function init() {
            const resp = await fetch('/api/hosts');
            hosts = await resp.json();
            const select = document.getElementById('hostSelect');
            hosts.forEach(h => {
                const opt = document.createElement('option');
                opt.value = h;
                opt.textContent = h;
                select.appendChild(opt);
            });

            // Load the chart
            loadChart();

            // Check URL params
            const params = new URLSearchParams(window.location.search);
            const host = params.get('host');
            const id = params.get('id');
            if (host && id) {
                document.getElementById('hostSelect').value = host;
                document.getElementById('goroSearch').value = id;
                showTab('viewer');
                loadGoroutine();
            }
        }

        async function searchGoroutines() {
            const host = document.getElementById('hostSelect').value;
            const id = document.getElementById('goroSearch').value;
            if (!host) {
                alert('Please select a host');
                return;
            }

            showLoading(true);
            const resp = await fetch('/api/search?host=' + encodeURIComponent(host) + '&id=' + encodeURIComponent(id));
            const results = await resp.json();
            showLoading(false);

            const container = document.getElementById('searchResults');
            container.innerHTML = '';
            container.style.display = results.length ? 'block' : 'none';

            results.forEach(r => {
                const div = document.createElement('div');
                div.className = 'search-result';
                div.innerHTML = '<span>Goroutine ' + r.id + '</span><span>' + r.count + ' frames, ' + formatDuration(r.last - r.first) + '</span>';
                div.onclick = () => {
                    document.getElementById('goroSearch').value = r.id;
                    loadGoroutine();
                };
                container.appendChild(div);
            });
        }

        async function loadGoroutine() {
            const host = document.getElementById('hostSelect').value;
            const id = document.getElementById('goroSearch').value;
            if (!host || !id) {
                alert('Please select a host and enter a goroutine ID');
                return;
            }

            showLoading(true);
            const resp = await fetch('/api/goroutine?host=' + encodeURIComponent(host) + '&id=' + encodeURIComponent(id));
            if (!resp.ok) {
                showLoading(false);
                alert('Goroutine not found');
                return;
            }

            currentData = await resp.json();
            showLoading(false);

            if (!currentData.e || currentData.e.length === 0) {
                alert('No data for this goroutine');
                return;
            }

            // Update URL
            history.replaceState(null, '', '?host=' + encodeURIComponent(host) + '&id=' + encodeURIComponent(id));

            // Setup viewer
            document.getElementById('searchResults').style.display = 'none';
            document.getElementById('viewer').style.display = 'flex';

            const slider = document.getElementById('timeSlider');
            slider.max = currentData.e.length - 1;
            slider.value = 0;

            document.getElementById('startTime').textContent = formatTime(currentData.e[0].t);
            document.getElementById('endTime').textContent = formatTime(currentData.e[currentData.e.length - 1].t);

            currentFrame = 0;
            previousStack = '';
            renderFrame();

            // Load children goroutines (also renders the mini chart)
            loadChildren(host, id);
        }

        async function loadChildren(host, id) {
            const resp = await fetch('/api/children?host=' + encodeURIComponent(host) + '&id=' + encodeURIComponent(id));
            childrenData = await resp.json();

            const container = document.getElementById('childrenContainer');
            const countSpan = document.getElementById('childrenCount');
            const list = document.getElementById('childrenList');

            if (!childrenData || childrenData.length === 0) {
                container.style.display = 'none';
                renderViewerChart(); // Hide the chart
                return;
            }

            container.style.display = 'flex';
            countSpan.textContent = childrenData.length;

            // Sort by first seen
            childrenData.sort((a, b) => a.first - b.first);

            // Render children list
            let html = '';
            childrenData.forEach(child => {
                const duration = formatDuration(child.last - child.first);
                const startTime = formatTime(child.first).substring(11); // Just time part
                const endTime = formatTime(child.last).substring(11);
                html += '<div class="child-item">' +
                    '<span class="child-id" onclick="goToChild(' + child.id + ')">#' + child.id + '</span>' +
                    '<span class="child-funcs" title="' + escapeHtml(child.funcs) + '">' + escapeHtml(child.funcs) + '</span>' +
                    '<span class="child-time">' + startTime + ' - ' + endTime + '</span>' +
                    '<span class="child-duration">' + duration + '</span>' +
                    '</div>';
            });
            list.innerHTML = html;

            // Reset visibility state
            childrenVisible = false;
            document.getElementById('childrenToggle').textContent = '▼ Show';
            list.style.display = 'none';

            // Render the mini chart showing active children over time
            renderViewerChart();
        }

        function toggleChildren() {
            childrenVisible = !childrenVisible;
            document.getElementById('childrenList').style.display = childrenVisible ? 'block' : 'none';
            document.getElementById('childrenToggle').textContent = childrenVisible ? '▲ Hide' : '▼ Show';
        }

        function goToChild(childId) {
            const host = document.getElementById('hostSelect').value;
            window.location.href = '?host=' + encodeURIComponent(host) + '&id=' + childId;
        }

        function renderFrame() {
            if (!currentData || !currentData.e) return;

            const entry = currentData.e[currentFrame];
            document.getElementById('currentTime').textContent = formatTime(entry.t);
            document.getElementById('currentState').textContent = entry.s;
            document.getElementById('frameCounter').textContent = (currentFrame + 1) + ' / ' + currentData.e.length;
            document.getElementById('timeSlider').value = currentFrame;

            // Show parent goroutine link
            const parentLink = document.getElementById('parentLink');
            if (entry.c && entry.c > 0) {
                const host = document.getElementById('hostSelect').value;
                parentLink.innerHTML = '<a class="parent-link" href="?host=' + encodeURIComponent(host) + '&id=' + entry.c + '">← parent goroutine ' + entry.c + '</a>';
            } else {
                parentLink.innerHTML = '';
            }

            // Render stack with diff highlighting
            // Reverse the stack so lowest function (root) is at top - this keeps the display stable
            const stackView = document.getElementById('stackView');
            const lines = entry.k.split('\n').reverse();
            const prevLines = previousStack.split('\n').reverse();

            let html = '';
            lines.forEach((line, i) => {
                let cls = 'stack-line';
                if (line.startsWith('\t') || line.startsWith('/') || line.includes('.go:')) {
                    cls += ' file';
                } else {
                    cls += ' func';
                }
                
                if (previousStack && i < prevLines.length && line !== prevLines[i]) {
                    cls += ' changed';
                } else if (previousStack && i >= prevLines.length) {
                    cls += ' new';
                }

                html += '<span class="' + cls + '">' + escapeHtml(line) + '</span>';
            });

            stackView.innerHTML = html;
            previousStack = entry.k;

            // Update chart marker
            updateViewerChartMarker();
        }

        function onSliderChange() {
            currentFrame = parseInt(document.getElementById('timeSlider').value);
            renderFrame();
        }

        function togglePlay() {
            playing = !playing;
            document.getElementById('playBtn').textContent = playing ? '⏸ Pause' : '▶ Play';

            if (playing) {
                const speed = parseInt(document.getElementById('speedSelect').value);
                playInterval = setInterval(() => {
                    if (currentFrame < currentData.e.length - 1) {
                        currentFrame++;
                        renderFrame();
                    } else {
                        togglePlay(); // Stop at end
                    }
                }, speed);
            } else {
                clearInterval(playInterval);
            }
        }

        function formatTime(ts) {
            const d = new Date(ts * 1000);
            return d.toISOString().replace('T', ' ').substring(0, 19);
        }

        function formatDuration(seconds) {
            if (seconds < 60) return seconds + 's';
            if (seconds < 3600) return Math.floor(seconds / 60) + 'm ' + (seconds % 60) + 's';
            return Math.floor(seconds / 3600) + 'h ' + Math.floor((seconds % 3600) / 60) + 'm';
        }

        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text;
            return div.innerHTML;
        }

        function showLoading(show) {
            document.getElementById('loading').style.display = show ? 'block' : 'none';
        }

        function renderViewerChart() {
            const chartContainer = document.getElementById('viewerChart').parentElement.parentElement;
            
            if (!currentData || !childrenData || childrenData.length === 0) {
                // Hide chart if no children
                chartContainer.style.display = 'none';
                if (viewerChart) {
                    viewerChart.destroy();
                    viewerChart = null;
                }
                return;
            }

            chartContainer.style.display = 'block';

            // Get all unique timestamps from the goroutine's entries
            const timestamps = currentData.e.map(e => e.t);

            // For each timestamp, count how many children were active
            const data = timestamps.map(ts => {
                let count = 0;
                for (const child of childrenData) {
                    if (ts >= child.first && ts <= child.last) {
                        count++;
                    }
                }
                return { x: new Date(ts * 1000), y: count };
            });

            // Get current timestamp for the marker
            const currentTs = currentData.e[currentFrame].t * 1000;

            const ctx = document.getElementById('viewerChart').getContext('2d');

            if (viewerChart) {
                viewerChart.destroy();
            }

            viewerChart = new Chart(ctx, {
                type: 'line',
                data: {
                    datasets: [{
                        label: 'Active Children',
                        data: data,
                        borderColor: 'rgb(78, 201, 176)',
                        backgroundColor: 'rgba(78, 201, 176, 0.1)',
                        borderWidth: 1.5,
                        pointRadius: 0,
                        tension: 0,
                        fill: true,
                        stepped: 'before'
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    animation: false,
                    plugins: {
                        legend: { display: false },
                        tooltip: { enabled: false },
                        annotation: {
                            annotations: {
                                currentLine: {
                                    type: 'line',
                                    xMin: currentTs,
                                    xMax: currentTs,
                                    borderColor: 'rgb(255, 99, 132)',
                                    borderWidth: 2,
                                    borderDash: [5, 5]
                                }
                            }
                        }
                    },
                    scales: {
                        x: {
                            type: 'time',
                            time: {
                                displayFormats: {
                                    minute: 'HH:mm',
                                    hour: 'HH:mm'
                                }
                            },
                            grid: { color: '#333' },
                            ticks: { color: '#888', maxTicksLimit: 8 }
                        },
                        y: {
                            beginAtZero: true,
                            grid: { color: '#333' },
                            ticks: { 
                                color: '#888',
                                stepSize: 1
                            },
                            title: {
                                display: true,
                                text: 'Active Children',
                                color: '#888',
                                font: { size: 10 }
                            }
                        }
                    }
                }
            });
        }

        function updateViewerChartMarker() {
            if (!viewerChart || !currentData) return;
            const currentTs = currentData.e[currentFrame].t * 1000;
            viewerChart.options.plugins.annotation.annotations.currentLine.xMin = currentTs;
            viewerChart.options.plugins.annotation.annotations.currentLine.xMax = currentTs;
            viewerChart.update('none');
        }

        // Keyboard controls
        document.addEventListener('keydown', (e) => {
            if (!currentData) return;
            if (e.key === 'ArrowLeft' && currentFrame > 0) {
                currentFrame--;
                renderFrame();
            } else if (e.key === 'ArrowRight' && currentFrame < currentData.e.length - 1) {
                currentFrame++;
                renderFrame();
            } else if (e.key === ' ') {
                e.preventDefault();
                togglePlay();
            }
        });

        init();
    </script>
</body>
</html>`)
}

// ========== Utilities ==========

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
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

type quietLogger struct{}

func (q *quietLogger) Infof(format string, args ...interface{})  {}
func (q *quietLogger) Errorf(format string, args ...interface{}) {}
func (q *quietLogger) Fatalf(format string, args ...interface{}) { log.Fatalf(format, args...) }

// Unused but needed for consistency
func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
