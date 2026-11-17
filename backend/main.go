package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "2.0.0"

var (
	mu      sync.RWMutex
	current Status

	smp    = &sampler{}
	live   = newLiveRing(liveCapacity)
	alerts *alertEngine
)

func collector() {
	t := time.NewTicker(sampleInterval)
	defer t.Stop()
	for {
		s := smp.collect()
		alerts.evaluate(&s)
		mu.Lock()
		current = s
		mu.Unlock()
		live.add(toLivePoint(s))
		<-t.C
	}
}

func historyWriter() {
	t := time.NewTicker(historyInterval)
	defer t.Stop()
	for range t.C {
		ps := live.since(time.Now().Unix() - int64(historyInterval/time.Second))
		if len(ps) == 0 {
			continue
		}
		if err := saveHistory(summarizeMinute(ps)); err != nil {
			log.Println("History writer:", err)
		}
	}
}

// ---------- HTTP helpers ----------

func jsonResponse(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// intParam membaca parameter integer dan membatasinya ke [lo, hi].
func intParam(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		v = def
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// procAccess: "token" (butuh token), "public", atau "disabled".
func procAccess() string {
	switch {
	case cfg.Token != "":
		return "token"
	case cfg.PublicProcs:
		return "public"
	}
	return "disabled"
}

func authorized(r *http.Request) bool {
	switch procAccess() {
	case "public":
		return true
	case "token":
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		return subtle.ConstantTimeCompare([]byte(tok), []byte(cfg.Token)) == 1
	}
	return false
}

// ---------- handlers ----------

func handleStatus(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	s := current
	mu.RUnlock()
	s.CPUPercent = round1(s.CPUPercent)
	s.Memory.Percent = round1(s.Memory.Percent)
	s.Memory.SwapPercent = round1(s.Memory.SwapPercent)
	s.Disk.Percent = round1(s.Disk.Percent)
	s.Health = alerts.health()
	jsonResponse(w, s)
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	hours := intParam(r, "hours", 1, 1, 168)
	points := 0 // 0 = data mentah (kompatibel v1)
	if r.URL.Query().Get("points") != "" {
		points = intParam(r, "points", 360, 30, 2000)
	}
	h, err := hcache.get(hours, points)
	if err != nil {
		log.Println("history:", err)
		jsonResponse(w, History{Hours: hours, Points: []Point{}})
		return
	}
	jsonResponse(w, h)
}

func handleLive(w http.ResponseWriter, r *http.Request) {
	minutes := intParam(r, "minutes", 15, 1, 60)
	since := time.Now().Unix() - int64(minutes*60)
	if s, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && s > since {
		since = s
	}
	jsonResponse(w, map[string]interface{}{
		"interval": int(sampleInterval / time.Second),
		"points":   live.since(since),
	})
}

func handleAlerts(w http.ResponseWriter, r *http.Request) {
	active, events := alerts.snapshot(intParam(r, "limit", 100, 1, maxEvents))
	jsonResponse(w, map[string]interface{}{"active": active, "events": events})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, map[string]interface{}{
		"version":          version,
		"interval":         int(sampleInterval / time.Second),
		"history_interval": int(historyInterval / time.Second),
		"live_window":      liveCapacity * int(sampleInterval/time.Second),
		"retention_days":   cfg.RetentionDays,
		"thresholds":       cfg.Thresholds,
		"services":         cfg.Services,
		"features": map[string]interface{}{
			"processes":  procAccess(),
			"containers": false,
		},
	})
}

func handleProcesses(w http.ResponseWriter, r *http.Request) {
	switch {
	case procAccess() == "disabled":
		jsonError(w, http.StatusForbidden, "process monitoring disabled")
		return
	case !authorized(r):
		w.Header().Set("WWW-Authenticate", "Bearer")
		jsonError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	mu.RLock()
	total := current.Memory.Total
	mu.RUnlock()
	list := listProcesses(total)
	jsonResponse(w, map[string]interface{}{
		"visible":   len(list),
		"limited":   runtime.GOOS == "android" || os.Getenv("ANDROID_ROOT") != "",
		"processes": list,
	})
}

func main() {
	if err := migrate(); err != nil {
		log.Println("migrate:", err)
	}
	alerts = newAlertEngine()
	if evs, err := loadEvents(maxEvents); err == nil {
		alerts.load(evs)
	} else {
		log.Println("load events:", err)
	}
	alerts.info("monitor", "Monitor dimulai (v"+version+")")

	smp.static = detectStatic()
	current = smp.collect()
	live.add(toLivePoint(current))
	go func() {
		time.Sleep(sampleInterval)
		collector()
	}()
	go historyWriter()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]interface{}{"ok": true, "service": "go-monitor", "time": time.Now().Unix()})
	})
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/history", handleHistory)
	mux.HandleFunc("/api/live", handleLive)
	mux.HandleFunc("/api/alerts", handleAlerts)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/processes", handleProcesses)
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]string{
			"service": "go-monitor", "version": version,
			"go": runtime.Version(), "arch": runtime.GOARCH,
		})
	})
	if cfg.StaticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(cfg.StaticDir)))
		fmt.Println("Serving static files from", cfg.StaticDir)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           secure(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	fmt.Println("Go Monitor", version, "listening on", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
