package main

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------- tipe payload ----------

type Memory struct {
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Free        uint64  `json:"free"` // = MemAvailable (kompatibel v1)
	Percent     float64 `json:"percent"`
	SwapTotal   uint64  `json:"swap_total"`
	SwapUsed    uint64  `json:"swap_used"`
	SwapFree    uint64  `json:"swap_free"`
	SwapPercent float64 `json:"swap_percent"`
	// v2
	Available uint64 `json:"available"`
	Cached    uint64 `json:"cached"`
	Buffers   uint64 `json:"buffers"`
}

type Disk struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	Percent float64 `json:"percent"`
	Mount   string  `json:"mount,omitempty"`
}

type CPUInfo struct {
	PerCore []*float64 `json:"per_core"` // null = core offline / tidak terbaca
	FreqMHz []*float64 `json:"freq_mhz"` // null bila tidak tersedia
}

type NetIface struct {
	Name    string   `json:"name"`
	RxRate  *float64 `json:"rx_rate"`
	TxRate  *float64 `json:"tx_rate"`
	RxBytes uint64   `json:"rx_bytes"`
	TxBytes uint64   `json:"tx_bytes"`
	RxErrs  uint64   `json:"rx_errors"`
	TxErrs  uint64   `json:"tx_errors"`
	RxDrop  uint64   `json:"rx_drops"`
	TxDrop  uint64   `json:"tx_drops"`
}

type Network struct {
	RxRate     *float64   `json:"rx_rate"` // byte/detik, null pada sampel pertama
	TxRate     *float64   `json:"tx_rate"`
	RxTotal    uint64     `json:"rx_total"`
	TxTotal    uint64     `json:"tx_total"`
	Interfaces []NetIface `json:"interfaces"`
}

type DiskIO struct {
	Devices   []string `json:"devices"`
	ReadRate  *float64 `json:"read_rate"` // byte/detik
	WriteRate *float64 `json:"write_rate"`
	ReadIOPS  *float64 `json:"read_iops"`
	WriteIOPS *float64 `json:"write_iops"`
}

type Temperature struct {
	CPU       *float64 `json:"cpu"`
	CPUSource string   `json:"cpu_source,omitempty"`
	Battery   *float64 `json:"battery"`
}

type Battery struct {
	Level  *float64 `json:"level"`
	Status string   `json:"status,omitempty"`
}

type Health struct {
	State   string   `json:"state"` // online | degraded
	Reasons []string `json:"reasons"`
}

type Status struct {
	// --- field v1 (jangan diubah, dipakai klien lama) ---
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
	// --- field v2 (tambahan) ---
	OS          string      `json:"os"`
	Kernel      string      `json:"kernel"`
	Model       string      `json:"model,omitempty"`
	BootTime    int64       `json:"boot_time"`
	Interval    int         `json:"interval"`
	CPU         CPUInfo     `json:"cpu"`
	Network     *Network    `json:"network"`
	DiskIO      *DiskIO     `json:"disk_io"`
	Filesystems []Disk      `json:"filesystems"`
	Temperature Temperature `json:"temperature"`
	Battery     *Battery    `json:"battery"`
	Containers  *int        `json:"containers"` // selalu null: tidak didukung
	Health      Health      `json:"health"`
}

// LivePoint adalah satu sampel 5 detik yang disimpan di memori (ring buffer).
type LivePoint struct {
	Time      int64    `json:"time"`
	CPU       float64  `json:"cpu"`
	RAM       float64  `json:"ram"`
	Swap      float64  `json:"swap"`
	Disk      float64  `json:"disk"`
	NetRx     *float64 `json:"net_rx"`
	NetTx     *float64 `json:"net_tx"`
	DiskRead  *float64 `json:"disk_read"`
	DiskWrite *float64 `json:"disk_write"`
	Temp      *float64 `json:"temp"`
}

// ---------- helper ----------

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func fptr(v float64) *float64 { r := round1(v); return &r }

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) * 100 / float64(total)
}

// ---------- parser (dipisah agar bisa dites) ----------

func parseMeminfo(s string) Memory {
	values := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				values[strings.TrimSuffix(f[0], ":")] = v * 1024
			}
		}
	}
	var m Memory
	m.Total = values["MemTotal"]
	m.Available = values["MemAvailable"]
	if m.Available == 0 {
		m.Available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	m.Free = m.Available
	if m.Total >= m.Free {
		m.Used = m.Total - m.Free
	}
	m.Percent = pct(m.Used, m.Total)
	m.Cached = values["Cached"]
	m.Buffers = values["Buffers"]
	m.SwapTotal = values["SwapTotal"]
	m.SwapFree = values["SwapFree"]
	if m.SwapTotal >= m.SwapFree {
		m.SwapUsed = m.SwapTotal - m.SwapFree
	}
	m.SwapPercent = pct(m.SwapUsed, m.SwapTotal)
	return m
}

type cpuTimes struct{ total, idle uint64 }

// parseCPUStat mengembalikan agregat, per-core (indeks -> waktu) dan btime.
// Pada Android core yang di-hotplug offline tidak muncul di /proc/stat.
func parseCPUStat(s string) (agg cpuTimes, cores map[int]cpuTimes, btime int64) {
	cores = map[int]cpuTimes{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "cpu":
			agg = cpuLine(f[1:])
		case strings.HasPrefix(f[0], "cpu"):
			if idx, err := strconv.Atoi(f[0][3:]); err == nil {
				cores[idx] = cpuLine(f[1:])
			}
		case f[0] == "btime":
			btime, _ = strconv.ParseInt(f[1], 10, 64)
		}
	}
	return
}

func cpuLine(f []string) cpuTimes {
	// user nice system idle iowait irq softirq steal (guest sudah termasuk di user)
	var t cpuTimes
	for i, s := range f {
		if i >= 8 {
			break
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			continue
		}
		t.total += v
		if i == 3 || i == 4 {
			t.idle += v
		}
	}
	return t
}

// cpuUsage mengembalikan persen pemakaian, ok=false bila delta tidak valid.
func cpuUsage(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0, false
	}
	dt, di := cur.total-prev.total, cur.idle-prev.idle
	if di > dt {
		return 0, false
	}
	return float64(dt-di) * 100 / float64(dt), true
}

type netCounters struct{ rx, tx, rxErr, txErr, rxDrop, txDrop uint64 }

func parseNetDev(s string) map[string]netCounters {
	out := map[string]netCounters{}
	for _, line := range strings.Split(s, "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		f := strings.Fields(line[i+1:])
		if name == "" || len(f) < 16 {
			continue
		}
		u := func(k int) uint64 { v, _ := strconv.ParseUint(f[k], 10, 64); return v }
		out[name] = netCounters{rx: u(0), rxErr: u(2), rxDrop: u(3), tx: u(8), txErr: u(10), txDrop: u(11)}
	}
	return out
}

type diskCounters struct{ reads, readSect, writes, writeSect uint64 }

var wholeDiskRe = regexp.MustCompile(`^(mmcblk\d+|sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+)$`)

// parseDiskstats hanya mengambil disk utuh (bukan partisi/dm/loop) agar tidak dobel hitung.
func parseDiskstats(s string) map[string]diskCounters {
	out := map[string]diskCounters{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || !wholeDiskRe.MatchString(f[2]) {
			continue
		}
		u := func(k int) uint64 { v, _ := strconv.ParseUint(f[k], 10, 64); return v }
		out[f[2]] = diskCounters{reads: u(3), readSect: u(5), writes: u(7), writeSect: u(9)}
	}
	return out
}

// normalizeTemp mengubah nilai sysfs (m°C, d°C atau °C) menjadi °C.
// Mengembalikan ok=false untuk nilai yang tidak masuk akal.
func normalizeTemp(raw float64) (float64, bool) {
	v := raw
	switch {
	case v >= 1000:
		v /= 1000
	case v >= 150:
		v /= 10
	}
	if v <= 0 || v > 125 {
		return 0, false
	}
	return v, true
}

var cpuThermalRe = regexp.MustCompile(`(?i)(tsens|cpu|x86_pkg_temp|coretemp|soc[-_]?thermal|k10temp|cpu-thermal)`)

// ---------- sampler ----------

type sampler struct {
	mu        sync.Mutex
	prevAt    time.Time
	prevCPU   cpuTimes
	prevCores map[int]cpuTimes
	prevNet   map[string]netCounters
	prevDisk  map[string]diskCounters
	cpuPct    float64
	coresPct  map[int]float64

	svcMu   sync.Mutex
	svcAt   time.Time
	svcVals map[string]bool

	static staticInfo
}

type staticInfo struct {
	hostname, os, kernel, model, arch string
}

func detectStatic() staticInfo {
	host, _ := os.Hostname()
	si := staticInfo{hostname: host, arch: runtime.GOARCH, kernel: strings.TrimSpace(readFile("/proc/sys/kernel/osrelease"))}
	if m := strings.TrimSpace(readFile("/proc/sys/kernel/arch")); m != "" {
		si.arch = m
	} else if out, err := exec.Command("uname", "-m").Output(); err == nil {
		si.arch = strings.TrimSpace(string(out))
	}
	if runtime.GOOS == "android" || os.Getenv("ANDROID_ROOT") != "" {
		ver := getprop("ro.build.version.release")
		si.os = strings.TrimSpace("Android " + ver)
		si.model = getprop("ro.product.model")
	} else {
		si.os = osRelease()
	}
	if si.os == "" {
		si.os = runtime.GOOS
	}
	return si
}

func getprop(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getprop", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func osRelease() string {
	sc := bufio.NewScanner(strings.NewReader(readFile("/etc/os-release")))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func statDisk(path string) (Disk, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil || st.Blocks == 0 {
		return Disk{Mount: path}, false
	}
	bs := uint64(st.Bsize)
	d := Disk{Mount: path, Total: st.Blocks * bs, Free: st.Bavail * bs}
	// Rumus sama dengan v1 (used = total - available) agar history tetap konsisten.
	if d.Total >= d.Free {
		d.Used = d.Total - d.Free
	}
	d.Percent = pct(d.Used, d.Total)
	return d, true
}

func readFilesystems() []Disk {
	out := []Disk{}
	seen := map[string]bool{}
	for _, p := range cfg.FSPaths {
		d, ok := statDisk(p)
		if !ok {
			continue
		}
		sig := fmt.Sprintf("%d/%d", d.Total, d.Free)
		if seen[sig] { // mount yang sama (bind/sdcardfs) cukup ditampilkan sekali
			continue
		}
		seen[sig] = true
		d.Percent = round1(d.Percent)
		out = append(out, d)
	}
	return out
}

func readLoad() []string {
	f := strings.Fields(readFile("/proc/loadavg"))
	if len(f) < 3 {
		return []string{"0", "0", "0"}
	}
	return f[:3]
}

func readUptime() uint64 {
	f := strings.Fields(readFile("/proc/uptime"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return uint64(v)
}

func readFreqs(n int) []*float64 {
	out := make([]*float64, n)
	for i := 0; i < n; i++ {
		s := strings.TrimSpace(readFile(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", i)))
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
			out[i] = fptr(v / 1000)
		}
	}
	return out
}

func readTemps() Temperature {
	var t Temperature
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, z := range zones {
		typ := strings.TrimSpace(readFile(z + "/type"))
		if !cpuThermalRe.MatchString(typ) {
			continue
		}
		raw, err := strconv.ParseFloat(strings.TrimSpace(readFile(z+"/temp")), 64)
		if err != nil {
			continue
		}
		if v, ok := normalizeTemp(raw); ok && (t.CPU == nil || v > *t.CPU) {
			t.CPU = fptr(v)
			t.CPUSource = typ
		}
	}
	if raw, err := strconv.ParseFloat(strings.TrimSpace(readFile("/sys/class/power_supply/battery/temp")), 64); err == nil {
		if v, ok := normalizeTemp(raw); ok {
			t.Battery = fptr(v)
		}
	}
	return t
}

func readBattery() *Battery {
	capS := strings.TrimSpace(readFile("/sys/class/power_supply/battery/capacity"))
	v, err := strconv.ParseFloat(capS, 64)
	if err != nil {
		return nil
	}
	return &Battery{Level: &v, Status: strings.TrimSpace(readFile("/sys/class/power_supply/battery/status"))}
}

// serviceRunning membaca file status runit secara langsung (tanpa fork proses),
// dengan fallback ke `sv status` bila file tidak bisa dibaca.
func serviceRunning(name string) bool {
	if st := strings.TrimSpace(readFile(filepath.Join(cfg.ServiceDir, name, "supervise", "stat"))); st != "" {
		return st == "run"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sv", "status", name).CombinedOutput()
	return err == nil && strings.HasPrefix(string(out), "run:")
}

func (s *sampler) services() map[string]bool {
	s.svcMu.Lock()
	defer s.svcMu.Unlock()
	if s.svcVals == nil || time.Since(s.svcAt) >= 15*time.Second {
		vals := map[string]bool{}
		for _, n := range cfg.Services {
			vals[n] = serviceRunning(n)
		}
		s.svcVals, s.svcAt = vals, time.Now()
	}
	cp := make(map[string]bool, len(s.svcVals))
	for k, v := range s.svcVals {
		cp[k] = v
	}
	return cp
}

func (s *sampler) collect() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	dt := now.Sub(s.prevAt).Seconds()
	havePrev := !s.prevAt.IsZero() && dt > 0.5

	// CPU
	agg, cores, btime := parseCPUStat(readFile("/proc/stat"))
	if havePrev {
		if v, ok := cpuUsage(s.prevCPU, agg); ok {
			s.cpuPct = v
		}
	}
	maxIdx := runtime.NumCPU() - 1
	for i := range cores {
		if i > maxIdx {
			maxIdx = i
		}
	}
	perCore := make([]*float64, maxIdx+1)
	newCorePct := map[int]float64{}
	for i, c := range cores {
		if p, ok := s.prevCores[i]; ok && havePrev {
			if v, ok := cpuUsage(p, c); ok {
				newCorePct[i] = v
			} else if old, ok := s.coresPct[i]; ok {
				newCorePct[i] = old
			}
		}
		if v, ok := newCorePct[i]; ok {
			perCore[i] = fptr(v)
		}
	}
	s.prevCPU, s.prevCores, s.coresPct = agg, cores, newCorePct

	// Network
	var network *Network
	if nets := parseNetDev(readFile("/proc/net/dev")); len(nets) > 0 {
		network = &Network{Interfaces: []NetIface{}}
		var rxSum, txSum float64
		ratesOK := havePrev && s.prevNet != nil
		names := make([]string, 0, len(nets))
		for n := range nets {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := nets[n]
			if n == "lo" {
				continue
			}
			network.RxTotal += c.rx
			network.TxTotal += c.tx
			ifc := NetIface{Name: n, RxBytes: c.rx, TxBytes: c.tx, RxErrs: c.rxErr, TxErrs: c.txErr, RxDrop: c.rxDrop, TxDrop: c.txDrop}
			if p, ok := s.prevNet[n]; ok && ratesOK {
				rx, tx := 0.0, 0.0
				if c.rx >= p.rx {
					rx = float64(c.rx-p.rx) / dt
				}
				if c.tx >= p.tx {
					tx = float64(c.tx-p.tx) / dt
				}
				ifc.RxRate, ifc.TxRate = fptr(rx), fptr(tx)
				rxSum += rx
				txSum += tx
			}
			if c.rx+c.tx > 0 { // sembunyikan interface yang tidak pernah dipakai
				network.Interfaces = append(network.Interfaces, ifc)
			}
		}
		if ratesOK {
			network.RxRate, network.TxRate = fptr(rxSum), fptr(txSum)
		}
		s.prevNet = nets
	}

	// Disk I/O
	var dio *DiskIO
	if disks := parseDiskstats(readFile("/proc/diskstats")); len(disks) > 0 {
		dio = &DiskIO{Devices: []string{}}
		var r, w, ri, wi float64
		ok := havePrev && s.prevDisk != nil
		for n, c := range disks {
			dio.Devices = append(dio.Devices, n)
			if p, found := s.prevDisk[n]; found && ok {
				if c.readSect >= p.readSect && c.writeSect >= p.writeSect && c.reads >= p.reads && c.writes >= p.writes {
					r += float64(c.readSect-p.readSect) * 512 / dt
					w += float64(c.writeSect-p.writeSect) * 512 / dt
					ri += float64(c.reads-p.reads) / dt
					wi += float64(c.writes-p.writes) / dt
				}
			}
		}
		sort.Strings(dio.Devices)
		if ok {
			dio.ReadRate, dio.WriteRate, dio.ReadIOPS, dio.WriteIOPS = fptr(r), fptr(w), fptr(ri), fptr(wi)
		}
		s.prevDisk = disks
	}
	s.prevAt = now

	disk, _ := statDisk(cfg.DiskPath)
	disk.Mount = ""

	return Status{
		Hostname:     s.static.hostname,
		Architecture: s.static.arch,
		CPUCores:     runtime.NumCPU(),
		CPUPercent:   s.cpuPct,
		Load:         readLoad(),
		Uptime:       readUptime(),
		Memory:       parseMeminfo(readFile("/proc/meminfo")),
		Disk:         disk,
		Services:     s.services(),
		Time:         now.Unix(),
		OS:           s.static.os,
		Kernel:       s.static.kernel,
		Model:        s.static.model,
		BootTime:     btime,
		Interval:     int(sampleInterval / time.Second),
		CPU:          CPUInfo{PerCore: perCore, FreqMHz: readFreqs(maxIdx + 1)},
		Network:      network,
		DiskIO:       dio,
		Filesystems:  readFilesystems(),
		Temperature:  readTemps(),
		Battery:      readBattery(),
	}
}

func toLivePoint(s Status) LivePoint {
	p := LivePoint{
		Time: s.Time, CPU: round1(s.CPUPercent), RAM: round1(s.Memory.Percent),
		Swap: round1(s.Memory.SwapPercent), Disk: round1(s.Disk.Percent),
		Temp: s.Temperature.CPU,
	}
	if s.Network != nil {
		p.NetRx, p.NetTx = s.Network.RxRate, s.Network.TxRate
	}
	if s.DiskIO != nil {
		p.DiskRead, p.DiskWrite = s.DiskIO.ReadRate, s.DiskIO.WriteRate
	}
	return p
}

// ---------- ring buffer data live ----------

type liveRing struct {
	mu   sync.RWMutex
	buf  []LivePoint
	next int
	full bool
}

func newLiveRing(n int) *liveRing { return &liveRing{buf: make([]LivePoint, n)} }

func (r *liveRing) add(p LivePoint) {
	r.mu.Lock()
	r.buf[r.next] = p
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// since mengembalikan titik dengan Time > t, urut naik.
func (r *liveRing) since(t int64) []LivePoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []LivePoint{}
	n, start := r.next, 0
	if r.full {
		n, start = len(r.buf), r.next
	}
	for i := 0; i < n; i++ {
		p := r.buf[(start+i)%len(r.buf)]
		if p.Time > t {
			out = append(out, p)
		}
	}
	return out
}
