package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const (
	usageFile           = "usage.json"
	usageKeepDays       = 90
	usageMaxSessionsDev = 1000
	usageMaxPayloadDays = 100
	usageMaxAppsPerDay  = 500
)

var usageDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type UsageSessionRec struct {
	App   string    `json:"app"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type deviceUsage struct {
	Days     map[string]map[string]int `json:"days"`
	Sessions []UsageSessionRec         `json:"sessions"`
}

var usageStore = map[string]*deviceUsage{}

func saveUsage() {
	b, err := json.Marshal(usageStore)
	if err != nil {
		log.Println("usage save failed:", err)
		return
	}
	tmp := usageFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Println("usage save failed:", err)
		return
	}
	if err := os.Rename(tmp, usageFile); err != nil {
		log.Println("usage save failed:", err)
	}
}

func mergeUsage(du *deviceUsage, in deviceUsage, now time.Time) {
	if du.Days == nil {
		du.Days = map[string]map[string]int{}
	}
	for date, apps := range in.Days {
		if !usageDateRe.MatchString(date) {
			continue
		}
		cur := du.Days[date]
		if cur == nil {
			cur = map[string]int{}
			du.Days[date] = cur
		}
		for app, sec := range apps {
			if app == "" || len(app) > 100 || sec <= 0 {
				continue
			}
			if sec > 86400 {
				sec = 86400
			}
			if _, ok := cur[app]; !ok && len(cur) >= usageMaxAppsPerDay {
				continue
			}
			cur[app] += sec
			if cur[app] > 86400 {
				cur[app] = 86400
			}
		}
	}
	cutoff := now.AddDate(0, 0, -usageKeepDays).Format("2006-01-02")
	for date := range du.Days {
		if date < cutoff {
			delete(du.Days, date)
		}
	}
	for _, s := range in.Sessions {
		if s.App == "" || len(s.App) > 100 || !s.End.After(s.Start) || s.End.Sub(s.Start) > 24*time.Hour {
			continue
		}
		du.Sessions = append(du.Sessions, UsageSessionRec{App: s.App, Start: s.Start, End: s.End})
	}
	if len(du.Sessions) > usageMaxSessionsDev {
		du.Sessions = du.Sessions[len(du.Sessions)-usageMaxSessionsDev:]
	}
}

type usageAppView struct {
	App     string `json:"app"`
	Seconds int    `json:"seconds"`
}

type usageDayView struct {
	Date  string         `json:"date"`
	Total int            `json:"total"`
	Apps  []usageAppView `json:"apps"`
}

type usageView struct {
	Days     []usageDayView    `json:"days"`
	Sessions []UsageSessionRec `json:"sessions"`
}

func init() {
	if b, err := os.ReadFile(usageFile); err == nil {
		var m map[string]*deviceUsage
		if json.Unmarshal(b, &m) == nil && m != nil {
			usageStore = m
		}
	}

	http.HandleFunc("POST /usage", func(w http.ResponseWriter, r *http.Request) {
		d := authDevice(r)
		if d == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var in deviceUsage
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in) != nil || len(in.Days) > usageMaxPayloadDays {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		du := usageStore[d.ID]
		if du == nil {
			du = &deviceUsage{}
			usageStore[d.ID] = du
		}
		mergeUsage(du, in, time.Now())
		saveUsage()
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})

	http.HandleFunc("GET /admin/devices/{id}/usage", admin(func(w http.ResponseWriter, r *http.Request) {
		n := 7
		if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= usageKeepDays {
			n = v
		}
		cutoff := time.Now().AddDate(0, 0, -(n - 1)).Format("2006-01-02")
		out := usageView{Days: []usageDayView{}, Sessions: []UsageSessionRec{}}
		mu.Lock()
		if du := usageStore[r.PathValue("id")]; du != nil {
			for date, apps := range du.Days {
				if date < cutoff {
					continue
				}
				dv := usageDayView{Date: date, Apps: []usageAppView{}}
				for app, sec := range apps {
					dv.Apps = append(dv.Apps, usageAppView{App: app, Seconds: sec})
					dv.Total += sec
				}
				sort.Slice(dv.Apps, func(i, j int) bool { return dv.Apps[i].Seconds > dv.Apps[j].Seconds })
				out.Days = append(out.Days, dv)
			}
			sort.Slice(out.Days, func(i, j int) bool { return out.Days[i].Date > out.Days[j].Date })
			for i := len(du.Sessions) - 1; i >= 0 && len(out.Sessions) < 200; i-- {
				out.Sessions = append(out.Sessions, du.Sessions[i])
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
}