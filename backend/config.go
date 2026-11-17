package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Semua konfigurasi dibaca dari environment variable agar binary yang sama
// bisa dipakai di server (Termux) dan di mesin development.

const (
	sampleInterval  = 5 * time.Second  // interval collector metrik
	historyInterval = 60 * time.Second // interval penyimpanan ke SQLite
	liveCapacity    = 720              // 720 x 5 detik = 1 jam data live di memori
)

type Threshold struct {
	Warning  float64 `json:"warning"`
	Critical float64 `json:"critical"`
}

type Config struct {
	ListenAddr    string
	DBPath        string
	StaticDir     string
	DiskPath      string
	FSPaths       []string
	Services      []string
	ServiceDir    string
	Token         string
	PublicProcs   bool
	RetentionDays int
	Thresholds    map[string]Threshold
}

var cfg = loadConfig()

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envList(k, def string) []string {
	out := []string{}
	for _, s := range strings.Split(envOr(k, def), ",") {
		if s = strings.TrimSpace(s); s != "" && s != "none" {
			out = append(out, s)
		}
	}
	return out
}

func envThreshold(k string, warn, crit float64) Threshold {
	t := Threshold{warn, crit}
	parts := strings.Split(os.Getenv(k), ",")
	if len(parts) == 2 {
		w, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		c, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if e1 == nil && e2 == nil && w > 0 && c >= w {
			t = Threshold{w, c}
		}
	}
	return t
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func loadConfig() Config {
	defDisk := "/"
	if exists("/data") {
		defDisk = "/data"
	}
	defSvDir := os.Getenv("SVDIR")
	if defSvDir == "" {
		if prefix := os.Getenv("PREFIX"); prefix != "" {
			defSvDir = prefix + "/var/service"
		} else {
			defSvDir = "/data/data/com.termux/files/usr/var/service"
		}
	}
	retention, err := strconv.Atoi(envOr("RETENTION_DAYS", "7"))
	if err != nil || retention < 1 || retention > 90 {
		retention = 7
	}

	return Config{
		ListenAddr:    envOr("LISTEN_ADDR", "127.0.0.1:8766"),
		DBPath:        envOr("DB_PATH", "/data/data/com.termux/files/home/homelab/database/homelab.db"),
		StaticDir:     os.Getenv("STATIC_DIR"),
		DiskPath:      envOr("DISK_PATH", defDisk),
		FSPaths:       envList("FS_PATHS", "/data,/system,/storage/emulated,/"),
		Services:      envList("SERVICES", "nginx,sshd,cloudflared"),
		ServiceDir:    envOr("SV_DIR", defSvDir),
		Token:         os.Getenv("MONITOR_TOKEN"),
		PublicProcs:   os.Getenv("ALLOW_PUBLIC_PROCESSES") == "1",
		RetentionDays: retention,
		Thresholds: map[string]Threshold{
			"cpu":          envThreshold("THRESH_CPU", 85, 95),
			"ram":          envThreshold("THRESH_RAM", 85, 95),
			"disk":         envThreshold("THRESH_DISK", 80, 90),
			"temp":         envThreshold("THRESH_TEMP", 60, 70),
			"battery_temp": envThreshold("THRESH_BATTERY_TEMP", 42, 47),
		},
	}
}
