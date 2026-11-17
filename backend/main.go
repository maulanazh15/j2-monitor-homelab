package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	listenAddr = envOr("LISTEN_ADDR", "127.0.0.1:8766")
	dbPath     = envOr("DB_PATH", "/data/data/com.termux/files/home/homelab/database/homelab.db")
	staticDir  = os.Getenv("STATIC_DIR") // dev mode: serve frontend if set
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type Memory struct {
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Free        uint64  `json:"free"`
	Percent     float64 `json:"percent"`
	SwapTotal   uint64  `json:"swap_total"`
	SwapUsed    uint64  `json:"swap_used"`
	SwapFree    uint64  `json:"swap_free"`
	SwapPercent float64 `json:"swap_percent"`
}

type Disk struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	Percent float64 `json:"percent"`
}

type Status struct {
	Hostname     string          `json:"hostname"`
	Architecture string          `json:"architecture"`
	CPUCores     int             `json:"cpu_cores"`
	CPUPercent   float64         `json:"cpu_percent"`
	Load         []string        `json:"load"`
	Uptime       uint64          `json:"uptime"`
	Memory       Memory          `json:"memory"`
	Disk         Disk            `json:"disk"`
	Services     map[string]bool `json:"services"`
	Time         int64           `json:"time"`
}

type Point struct {
	Time int64   `json:"time"`
	CPU  float64 `json:"cpu"`
	RAM  float64 `json:"ram"`
	Swap float64 `json:"swap"`
	Disk float64 `json:"disk"`
}

type History struct {
	Hours  int     `json:"hours"`
	Points []Point `json:"points"`
}

var (
	mu      sync.RWMutex
	current Status
	history History
)

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func readMem() Memory {
	var m Memory
	values := make(map[string]uint64)

	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			v, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				values[strings.TrimSuffix(fields[0], ":")] = v * 1024
			}
		}
	}

	m.Total = values["MemTotal"]
	m.Free = values["MemAvailable"]
	if m.Free == 0 {
		m.Free = values["MemFree"]
	}
	if m.Total >= m.Free {
		m.Used = m.Total - m.Free
	}
	if m.Total > 0 {
		m.Percent = float64(m.Used) * 100 / float64(m.Total)
	}

	m.SwapTotal = values["SwapTotal"]
	m.SwapFree = values["SwapFree"]
	if m.SwapTotal >= m.SwapFree {
		m.SwapUsed = m.SwapTotal - m.SwapFree
	}
	if m.SwapTotal > 0 {
		m.SwapPercent = float64(m.SwapUsed) * 100 / float64(m.SwapTotal)
	}
	return m
}

func readDisk() Disk {
	var d Disk
	var st syscall.Statfs_t
	if syscall.Statfs("/data", &st) != nil {
		return d
	}

	d.Total = st.Blocks * uint64(st.Bsize)
	d.Free = st.Bavail * uint64(st.Bsize)
	if d.Total >= d.Free {
		d.Used = d.Total - d.Free
	}
	if d.Total > 0 {
		d.Percent = float64(d.Used) * 100 / float64(d.Total)
	}
	return d
}

func readCPU() float64 {
	fields := strings.Fields(readFile("/proc/stat"))
	if len(fields) < 8 || fields[0] != "cpu" {
		return 0
	}

	// user, nice, system, idle, iowait, irq, softirq, steal
	// guest dan guest_nice tidak dijumlahkan karena sudah
	// termasuk dalam user dan nice.
	var total, idle uint64

	for i, f := range fields[1:] {
		if i >= 8 {
			break
		}

		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}

		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}

	cpuMu.Lock()
	defer cpuMu.Unlock()

	if cpuReady && total > cpuTotal && idle >= cpuIdle {
		dt := total - cpuTotal
		di := idle - cpuIdle

		if dt > 0 && di <= dt {
			cpuPercent = float64(dt-di) * 100 / float64(dt)
		}
	}

	cpuTotal = total
	cpuIdle = idle
	cpuReady = true

	return cpuPercent
}

var (
	cpuMu      sync.Mutex
	cpuTotal   uint64
	cpuIdle    uint64
	cpuReady   bool
	cpuPercent float64
)

func readLoad() []string {
	fields := strings.Fields(readFile("/proc/loadavg"))
	if len(fields) < 3 {
		return []string{"0", "0", "0"}
	}
	return fields[:3]
}

func readUptime() uint64 {
	fields := strings.Fields(readFile("/proc/uptime"))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return uint64(v)
}

func serviceRunning(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sv", "status", name).CombinedOutput()
	return err == nil && strings.HasPrefix(string(out), "run:")
}

func collect() Status {
	host, _ := os.Hostname()
	return Status{
		Hostname:     host,
		Architecture: runtime.GOARCH,
		CPUCores:     runtime.NumCPU(),
		CPUPercent:   readCPU(),
		Load:         readLoad(),
		Uptime:       readUptime(),
		Memory:       readMem(),
		Disk:         readDisk(),
		Services: map[string]bool{
			"nginx":       serviceRunning("nginx"),
			"sshd":        serviceRunning("sshd"),
			"cloudflared": serviceRunning("cloudflared"),
		},
		Time: time.Now().Unix(),
	}
}

func refreshHistory(hours int) {
	if hours < 1 {
		hours = 1
	}
	if hours > 168 {
		hours = 168
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := fmt.Sprintf(
		"SELECT timestamp,cpu,ram,swap,disk FROM monitor_history WHERE timestamp >= strftime('%%s','now') - %d ORDER BY timestamp;",
		hours*3600,
	)

	out, err := exec.CommandContext(ctx, "sqlite3", "-separator", "|", dbPath, query).Output()
	if err != nil {
		return
	}

	points := make([]Point, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 5 {
			continue
		}
		t, e1 := strconv.ParseInt(f[0], 10, 64)
		c, e2 := strconv.ParseFloat(f[1], 64)
		r, e3 := strconv.ParseFloat(f[2], 64)
		s, e4 := strconv.ParseFloat(f[3], 64)
		d, e5 := strconv.ParseFloat(f[4], 64)
		if e1 == nil && e2 == nil && e3 == nil && e4 == nil && e5 == nil {
			points = append(points, Point{t, c, r, s, d})
		}
	}

	mu.Lock()
	history = History{Hours: hours, Points: points}
	mu.Unlock()
}

func collector() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	for {
		s := collect()
		mu.Lock()
		current = s
		mu.Unlock()
		<-t.C
	}
}

func saveHistory(s Status) error {
	if s.Time <= 0 {
		s.Time = time.Now().Unix()
	}

	sql := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS monitor_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp INTEGER NOT NULL,
    cpu REAL,
    ram REAL,
    swap REAL,
    disk REAL
);
BEGIN IMMEDIATE;
INSERT INTO monitor_history
(timestamp, cpu, ram, swap, disk)
VALUES (%d, %s, %s, %s, %s);
DELETE FROM monitor_history
WHERE timestamp < %d;
COMMIT;
`,
		s.Time,
		strconv.FormatFloat(s.CPUPercent, 'f', 4, 64),
		strconv.FormatFloat(s.Memory.Percent, 'f', 4, 64),
		strconv.FormatFloat(s.Memory.SwapPercent, 'f', 4, 64),
		strconv.FormatFloat(s.Disk.Percent, 'f', 4, 64),
		time.Now().Unix()-7*86400,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(
		ctx,
		"sqlite3",
		"-batch",
		"-cmd", ".timeout 5000",
		"-cmd", ".bail on",
		dbPath,
		sql,
	).CombinedOutput()

	if err != nil {
		return fmt.Errorf("sqlite write failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

func historyWriter() {
	saveCurrent := func() {
		mu.RLock()
		s := current
		mu.RUnlock()

		if err := saveHistory(s); err != nil {
			log.Println("History writer:", err)
			return
		}
		log.Println("History sample saved")
	}

	// Simpan sampel pertama saat service mulai.
	saveCurrent()

	t := time.NewTicker(60 * time.Second)
	defer t.Stop()

	for range t.C {
		saveCurrent()
	}
}

func historyCollector() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		refreshHistory(168)
		<-t.C
	}
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func jsonResponse(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func main() {
	current = collect()
	refreshHistory(168)
	go collector()
	go historyCollector()
	go historyWriter()

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]interface{}{
			"ok": true, "service": "go-monitor", "time": time.Now().Unix(),
		})
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		s := current
		mu.RUnlock()

		s.CPUPercent = round1(s.CPUPercent)
		s.Memory.Percent = round1(s.Memory.Percent)
		s.Memory.SwapPercent = round1(s.Memory.SwapPercent)
		s.Disk.Percent = round1(s.Disk.Percent)

		jsonResponse(w, s)
	})

	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		if hours == 0 {
			hours = 1
		}
		if hours < 1 {
			hours = 1
		}
		if hours > 168 {
			hours = 168
		}

		mu.RLock()
		h := history
		mu.RUnlock()

		cutoff := time.Now().Unix() - int64(hours*3600)
		points := make([]Point, 0)
		for _, p := range h.Points {
			if p.Time >= cutoff {
				p.CPU = round1(p.CPU)
				p.RAM = round1(p.RAM)
				p.Swap = round1(p.Swap)
				p.Disk = round1(p.Disk)
				points = append(points, p)
			}
		}
		jsonResponse(w, History{Hours: hours, Points: points})
	})

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]string{
			"service": "go-monitor", "version": "1.0.0",
			"go": runtime.Version(), "arch": runtime.GOARCH,
		})
	})

	if staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(staticDir)))
		fmt.Println("Serving static files from", staticDir)
	}

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	fmt.Println("Go Monitor listening on", listenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Keep net imported to make the listener binding explicit at startup.
var _ = net.ParseIP
