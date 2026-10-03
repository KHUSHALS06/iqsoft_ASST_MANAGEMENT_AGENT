package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	historyFile      = "history.json"
	maxHistoryPerDev = 500
)

type HistoryEvent struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Source     string    `json:"source"`
	OldVersion string    `json:"old_version,omitempty"`
	NewVersion string    `json:"new_version,omitempty"`
}

var history = map[string][]HistoryEvent{}

type histApp struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  string `json:"source"`
}

func parseApps(inv json.RawMessage) (map[string]histApp, bool) {
	var p struct {
		Software []histApp `json:"software"`
	}
	if inv == nil || json.Unmarshal(inv, &p) != nil || p.Software == nil {
		return nil, false
	}
	m := make(map[string]histApp, len(p.Software))
	for _, a := range p.Software {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		m[strings.ToLower(a.Name)+"|"+strings.ToLower(a.Source)] = a
	}
	return m, true
}

func diffApps(oldM, newM map[string]histApp, now time.Time) []HistoryEvent {
	var ev []HistoryEvent
	for k, n := range newM {
		o, ok := oldM[k]
		if !ok {
			ev = append(ev, HistoryEvent{Time: now, Kind: "installed", Name: n.Name, Source: n.Source, NewVersion: n.Version})
		} else if o.Version != n.Version {
			ev = append(ev, HistoryEvent{Time: now, Kind: "changed", Name: n.Name, Source: n.Source, OldVersion: o.Version, NewVersion: n.Version})
		}
	}
	for k, o := range oldM {
		if _, ok := newM[k]; !ok {
			ev = append(ev, HistoryEvent{Time: now, Kind: "removed", Name: o.Name, Source: o.Source, OldVersion: o.Version})
		}
	}
	return ev
}

func saveHistory() {
	b, err := json.Marshal(history)
	if err != nil {
		log.Println("history save failed:", err)
		return
	}
	tmp := historyFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Println("history save failed:", err)
		return
	}
	if err := os.Rename(tmp, historyFile); err != nil {
		log.Println("history save failed:", err)
	}
}

func recordHistory(deviceID string, oldInv, newInv json.RawMessage) {
	oldM, okOld := parseApps(oldInv)
	newM, okNew := parseApps(newInv)
	if !okOld || !okNew {
		return
	}
	ev := diffApps(oldM, newM, time.Now())
	if len(ev) == 0 {
		return
	}
	list := append(history[deviceID], ev...)
	if len(list) > maxHistoryPerDev {
		list = list[len(list)-maxHistoryPerDev:]
	}
	history[deviceID] = list
	saveHistory()
	log.Printf("history %s: %d change(s)", deviceID, len(ev))
}

func init() {
	if b, err := os.ReadFile(historyFile); err == nil {
		var h map[string][]HistoryEvent
		if json.Unmarshal(b, &h) == nil && h != nil {
			history = h
		}
	}

	http.HandleFunc("GET /admin/devices/{id}/history", admin(func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= maxHistoryPerDev {
			limit = v
		}
		mu.Lock()
		src := history[r.PathValue("id")]
		out := make([]HistoryEvent, 0, limit)
		for i := len(src) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, src[i])
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
}