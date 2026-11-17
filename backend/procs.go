package main

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Daftar proses dibaca langsung dari /proc. Hanya nama (comm) yang dikirim,
// bukan command line, karena argumen bisa berisi token/secret.
// Pada Android, aplikasi hanya bisa melihat proses milik UID-nya sendiri.

type Proc struct {
	PID     int      `json:"pid"`
	Name    string   `json:"name"`
	State   string   `json:"state"`
	CPU     *float64 `json:"cpu"` // % dari satu core
	RSS     uint64   `json:"rss"` // byte
	Mem     float64  `json:"mem"` // % dari total RAM
	Threads int      `json:"threads"`
}

type procSample struct {
	ticks uint64
	proc  Proc
}

const clkTck = 100 // USER_HZ standar Linux/Android

var (
	procMu     sync.Mutex
	procPrev   map[int]uint64
	procPrevAt time.Time
)

func parseProcStat(s string) (Proc, uint64, bool) {
	l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if l < 0 || r < l || r+2 > len(s) {
		return Proc{}, 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s[:l]))
	if err != nil {
		return Proc{}, 0, false
	}
	f := strings.Fields(s[r+2:]) // f[0] = field ke-3 (state)
	if len(f) < 22 {
		return Proc{}, 0, false
	}
	u := func(k int) uint64 { v, _ := strconv.ParseUint(f[k], 10, 64); return v }
	threads, _ := strconv.Atoi(f[17])
	p := Proc{PID: pid, Name: s[l+1 : r], State: f[0], Threads: threads, RSS: u(21) * uint64(os.Getpagesize())}
	return p, u(11) + u(12), true
}

func sampleProcs() map[int]procSample {
	out := map[int]procSample{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, ticks, ok := parseProcStat(readFile("/proc/" + e.Name() + "/stat")); ok && p.PID == pid {
			out[pid] = procSample{ticks, p}
		}
	}
	return out
}

func listProcesses(memTotal uint64) []Proc {
	procMu.Lock()
	defer procMu.Unlock()

	if procPrev == nil || time.Since(procPrevAt) > 30*time.Second {
		// tidak ada sampel sebelumnya yang relevan: ambil baseline singkat
		base := sampleProcs()
		procPrev = map[int]uint64{}
		for pid, s := range base {
			procPrev[pid] = s.ticks
		}
		procPrevAt = time.Now()
		time.Sleep(700 * time.Millisecond)
	}
	cur := sampleProcs()
	now := time.Now()
	dt := now.Sub(procPrevAt).Seconds()

	list := make([]Proc, 0, len(cur))
	next := make(map[int]uint64, len(cur))
	for pid, s := range cur {
		p := s.proc
		if prev, ok := procPrev[pid]; ok && dt > 0 && s.ticks >= prev {
			v := float64(s.ticks-prev) / clkTck / dt * 100
			p.CPU = fptr(v)
		}
		p.Mem = round1(pct(p.RSS, memTotal))
		next[pid] = s.ticks
		list = append(list, p)
	}
	procPrev, procPrevAt = next, now

	sort.Slice(list, func(i, j int) bool {
		ci, cj := 0.0, 0.0
		if list[i].CPU != nil {
			ci = *list[i].CPU
		}
		if list[j].CPU != nil {
			cj = *list[j].CPU
		}
		if ci != cj {
			return ci > cj
		}
		return list[i].RSS > list[j].RSS
	})
	if len(list) > 300 {
		list = list[:300]
	}
	return list
}
