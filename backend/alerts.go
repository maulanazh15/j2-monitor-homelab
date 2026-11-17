package main

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

type Event struct {
	ID         int64    `json:"id"`
	Key        string   `json:"key"`
	Severity   string   `json:"severity"` // info | warning | critical
	Message    string   `json:"message"`
	Value      *float64 `json:"value"`
	StartedAt  int64    `json:"started_at"`
	ResolvedAt *int64   `json:"resolved_at"`
}

const (
	confirmSamples = 3   // kondisi harus bertahan 3 sampel (~15 detik) sebelum alert berubah
	maxEvents      = 300 // event yang disimpan di memori
)

type rule struct {
	key, label, unit string
	hyst             float64
	value            func(*Status) *float64
}

var rules = []rule{
	{"cpu", "CPU", "%", 5, func(s *Status) *float64 { v := s.CPUPercent; return &v }},
	{"ram", "RAM", "%", 3, func(s *Status) *float64 { v := s.Memory.Percent; return &v }},
	{"disk", "Disk", "%", 1, func(s *Status) *float64 {
		if s.Disk.Total == 0 {
			return nil
		}
		v := s.Disk.Percent
		return &v
	}},
	{"temp", "Suhu CPU", "°C", 3, func(s *Status) *float64 { return s.Temperature.CPU }},
	{"battery_temp", "Suhu baterai", "°C", 2, func(s *Status) *float64 { return s.Temperature.Battery }},
}

type ruleState struct {
	level, cand, count int
	eventID            int64
}

type alertEngine struct {
	mu      sync.RWMutex
	states  map[string]*ruleState
	events  []Event // terbaru di akhir
	seq     int64
	persist chan Event
}

func newAlertEngine() *alertEngine {
	a := &alertEngine{states: map[string]*ruleState{}, persist: make(chan Event, 64)}
	go func() {
		for e := range a.persist {
			if err := saveEvent(e); err != nil {
				log.Println("event:", err)
			}
		}
	}()
	return a
}

var severities = []string{"", "warning", "critical"}

func (a *alertEngine) newID(now time.Time) int64 {
	id := now.UnixNano() / 1000
	if id <= a.seq {
		id = a.seq + 1
	}
	a.seq = id
	return id
}

func (a *alertEngine) store(e Event) {
	for i := len(a.events) - 1; i >= 0; i-- {
		if a.events[i].ID == e.ID {
			a.events[i] = e
			a.queue(e)
			return
		}
	}
	a.events = append(a.events, e)
	if len(a.events) > maxEvents {
		a.events = a.events[len(a.events)-maxEvents:]
	}
	a.queue(e)
}

func (a *alertEngine) queue(e Event) {
	select {
	case a.persist <- e:
	default:
		log.Println("event: antrean penuh, event tidak disimpan ke DB")
	}
}

func (a *alertEngine) find(id int64) *Event {
	for i := len(a.events) - 1; i >= 0; i-- {
		if a.events[i].ID == id {
			return &a.events[i]
		}
	}
	return nil
}

// load mengambil event dari DB saat start. Event yang masih aktif dari proses
// sebelumnya ditutup karena statusnya tidak lagi diketahui; akan dibuka ulang
// bila kondisinya masih terjadi.
func (a *alertEngine) load(evs []Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().Unix()
	sort.Slice(evs, func(i, j int) bool { return evs[i].StartedAt < evs[j].StartedAt })
	for _, e := range evs {
		if e.ResolvedAt == nil {
			e.ResolvedAt = &now
			a.queue(e)
		}
		if e.ID > a.seq {
			a.seq = e.ID
		}
		a.events = append(a.events, e)
	}
}

func (a *alertEngine) info(key, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	ts := now.Unix()
	a.store(Event{ID: a.newID(now), Key: key, Severity: "info", Message: msg, StartedAt: ts, ResolvedAt: &ts})
}

func levelFor(v float64, t Threshold, cur int, hyst float64) int {
	lvl := 0
	if v >= t.Critical {
		lvl = 2
	} else if v >= t.Warning {
		lvl = 1
	}
	// hysteresis: turun level hanya bila nilai cukup jauh di bawah ambang
	if lvl < cur {
		if cur == 2 && v >= t.Critical-hyst {
			lvl = 2
		} else if cur >= 1 && v >= t.Warning-hyst && lvl < 1 {
			lvl = 1
		}
	}
	return lvl
}

func (a *alertEngine) evaluate(s *Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for _, r := range rules {
		v := r.value(s)
		if v == nil {
			continue // metrik tidak didukung: tidak ada alert
		}
		t := cfg.Thresholds[r.key]
		st := a.state(r.key)
		target := levelFor(*v, t, st.level, r.hyst)
		val := round1(*v)
		a.step(st, r.key, target, &val, now, func(l int) string {
			th := t.Warning
			if l == 2 {
				th = t.Critical
			}
			return fmt.Sprintf("%s tinggi: %.1f%s (ambang %s %.0f%s)", r.label, val, r.unit, severities[l], th, r.unit)
		})
	}
	for name, up := range s.Services {
		st := a.state("service:" + name)
		target := 0
		if !up {
			target = 2
		}
		a.step(st, "service:"+name, target, nil, now, func(int) string { return "Service " + name + " tidak berjalan" })
	}
}

func (a *alertEngine) state(key string) *ruleState {
	st := a.states[key]
	if st == nil {
		st = &ruleState{}
		a.states[key] = st
	}
	return st
}

func (a *alertEngine) step(st *ruleState, key string, target int, val *float64, now time.Time, msg func(int) string) {
	if target == st.level {
		st.count = 0
		// perbarui nilai puncak pada event aktif
		if st.eventID != 0 && val != nil {
			if e := a.find(st.eventID); e != nil && e.Value != nil && *val > *e.Value {
				v := *val
				e.Value = &v
			}
		}
		return
	}
	if target == st.cand {
		st.count++
	} else {
		st.cand, st.count = target, 1
	}
	if st.count < confirmSamples {
		return
	}
	ts := now.Unix()
	if st.eventID != 0 {
		if e := a.find(st.eventID); e != nil {
			cp := *e
			cp.ResolvedAt = &ts
			a.store(cp)
		}
		st.eventID = 0
	}
	if target > 0 {
		e := Event{ID: a.newID(now), Key: key, Severity: severities[target], Message: msg(target), Value: val, StartedAt: ts}
		a.store(e)
		st.eventID = e.ID
	}
	st.level, st.count = target, 0
}

func (a *alertEngine) snapshot(limit int) (active, events []Event) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	active, events = []Event{}, []Event{}
	for i := len(a.events) - 1; i >= 0; i-- {
		e := a.events[i]
		if e.ResolvedAt == nil {
			active = append(active, e)
		}
		if len(events) < limit {
			events = append(events, e)
		}
	}
	return
}

func (a *alertEngine) health() Health {
	active, _ := a.snapshot(0)
	h := Health{State: "online", Reasons: []string{}}
	for _, e := range active {
		h.State = "degraded"
		h.Reasons = append(h.Reasons, e.Message)
	}
	return h
}
