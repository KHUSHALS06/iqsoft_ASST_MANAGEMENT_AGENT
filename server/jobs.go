package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	jobTimeout      = 30 * time.Minute
	maxJobOutput    = 8 << 10
	maxOpenJobs     = 20
	maxFinishedJobs = 50
)

var (
	jobs        = map[string]*Job{}
	packageIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}$`)
	versionRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,63}$`)
	jobTypes    = map[string]bool{
		"refresh_inventory": true,
		"winget_install":    true,
		"winget_upgrade":    true,
		"winget_uninstall":  true,
	}
)

type Job struct {
	ID        string            `json:"id"`
	DeviceID  string            `json:"device_id"`
	Type      string            `json:"type"`
	Params    map[string]string `json:"params,omitempty"`
	Status    string            `json:"status"`
	Output    string            `json:"output,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type jobOrder struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"`
	Params map[string]string `json:"params"`
}

func cleanParams(jobType string, in map[string]string) (map[string]string, string) {
	if jobType == "refresh_inventory" {
		return map[string]string{}, ""
	}
	id := in["id"]
	if !packageIDRe.MatchString(id) {
		return nil, "invalid package id"
	}
	out := map[string]string{"id": id}
	if v := in["version"]; v != "" && jobType != "winget_uninstall" {
		if !versionRe.MatchString(v) {
			return nil, "invalid version"
		}
		out["version"] = v
	}
	return out, ""
}

func dispatchJobs(deviceID string) []jobOrder {
	now := time.Now()
	changed := false
	var due []*Job
	for _, j := range jobs {
		if j.DeviceID != deviceID {
			continue
		}
		if j.Status == "running" && now.Sub(j.UpdatedAt) > jobTimeout {
			j.Status = "failed"
			j.Output = "no result from agent before timeout"
			j.UpdatedAt = now
			changed = true
		}
		if j.Status == "pending" {
			due = append(due, j)
		}
	}
	sort.Slice(due, func(a, b int) bool { return due[a].CreatedAt.Before(due[b].CreatedAt) })
	orders := []jobOrder{}
	for _, j := range due {
		j.Status = "running"
		j.UpdatedAt = now
		changed = true
		orders = append(orders, jobOrder{ID: j.ID, Type: j.Type, Params: j.Params})
	}
	if changed {
		save()
	}
	return orders
}

func pruneFinished(deviceID string) {
	var finished []*Job
	for _, j := range jobs {
		if j.DeviceID == deviceID && (j.Status == "done" || j.Status == "failed") {
			finished = append(finished, j)
		}
	}
	if len(finished) <= maxFinishedJobs {
		return
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].CreatedAt.After(finished[b].CreatedAt) })
	for _, j := range finished[maxFinishedJobs:] {
		delete(jobs, j.ID)
	}
}

func registerJobs() {
	http.HandleFunc("POST /admin/devices/{id}/jobs", admin(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Type   string            `json:"type"`
			Params map[string]string `json:"params"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if !jobTypes[req.Type] {
			http.Error(w, "unknown job type", http.StatusBadRequest)
			return
		}
		params, msg := cleanParams(req.Type, req.Params)
		if msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		deviceID := r.PathValue("id")
		mu.Lock()
		defer mu.Unlock()
		if devices[deviceID] == nil {
			http.Error(w, "no such device", http.StatusNotFound)
			return
		}
		open := 0
		for _, j := range jobs {
			if j.DeviceID == deviceID && (j.Status == "pending" || j.Status == "running") {
				open++
			}
		}
		if open >= maxOpenJobs {
			http.Error(w, "too many open jobs for this device", http.StatusTooManyRequests)
			return
		}
		now := time.Now()
		j := &Job{ID: "job_" + randHex(4), DeviceID: deviceID, Type: req.Type, Params: params, Status: "pending", CreatedAt: now, UpdatedAt: now}
		jobs[j.ID] = j
		pruneFinished(deviceID)
		save()
		json.NewEncoder(w).Encode(j)
	}))

	http.HandleFunc("GET /admin/devices/{id}/jobs", admin(func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.PathValue("id")
		mu.Lock()
		list := []*Job{}
		for _, j := range jobs {
			if j.DeviceID == deviceID {
				list = append(list, j)
			}
		}
		mu.Unlock()
		sort.Slice(list, func(a, b int) bool { return list[a].CreatedAt.After(list[b].CreatedAt) })
		if len(list) > 20 {
			list = list[:20]
		}
		json.NewEncoder(w).Encode(list)
	}))

	http.HandleFunc("POST /jobs/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		d := authDevice(r)
		if d == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Status string `json:"status"`
			Output string `json:"output"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Status != "done" && req.Status != "failed" {
			http.Error(w, "bad status", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		j := jobs[r.PathValue("id")]
		if j == nil || j.DeviceID != d.ID {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		if j.Status != "running" {
			http.Error(w, "job is not running", http.StatusConflict)
			return
		}
		out := req.Output
		if len(out) > maxJobOutput {
			out = strings.ToValidUTF8(out[len(out)-maxJobOutput:], "")
		}
		j.Status = req.Status
		j.Output = out
		j.UpdatedAt = time.Now()
		save()
		w.Write([]byte(`{"ok":true}`))
	})
}