package main

// restrictions.go - decides WHICH PCs get install/uninstall blocking, keeps
// them in that state, and exposes it to the dashboard.
//
// Model
//   - A DEFAULT policy applies to every device that has no policy of its own.
//   - A DEVICE policy overrides the default for one PC.
//   - "No restrictions" is an explicit policy (all three switches off). Setting
//     it sends clear_restrictions to every PC that was previously restricted.
//   - Nothing is sent to a PC that has never been given a policy.
//
// Keeping PCs in line (reconcile loop, every restrictTick)
//   - Only ONLINE devices are touched, so no stale jobs pile up for offline PCs.
//   - A job is queued when the wanted policy differs from what we last asked
//     for, when the last job failed (retried after restrictRetryAfter), every
//     restrictReapplyEvery (picks up apps installed since, and repairs a PC
//     where the agent crashed while the installer block was lifted), and when
//     a PC comes back online (an agent restart).
//   - At most one restriction job is open per device.
//
// The agent does not report its restriction state yet, so "applied" here means
// "the last apply/clear job finished OK". It is stored in restrictions.json
// next to data.json.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	restrictionsFile     = "restrictions.json"
	restrictTick         = 15 * time.Second
	restrictReapplyEvery = 6 * time.Hour
	restrictRetryAfter   = 30 * time.Minute
	restrictMinGap       = 10 * time.Minute
)

type RestrictPolicy struct {
	BlockMSI       bool `json:"block_msi"`
	BlockUninstall bool `json:"block_uninstall"`
	BlockUserExe   bool `json:"block_user_exe"`
}

func (p RestrictPolicy) empty() bool { return p == RestrictPolicy{} }

func (p RestrictPolicy) params() map[string]string {
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		"block_msi":       b(p.BlockMSI),
		"block_uninstall": b(p.BlockUninstall),
		"block_user_exe":  b(p.BlockUserExe),
	}
}

// restrictRec is what we last asked one device to do, and how it went.
type restrictRec struct {
	Policy    RestrictPolicy `json:"policy"`
	JobID     string         `json:"job_id"`
	Status    string         `json:"status"` // queued | done | failed
	Detail    string         `json:"detail,omitempty"`
	QueuedAt  time.Time      `json:"queued_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	AppliedAt time.Time      `json:"applied_at"` // last time a job finished OK
	Due       bool           `json:"due,omitempty"`
}

type restrictStore struct {
	Default *RestrictPolicy           `json:"default"`
	Devices map[string]RestrictPolicy `json:"devices"`
	Recs    map[string]*restrictRec   `json:"recs"`
}

// All of this is protected by mu (main.go), like every other piece of state.
var (
	restrictions = restrictStore{Devices: map[string]RestrictPolicy{}, Recs: map[string]*restrictRec{}}
	wasOnline    = map[string]bool{} // in memory only: used to spot "came back online"
)

// saveRestrictionsLocked writes restrictions.json. The caller holds mu.
func saveRestrictionsLocked() {
	b, err := json.Marshal(restrictions)
	if err != nil {
		log.Println("save restrictions failed:", err)
		return
	}
	tmp := restrictionsFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Println("save restrictions failed:", err)
		return
	}
	if err := os.Rename(tmp, restrictionsFile); err != nil {
		log.Println("save restrictions failed:", err)
	}
}

func loadRestrictions() {
	b, err := os.ReadFile(restrictionsFile)
	if err != nil {
		return // first run
	}
	var s restrictStore
	if err := json.Unmarshal(b, &s); err != nil {
		log.Println("could not read", restrictionsFile, "- starting empty:", err)
		return
	}
	if s.Devices == nil {
		s.Devices = map[string]RestrictPolicy{}
	}
	if s.Recs == nil {
		s.Recs = map[string]*restrictRec{}
	}
	restrictions = s
	log.Printf("loaded restriction settings: default=%v, %d device override(s)", s.Default != nil, len(s.Devices))
}

// effectiveLocked returns the policy that applies to a device and where it
// came from: "device", "default" or "none" (never configured).
func effectiveLocked(deviceID string) (RestrictPolicy, string) {
	if p, ok := restrictions.Devices[deviceID]; ok {
		return p, "device"
	}
	if restrictions.Default != nil {
		return *restrictions.Default, "default"
	}
	return RestrictPolicy{}, "none"
}

func openRestrictJobLocked(deviceID string) bool {
	for _, j := range jobs {
		if j.DeviceID != deviceID || (j.Type != "apply_restrictions" && j.Type != "clear_restrictions") {
			continue
		}
		if j.Status == "pending" || j.Status == "running" {
			return true
		}
	}
	return false
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-n:], "")
}

// syncRecLocked copies the outcome of a finished job into the record.
func syncRecLocked(rec *restrictRec, now time.Time) bool {
	if rec.Status != "queued" {
		return false
	}
	j := jobs[rec.JobID]
	if j == nil {
		rec.Status, rec.Detail, rec.UpdatedAt = "failed", "job record lost before a result arrived", now
		return true
	}
	switch j.Status {
	case "done":
		rec.Status, rec.Detail, rec.UpdatedAt, rec.AppliedAt = "done", lastN(j.Output, 1000), j.UpdatedAt, j.UpdatedAt
		return true
	case "failed":
		rec.Status, rec.Detail, rec.UpdatedAt = "failed", lastN(j.Output, 1000), j.UpdatedAt
		return true
	}
	return false
}

// reconcileLocked queues whatever jobs are needed to bring online devices in
// line with their wanted policy. It reports whether anything changed (the
// caller then saves). The caller holds mu.
func reconcileLocked(now time.Time) bool {
	changed := false
	for id, d := range devices {
		online := isOnline(d)
		prevOnline, seen := wasOnline[id]
		wasOnline[id] = online
		cameBack := online && seen && !prevOnline

		rec := restrictions.Recs[id]
		if rec != nil && syncRecLocked(rec, now) {
			changed = true
		}

		want, source := effectiveLocked(id)
		if source == "none" {
			want = RestrictPolicy{} // never configured: only matters if we restricted it earlier
		}
		if !online || openRestrictJobLocked(id) {
			continue
		}

		need := false
		switch {
		case rec == nil:
			need = !want.empty()
		case rec.Policy != want:
			need = true
		case want.empty():
			need = rec.Status == "failed" && now.Sub(rec.UpdatedAt) >= restrictRetryAfter
		case rec.Status == "failed":
			need = now.Sub(rec.UpdatedAt) >= restrictRetryAfter
		case rec.Status == "done":
			need = now.Sub(rec.AppliedAt) >= restrictReapplyEvery ||
				(cameBack && now.Sub(rec.UpdatedAt) >= restrictMinGap)
		}
		if rec != nil && rec.Due {
			if !(want.empty() && rec.Policy.empty()) { // nothing to clear on a never-restricted PC
				need = true
			}
			rec.Due = false
			changed = true
		}
		if !need {
			continue
		}

		jobType := "apply_restrictions"
		params := want.params()
		if want.empty() {
			jobType, params = "clear_restrictions", map[string]string{}
		}
		j, reason := enqueueJobLocked(id, jobType, params)
		if j == nil {
			log.Printf("restrictions: could not queue %s for %s: %s", jobType, d.Hostname, reason)
			continue
		}
		next := &restrictRec{Policy: want, JobID: j.ID, Status: "queued", QueuedAt: now, UpdatedAt: now}
		if rec != nil {
			next.AppliedAt = rec.AppliedAt
		}
		restrictions.Recs[id] = next
		log.Printf("restrictions: queued %s for %s (%+v)", jobType, d.Hostname, want)
		changed = true
	}
	return changed
}

// markDueLocked makes the next reconcile re-send the policy to this device
// even if nothing changed (so POSTing the same policy again acts as "apply now").
func markDueLocked(deviceID string) {
	if rec := restrictions.Recs[deviceID]; rec != nil {
		rec.Due = true
	}
}

type restrictRow struct {
	DeviceID string         `json:"device_id"`
	Hostname string         `json:"hostname"`
	Online   bool           `json:"online"`
	Source   string         `json:"source"` // device | default | none
	Policy   RestrictPolicy `json:"policy"`
	Applied  *restrictRec   `json:"applied"` // null: nothing sent yet
}

func rowLocked(d *Device) restrictRow {
	p, src := effectiveLocked(d.ID)
	var rec *restrictRec
	if r := restrictions.Recs[d.ID]; r != nil {
		c := *r
		rec = &c
	}
	return restrictRow{DeviceID: d.ID, Hostname: d.Hostname, Online: isOnline(d), Source: src, Policy: p, Applied: rec}
}

type restrictBody struct {
	BlockMSI       bool `json:"block_msi"`
	BlockUninstall bool `json:"block_uninstall"`
	BlockUserExe   bool `json:"block_user_exe"`
	Inherit        bool `json:"inherit"` // device endpoint only: drop this PC's own policy
}

func (b restrictBody) policy() RestrictPolicy {
	return RestrictPolicy{BlockMSI: b.BlockMSI, BlockUninstall: b.BlockUninstall, BlockUserExe: b.BlockUserExe}
}

func decodeRestrictBody(w http.ResponseWriter, r *http.Request) (restrictBody, bool) {
	var b restrictBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return b, false
	}
	return b, true
}

func registerRestrictions() {
	loadRestrictions()

	go func() {
		for range time.Tick(restrictTick) {
			mu.Lock()
			if reconcileLocked(time.Now()) {
				save()
				saveRestrictionsLocked()
			}
			mu.Unlock()
		}
	}()

	// Overview for the dashboard: the default plus one row per device.
	http.HandleFunc("GET /admin/restrictions", admin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rows := []restrictRow{}
		for _, d := range devices {
			rows = append(rows, rowLocked(d))
		}
		def := restrictions.Default
		mu.Unlock()
		sort.Slice(rows, func(a, b int) bool {
			return strings.ToLower(rows[a].Hostname) < strings.ToLower(rows[b].Hostname)
		})
		json.NewEncoder(w).Encode(map[string]any{"default": def, "devices": rows})
	}))

	http.HandleFunc("GET /admin/devices/{id}/restrictions", admin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		d := devices[r.PathValue("id")]
		if d == nil {
			mu.Unlock()
			http.Error(w, "no such device", http.StatusNotFound)
			return
		}
		row := rowLocked(d)
		mu.Unlock()
		json.NewEncoder(w).Encode(row)
	}))

	// Set the default policy for every PC that has no policy of its own.
	// All switches off = remove restrictions from the PCs that had them.
	http.HandleFunc("POST /admin/restrictions/default", admin(func(w http.ResponseWriter, r *http.Request) {
		b, ok := decodeRestrictBody(w, r)
		if !ok {
			return
		}
		p := b.policy()
		mu.Lock()
		defer mu.Unlock()
		restrictions.Default = &p
		for id := range devices {
			if _, own := restrictions.Devices[id]; !own {
				markDueLocked(id)
			}
		}
		reconcileLocked(time.Now())
		save()
		saveRestrictionsLocked()
		log.Printf("restrictions: default policy set to %+v", p)
		json.NewEncoder(w).Encode(map[string]any{"default": p})
	}))

	// Set one PC's own policy, or {"inherit":true} to fall back to the default.
	http.HandleFunc("POST /admin/devices/{id}/restrictions", admin(func(w http.ResponseWriter, r *http.Request) {
		b, ok := decodeRestrictBody(w, r)
		if !ok {
			return
		}
		id := r.PathValue("id")
		mu.Lock()
		defer mu.Unlock()
		d := devices[id]
		if d == nil {
			http.Error(w, "no such device", http.StatusNotFound)
			return
		}
		if b.Inherit {
			delete(restrictions.Devices, id)
		} else {
			restrictions.Devices[id] = b.policy()
		}
		markDueLocked(id)
		reconcileLocked(time.Now())
		save()
		saveRestrictionsLocked()
		log.Printf("restrictions: %s set to %+v (inherit=%v)", d.Hostname, b.policy(), b.Inherit)
		json.NewEncoder(w).Encode(rowLocked(d))
	}))
}