package main

import (
	"strings"
	"testing"
)

const sampleStat = `cpu  2431864 657914 1496360 107395017 111896 1200 179715 0 0 0
cpu0 722520 184809 569198 26357085 45537 887 138625 0 0 0
cpu2 513156 155208 288253 27094774 19071 80 10540 0 0 0
intr 82825952 0 0
btime 1794641905
`

func TestParseCPUStat(t *testing.T) {
	agg, cores, btime := parseCPUStat(sampleStat)
	if btime != 1794641905 {
		t.Fatalf("btime = %d", btime)
	}
	if len(cores) != 2 || cores[2].total == 0 {
		t.Fatalf("core hotplug tidak terbaca: %+v", cores)
	}
	if _, ok := cores[1]; ok {
		t.Fatal("cpu1 offline seharusnya tidak ada")
	}
	if agg.idle != 107395017+111896 {
		t.Fatalf("idle = %d", agg.idle)
	}
}

func TestCPUUsage(t *testing.T) {
	v, ok := cpuUsage(cpuTimes{1000, 800}, cpuTimes{1100, 850})
	if !ok || v != 50 {
		t.Fatalf("got %v %v", v, ok)
	}
	if _, ok := cpuUsage(cpuTimes{1000, 800}, cpuTimes{900, 700}); ok {
		t.Fatal("counter mundur harus ditolak")
	}
}

func TestParseMeminfo(t *testing.T) {
	m := parseMeminfo("MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 400 kB\nBuffers: 30 kB\nCached: 200 kB\nSwapTotal: 500 kB\nSwapFree: 400 kB\n")
	if m.Total != 1000*1024 || m.Used != 600*1024 || m.Percent != 60 || m.Free != m.Available {
		t.Fatalf("mem = %+v", m)
	}
	if m.SwapUsed != 100*1024 || m.SwapPercent != 20 || m.Cached != 200*1024 || m.Buffers != 30*1024 {
		t.Fatalf("swap/cache = %+v", m)
	}
}

func TestParseNetDev(t *testing.T) {
	s := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
rmnet0: 206276712 2154342    1    2    0     0          0         0 186180919 2101163    3    4    0     0       0          0
    lo: 23456499   15449    0    0    0     0          0         0 23456499   15449    0    0    0     0       0          0`
	n := parseNetDev(s)
	c := n["rmnet0"]
	if c.rx != 206276712 || c.tx != 186180919 || c.rxErr != 1 || c.rxDrop != 2 || c.txErr != 3 || c.txDrop != 4 {
		t.Fatalf("rmnet0 = %+v", c)
	}
	if _, ok := n["lo"]; !ok {
		t.Fatal("lo harus terbaca (difilter di collector)")
	}
}

func TestParseDiskstats(t *testing.T) {
	s := ` 179       0 mmcblk0 2702680 688648 130655204 4456470 1154290 16154 45671568 8764410 0 2214500 13222090
 179       1 mmcblk0p1 10 0 20 0 0 0 0 0 0 0 0
 253       0 dm-0 1 0 2 0 0 0 0 0 0 0 0
   8       0 sda 5 0 10 0 6 0 12 0 0 0 0`
	d := parseDiskstats(s)
	if len(d) != 2 {
		t.Fatalf("hanya disk utuh yang diambil: %+v", d)
	}
	if d["mmcblk0"].readSect != 130655204 || d["mmcblk0"].writes != 1154290 {
		t.Fatalf("mmcblk0 = %+v", d["mmcblk0"])
	}
}

func TestNormalizeTemp(t *testing.T) {
	cases := map[float64]float64{36: 36, 305: 30.5, 35000: 35, 45123: 45.123}
	for in, want := range cases {
		if got, ok := normalizeTemp(in); !ok || got != want {
			t.Errorf("normalizeTemp(%v) = %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := normalizeTemp(0); ok {
		t.Error("0 harus dianggap tidak valid")
	}
	if !cpuThermalRe.MatchString("tsens_tz_sensor6") || cpuThermalRe.MatchString("battery") || cpuThermalRe.MatchString("mpp2_div1") {
		t.Error("regex sensor CPU salah")
	}
}

func TestParseProcStat(t *testing.T) {
	p, ticks, ok := parseProcStat("8751 (monitor (x)) S 8749 8749 0 0 -1 4194560 100 0 0 0 1200 300 0 0 20 0 7 0 500 586896384 2140 18446744073709551615")
	if !ok || p.PID != 8751 || p.Name != "monitor (x)" || p.State != "S" || ticks != 1500 || p.Threads != 7 {
		t.Fatalf("got %+v ticks=%d ok=%v", p, ticks, ok)
	}
	if p.RSS == 0 {
		t.Fatal("rss kosong")
	}
}

func TestHistoryStep(t *testing.T) {
	if historyStep(1, 0) != 0 || historyStep(1, 360) != 0 {
		t.Fatal("1 jam / 360 titik harus data mentah")
	}
	if s := historyStep(24, 360); s != 240 {
		t.Fatalf("24h/360 = %d", s)
	}
	if s := historyStep(168, 360); s%60 != 0 || 168*3600/s > 360 {
		t.Fatalf("7d step = %d", s)
	}
}

func TestLiveRing(t *testing.T) {
	r := newLiveRing(3)
	for i := int64(1); i <= 5; i++ {
		r.add(LivePoint{Time: i})
	}
	got := r.since(0)
	if len(got) != 3 || got[0].Time != 3 || got[2].Time != 5 {
		t.Fatalf("ring = %+v", got)
	}
	if len(r.since(4)) != 1 {
		t.Fatal("since filter salah")
	}
}

func TestSummarizeMinute(t *testing.T) {
	a, b := 10.0, 30.0
	p := summarizeMinute([]LivePoint{{Time: 1, CPU: 20, RAM: 50, NetRx: &a}, {Time: 2, CPU: 40, RAM: 55, NetRx: &b}})
	if p.CPU != 30 || *p.CPUMax != 40 || p.RAM != 55 || *p.NetRx != 20 || p.Temp != nil || p.Time != 2 {
		t.Fatalf("got %+v", p)
	}
}

func TestLevelForHysteresis(t *testing.T) {
	th := Threshold{80, 90}
	if levelFor(85, th, 0, 5) != 1 || levelFor(95, th, 0, 5) != 2 {
		t.Fatal("level naik salah")
	}
	if levelFor(87, th, 2, 5) != 2 {
		t.Fatal("hysteresis critical: 87 masih critical")
	}
	if levelFor(76, th, 1, 5) != 1 || levelFor(74, th, 1, 5) != 0 {
		t.Fatal("hysteresis warning salah")
	}
}

func TestAlertEngineDebounceAndResolve(t *testing.T) {
	a := &alertEngine{states: map[string]*ruleState{}, persist: make(chan Event, 100)}
	s := Status{CPUPercent: 99, Services: map[string]bool{"nginx": true}}
	a.evaluate(&s)
	a.evaluate(&s)
	if act, _ := a.snapshot(10); len(act) != 0 {
		t.Fatal("alert tidak boleh muncul sebelum 3 sampel")
	}
	a.evaluate(&s)
	act, _ := a.snapshot(10)
	if len(act) != 1 || act[0].Severity != "critical" || !strings.Contains(act[0].Message, "CPU") {
		t.Fatalf("active = %+v", act)
	}
	s.CPUPercent = 10
	s.Services["nginx"] = false
	for i := 0; i < 3; i++ {
		a.evaluate(&s)
	}
	act, evs := a.snapshot(10)
	if len(act) != 1 || act[0].Key != "service:nginx" {
		t.Fatalf("active = %+v", act)
	}
	if len(evs) != 2 || evs[1].ResolvedAt == nil {
		t.Fatalf("events = %+v", evs)
	}
	if h := a.health(); h.State != "degraded" {
		t.Fatalf("health = %+v", h)
	}
}
