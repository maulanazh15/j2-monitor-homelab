package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Penyimpanan memakai CLI sqlite3 (sama seperti v1) agar binary tetap
// tanpa cgo dan tanpa dependency tambahan. Semaphore membatasi agar hanya
// satu proses sqlite3 berjalan bersamaan di HP.

var dbSem = make(chan struct{}, 1)

func sqlite(timeout time.Duration, jsonOut bool, sql string) ([]byte, error) {
	dbSem <- struct{}{}
	defer func() { <-dbSem }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"-batch", "-cmd", ".timeout 5000", "-cmd", ".bail on"}
	if jsonOut {
		args = append(args, "-json")
	}
	args = append(args, cfg.DBPath, sql)
	out, err := exec.CommandContext(ctx, "sqlite3", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func sqlFloat(v *float64) string {
	if v == nil {
		return "NULL"
	}
	return strconv.FormatFloat(*v, 'f', 4, 64)
}

func sqlText(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func sqlInt(v *int64) string {
	if v == nil {
		return "NULL"
	}
	return strconv.FormatInt(*v, 10)
}

// migrate hanya menambah tabel/kolom/index (tidak menghapus apa pun),
// sehingga binary v1 tetap bisa memakai database yang sama (rollback aman).
func migrate() error {
	_, err := sqlite(15*time.Second, false, `
CREATE TABLE IF NOT EXISTS monitor_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp INTEGER NOT NULL,
    cpu REAL, ram REAL, swap REAL, disk REAL
);
CREATE INDEX IF NOT EXISTS idx_monitor_history_ts ON monitor_history(timestamp);
CREATE TABLE IF NOT EXISTS monitor_events (
    id INTEGER PRIMARY KEY,
    key TEXT NOT NULL,
    severity TEXT NOT NULL,
    message TEXT NOT NULL,
    value REAL,
    started_at INTEGER NOT NULL,
    resolved_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_monitor_events_started ON monitor_events(started_at);`)
	if err != nil {
		return err
	}
	out, err := sqlite(5*time.Second, true, "PRAGMA table_info(monitor_history);")
	if err != nil {
		return err
	}
	var cols []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(out, &cols)
	have := map[string]bool{}
	for _, c := range cols {
		have[c.Name] = true
	}
	var alter strings.Builder
	for _, c := range []string{"net_rx", "net_tx", "disk_read", "disk_write", "temp", "cpu_max"} {
		if !have[c] {
			fmt.Fprintf(&alter, "ALTER TABLE monitor_history ADD COLUMN %s REAL;\n", c)
		}
	}
	if alter.Len() > 0 {
		_, err = sqlite(15*time.Second, false, alter.String())
	}
	return err
}

// ---------- history ----------

type Point struct {
	Time      int64    `json:"time"`
	CPU       float64  `json:"cpu"`
	RAM       float64  `json:"ram"`
	Swap      float64  `json:"swap"`
	Disk      float64  `json:"disk"`
	CPUMax    *float64 `json:"cpu_max,omitempty"`
	NetRx     *float64 `json:"net_rx,omitempty"`
	NetTx     *float64 `json:"net_tx,omitempty"`
	DiskRead  *float64 `json:"disk_read,omitempty"`
	DiskWrite *float64 `json:"disk_write,omitempty"`
	Temp      *float64 `json:"temp,omitempty"`
}

type History struct {
	Hours  int     `json:"hours"`
	Step   int     `json:"step"` // detik per titik; 60 = data mentah
	Points []Point `json:"points"`
}

func avgPtr(ps []LivePoint, get func(LivePoint) *float64) *float64 {
	sum, n := 0.0, 0
	for _, p := range ps {
		if v := get(p); v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return nil
	}
	r := sum / float64(n)
	return &r
}

// summarizeMinute merangkum sampel 5 detik menjadi satu baris history:
// rata-rata untuk metrik yang fluktuatif, nilai terakhir untuk RAM/swap/disk.
func summarizeMinute(ps []LivePoint) Point {
	last := ps[len(ps)-1]
	p := Point{Time: last.Time, RAM: last.RAM, Swap: last.Swap, Disk: last.Disk}
	mx := 0.0
	for _, x := range ps {
		p.CPU += x.CPU
		if x.CPU > mx {
			mx = x.CPU
		}
	}
	p.CPU /= float64(len(ps))
	p.CPUMax = &mx
	p.NetRx = avgPtr(ps, func(l LivePoint) *float64 { return l.NetRx })
	p.NetTx = avgPtr(ps, func(l LivePoint) *float64 { return l.NetTx })
	p.DiskRead = avgPtr(ps, func(l LivePoint) *float64 { return l.DiskRead })
	p.DiskWrite = avgPtr(ps, func(l LivePoint) *float64 { return l.DiskWrite })
	p.Temp = avgPtr(ps, func(l LivePoint) *float64 { return l.Temp })
	return p
}

func saveHistory(p Point) error {
	now := time.Now().Unix()
	sql := fmt.Sprintf(`BEGIN IMMEDIATE;
INSERT INTO monitor_history (timestamp,cpu,ram,swap,disk,net_rx,net_tx,disk_read,disk_write,temp,cpu_max)
VALUES (%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s);
DELETE FROM monitor_history WHERE timestamp < %d;
DELETE FROM monitor_events WHERE started_at < %d;
COMMIT;`,
		p.Time, sqlFloat(&p.CPU), sqlFloat(&p.RAM), sqlFloat(&p.Swap), sqlFloat(&p.Disk),
		sqlFloat(p.NetRx), sqlFloat(p.NetTx), sqlFloat(p.DiskRead), sqlFloat(p.DiskWrite), sqlFloat(p.Temp), sqlFloat(p.CPUMax),
		now-int64(cfg.RetentionDays)*86400, now-30*86400)
	_, err := sqlite(10*time.Second, false, sql)
	return err
}

// historyStep menentukan ukuran bucket (detik) agar jumlah titik <= maxPoints.
// maxPoints <= 0 berarti data mentah (perilaku v1).
func historyStep(hours, maxPoints int) int {
	if maxPoints <= 0 {
		return 0
	}
	step := (hours*3600 + maxPoints - 1) / maxPoints
	if step <= 60 {
		return 0
	}
	// bulatkan ke kelipatan 60 detik agar bucket stabil
	return ((step + 59) / 60) * 60
}

func queryHistory(hours, maxPoints int) (History, error) {
	step := historyStep(hours, maxPoints)
	cutoff := time.Now().Unix() - int64(hours)*3600
	var sql string
	if step == 0 {
		sql = fmt.Sprintf(`SELECT timestamp AS time, cpu, ram, swap, disk, cpu_max, net_rx, net_tx, disk_read, disk_write, temp
FROM monitor_history WHERE timestamp >= %d ORDER BY timestamp;`, cutoff)
	} else {
		sql = fmt.Sprintf(`SELECT (timestamp/%[1]d)*%[1]d AS time, avg(cpu) AS cpu, avg(ram) AS ram, avg(swap) AS swap, avg(disk) AS disk,
max(coalesce(cpu_max,cpu)) AS cpu_max, avg(net_rx) AS net_rx, avg(net_tx) AS net_tx,
avg(disk_read) AS disk_read, avg(disk_write) AS disk_write, avg(temp) AS temp
FROM monitor_history WHERE timestamp >= %[2]d GROUP BY 1 ORDER BY 1;`, step, cutoff)
	}
	out, err := sqlite(8*time.Second, true, sql)
	if err != nil {
		return History{}, err
	}
	var rows []struct {
		Time                 int64
		CPU, RAM, Swap, Disk *float64
		CPUMax               *float64 `json:"cpu_max"`
		NetRx                *float64 `json:"net_rx"`
		NetTx                *float64 `json:"net_tx"`
		DiskRead             *float64 `json:"disk_read"`
		DiskWrite            *float64 `json:"disk_write"`
		Temp                 *float64
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		if err := json.Unmarshal([]byte(s), &rows); err != nil {
			return History{}, err
		}
	}
	r1 := func(v *float64) *float64 {
		if v == nil {
			return nil
		}
		return fptr(*v)
	}
	z := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return round1(*v)
	}
	h := History{Hours: hours, Step: step, Points: make([]Point, 0, len(rows))}
	if step == 0 {
		h.Step = int(historyInterval / time.Second)
	}
	for _, r := range rows {
		h.Points = append(h.Points, Point{
			Time: r.Time, CPU: z(r.CPU), RAM: z(r.RAM), Swap: z(r.Swap), Disk: z(r.Disk),
			CPUMax: r1(r.CPUMax), NetRx: r1(r.NetRx), NetTx: r1(r.NetTx),
			DiskRead: r1(r.DiskRead), DiskWrite: r1(r.DiskWrite), Temp: r1(r.Temp),
		})
	}
	return h, nil
}

// Cache singkat agar banyak tab/viewer tidak memicu banyak proses sqlite3.
type histCache struct {
	mu      sync.Mutex
	entries map[string]histEntry
}

type histEntry struct {
	at time.Time
	h  History
}

var hcache = &histCache{entries: map[string]histEntry{}}

func (c *histCache) get(hours, points int) (History, error) {
	key := fmt.Sprintf("%d/%d", hours, points)
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && time.Since(e.at) < 30*time.Second {
		c.mu.Unlock()
		return e.h, nil
	}
	c.mu.Unlock()
	h, err := queryHistory(hours, points)
	if err != nil {
		return h, err
	}
	c.mu.Lock()
	if len(c.entries) > 32 {
		c.entries = map[string]histEntry{}
	}
	c.entries[key] = histEntry{time.Now(), h}
	c.mu.Unlock()
	return h, nil
}

// ---------- events ----------

func saveEvent(e Event) error {
	_, err := sqlite(10*time.Second, false, fmt.Sprintf(
		`INSERT OR REPLACE INTO monitor_events (id,key,severity,message,value,started_at,resolved_at) VALUES (%d,%s,%s,%s,%s,%d,%s);`,
		e.ID, sqlText(e.Key), sqlText(e.Severity), sqlText(e.Message), sqlFloat(e.Value), e.StartedAt, sqlInt(e.ResolvedAt)))
	return err
}

func loadEvents(limit int) ([]Event, error) {
	out, err := sqlite(8*time.Second, true, fmt.Sprintf(
		`SELECT id,key,severity,message,value,started_at,resolved_at FROM monitor_events ORDER BY started_at DESC, id DESC LIMIT %d;`, limit))
	if err != nil {
		return nil, err
	}
	var evs []Event
	if s := strings.TrimSpace(string(out)); s != "" {
		if err := json.Unmarshal([]byte(s), &evs); err != nil {
			return nil, err
		}
	}
	return evs, nil
}
