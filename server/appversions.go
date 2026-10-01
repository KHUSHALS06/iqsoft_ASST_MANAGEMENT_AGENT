package main

// App version control
//
//   1. Desired-version config   - one AppPolicy per winget package id
//                                 ("Google.Chrome must be >= 131.0.6778.205").
//   2. Compliance calculation   - compares every device's last inventory with
//                                 those policies. Status is derived from the
//                                 inventory, NOT from job results, so it always
//                                 reflects what is really installed.
//   3. Push to all devices      - queues winget_install (app missing) or
//                                 winget_upgrade (app outdated) jobs, with the
//                                 policy's exact version, on every device that
//                                 needs it. Offline devices keep the job queued
//                                 and run it on their next heartbeat.
//
// All state lives behind the global mu (see main.go) and is persisted in
// data.json together with devices/inventories/jobs.

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------- model --

const (
	matchEquals   = "equals"   // inventory name == policy name (case-insensitive)
	matchPrefix   = "prefix"   // inventory name starts with policy name
	matchContains = "contains" // inventory name contains policy name

	ruleMinimum = "minimum" // installed version must be >= desired
	ruleExact   = "exact"   // installed version must be == desired

	stCompliant = "compliant"
	stOutdated  = "outdated" // installed, but older than desired (or != for exact)
	stAhead     = "ahead"    // exact rule only: installed is NEWER than desired
	stMissing   = "missing"  // not installed
	stUnknown   = "unknown"  // device has not uploaded an inventory yet

	maxPolicies = 200
)

// AppPolicy is the central "desired version" record for one application.
type AppPolicy struct {
	PackageID     string    `json:"package_id"` // winget id, e.g. Google.Chrome (also the key)
	Name          string    `json:"name"`       // text matched against inventory display names, e.g. "Google Chrome"
	MatchType     string    `json:"match_type"` // equals | prefix | contains
	Policy        string    `json:"policy"`     // minimum | exact
	Version       string    `json:"version"`    // desired version; "" = any version is fine
	UpdatedAt     time.Time `json:"updated_at"`
	LastPushAt    time.Time `json:"last_push_at"`
	LastPushCount int       `json:"last_push_count"`
}

// Protected by mu (main.go). Persisted via the `persisted` struct.
var appPolicies = map[string]*AppPolicy{}

var tokenRe = regexp.MustCompile(`[0-9]+|[A-Za-z]+`)

// ---------------------------------------------------- version comparison --

type vtok struct {
	num bool
	s   string // digits without leading zeros, or lower-case letters
}

func tokenize(v string) []vtok {
	v = strings.TrimSpace(v)
	if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		v = v[1:]
	}
	raw := tokenRe.FindAllString(v, -1)
	out := make([]vtok, 0, len(raw))
	for _, r := range raw {
		if r[0] >= '0' && r[0] <= '9' {
			r = strings.TrimLeft(r, "0")
			out = append(out, vtok{true, r}) // "" means zero
		} else {
			out = append(out, vtok{false, strings.ToLower(r)})
		}
	}
	return out
}

func cmpTok(a, b vtok) int {
	switch {
	case a.num && b.num:
		if len(a.s) != len(b.s) { // no leading zeros, so longer = bigger (no overflow issues)
			if len(a.s) < len(b.s) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.s, b.s)
	case a.num && !b.num:
		return 1 // 1.0.0 is newer than 1.0.0-beta
	case !a.num && b.num:
		return -1
	}
	return strings.Compare(a.s, b.s)
}

// compareVersions returns -1, 0 or 1. Missing trailing parts count as zero, so
// "131.0" == "131.0.0.0". Numbers compare numerically ("9" < "10"); a trailing
// word such as "beta" sorts before the plain release.
func compareVersions(a, b string) int {
	ta, tb := tokenize(a), tokenize(b)
	n := len(ta)
	if len(tb) > n {
		n = len(tb)
	}
	zero := vtok{true, ""}
	for i := 0; i < n; i++ {
		x, y := zero, zero
		if i < len(ta) {
			x = ta[i]
		}
		if i < len(tb) {
			y = tb[i]
		}
		if c := cmpTok(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// --------------------------------------------------------- compliance --

type installedApp struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type devSnapshot struct {
	haveInv bool
	apps    []installedApp
}

// snapshotLocked parses every device's inventory once. Caller holds mu.
func snapshotLocked() map[string]devSnapshot {
	snap := make(map[string]devSnapshot, len(devices))
	for id := range devices {
		raw, ok := inventories[id]
		if !ok {
			snap[id] = devSnapshot{}
			continue
		}
		var inv struct {
			Software []installedApp `json:"software"`
		}
		if json.Unmarshal(raw, &inv) != nil {
			snap[id] = devSnapshot{}
			continue
		}
		snap[id] = devSnapshot{haveInv: true, apps: inv.Software}
	}
	return snap
}

func nameMatches(p *AppPolicy, invName string) bool {
	a, b := strings.ToLower(strings.TrimSpace(invName)), strings.ToLower(p.Name)
	switch p.MatchType {
	case matchPrefix:
		return strings.HasPrefix(a, b)
	case matchContains:
		return strings.Contains(a, b)
	}
	return a == b
}

// evaluate returns the compliance status of one device against one policy,
// plus the highest matching installed version (for display).
func evaluate(p *AppPolicy, s devSnapshot) (status, installed string) {
	if !s.haveInv {
		return stUnknown, ""
	}
	found, best, equal := false, "", false
	for _, a := range s.apps {
		if !nameMatches(p, a.Name) {
			continue
		}
		if !found || compareVersions(a.Version, best) > 0 {
			best = a.Version
		}
		found = true
		if p.Version != "" && compareVersions(a.Version, p.Version) == 0 {
			equal = true
		}
	}
	switch {
	case !found:
		return stMissing, ""
	case p.Version == "":
		return stCompliant, best // any version is acceptable
	case equal:
		return stCompliant, best
	}
	c := compareVersions(best, p.Version)
	switch {
	case c < 0:
		return stOutdated, best
	case p.Policy == ruleExact: // c > 0 and no exact match
		return stAhead, best
	}
	return stCompliant, best
}

// openJobFor reports whether a winget job for this package is pending/running
// on the device. Caller holds mu.
func openJobFor(deviceID, pkg string) bool {
	for _, j := range jobs {
		if j.DeviceID != deviceID || !strings.HasPrefix(j.Type, "winget_") {
			continue
		}
		if (j.Status == "pending" || j.Status == "running") && strings.EqualFold(j.Params["id"], pkg) {
			return true
		}
	}
	return false
}

// enqueueJobLocked creates a pending job without saving. Same limits as the
// POST /admin/devices/{id}/jobs handler. Caller holds mu and calls save().
func enqueueJobLocked(deviceID, jobType string, params map[string]string) (*Job, string) {
	open := 0
	for _, j := range jobs {
		if j.DeviceID == deviceID && (j.Status == "pending" || j.Status == "running") {
			open++
		}
	}
	if open >= maxOpenJobs {
		return nil, "too many open jobs for this device"
	}
	now := time.Now()
	j := &Job{ID: "job_" + randHex(4), DeviceID: deviceID, Type: jobType, Params: params, Status: "pending", CreatedAt: now, UpdatedAt: now}
	jobs[j.ID] = j
	pruneFinished(deviceID)
	return j, ""
}

func isOnline(d *Device) bool { return time.Since(d.LastSeen) < onlineWindow }

func sortedPolicies() []*AppPolicy {
	list := make([]*AppPolicy, 0, len(appPolicies))
	for _, p := range appPolicies {
		list = append(list, p)
	}
	sort.Slice(list, func(a, b int) bool {
		return strings.ToLower(list[a].PackageID) < strings.ToLower(list[b].PackageID)
	})
	return list
}

func findPolicyKey(pkg string) (string, bool) {
	if _, ok := appPolicies[pkg]; ok {
		return pkg, true
	}
	for k := range appPolicies {
		if strings.EqualFold(k, pkg) {
			return k, true
		}
	}
	return "", false
}

func cleanPolicy(in AppPolicy) (*AppPolicy, string) {
	in.PackageID = strings.TrimSpace(in.PackageID)
	if !packageIDRe.MatchString(in.PackageID) {
		return nil, "invalid package id"
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 128 || !utf8.ValidString(in.Name) {
		return nil, "name is required (the application name as shown in Installed software, max 128 chars)"
	}
	switch in.MatchType {
	case "":
		in.MatchType = matchEquals
	case matchEquals, matchPrefix, matchContains:
	default:
		return nil, "match_type must be equals, prefix or contains"
	}
	switch in.Policy {
	case "":
		in.Policy = ruleMinimum
	case ruleMinimum, ruleExact:
	default:
		return nil, "policy must be minimum or exact"
	}
	in.Version = strings.TrimSpace(in.Version)
	if in.Version != "" && !versionRe.MatchString(in.Version) {
		return nil, "invalid version"
	}
	return &AppPolicy{
		PackageID: in.PackageID, Name: in.Name, MatchType: in.MatchType,
		Policy: in.Policy, Version: in.Version,
	}, ""
}

// ----------------------------------------------------------- HTTP API --

type policyView struct {
	*AppPolicy
	Total     int `json:"total"`
	Compliant int `json:"compliant"`
	Outdated  int `json:"outdated"`
	Missing   int `json:"missing"`
	Ahead     int `json:"ahead"`
	Unknown   int `json:"unknown"`
}

type deviceStatus struct {
	DeviceID  string `json:"device_id"`
	Hostname  string `json:"hostname"`
	Online    bool   `json:"online"`
	Status    string `json:"status"`
	Installed string `json:"installed"`
	Queued    bool   `json:"queued"` // an install/upgrade job for this app is already open
}

func registerAppVersions() {
	// List all policies with fleet-wide compliance counts.
	http.HandleFunc("GET /admin/app-policies", admin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		snap := snapshotLocked()
		out := []policyView{}
		for _, p := range sortedPolicies() {
			v := policyView{AppPolicy: p, Total: len(devices)}
			for id := range devices {
				st, _ := evaluate(p, snap[id])
				switch st {
				case stCompliant:
					v.Compliant++
				case stOutdated:
					v.Outdated++
				case stMissing:
					v.Missing++
				case stAhead:
					v.Ahead++
				default:
					v.Unknown++
				}
			}
			out = append(out, v)
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(out)
	}))

	// Create or update a policy (key = package id, case-insensitive).
	http.HandleFunc("POST /admin/app-policies", admin(func(w http.ResponseWriter, r *http.Request) {
		var req AppPolicy
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		p, msg := cleanPolicy(req)
		if msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		p.UpdatedAt = time.Now()
		mu.Lock()
		defer mu.Unlock()
		if key, ok := findPolicyKey(p.PackageID); ok {
			old := appPolicies[key]
			p.LastPushAt, p.LastPushCount = old.LastPushAt, old.LastPushCount
			delete(appPolicies, key)
		} else if len(appPolicies) >= maxPolicies {
			http.Error(w, "too many app policies", http.StatusTooManyRequests)
			return
		}
		appPolicies[p.PackageID] = p
		save()
		json.NewEncoder(w).Encode(p)
	}))

	http.HandleFunc("DELETE /admin/app-policies/{pkg}", admin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key, ok := findPolicyKey(r.PathValue("pkg"))
		if !ok {
			http.Error(w, "no such policy", http.StatusNotFound)
			return
		}
		delete(appPolicies, key)
		save()
		w.Write([]byte(`{"ok":true}`))
	}))

	// Per-device compliance for one policy.
	http.HandleFunc("GET /admin/app-policies/{pkg}/compliance", admin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key, ok := findPolicyKey(r.PathValue("pkg"))
		if !ok {
			http.Error(w, "no such policy", http.StatusNotFound)
			return
		}
		p, snap := appPolicies[key], snapshotLocked()
		out := []deviceStatus{}
		for id, d := range devices {
			st, inst := evaluate(p, snap[id])
			out = append(out, deviceStatus{id, d.Hostname, isOnline(d), st, inst, openJobFor(id, p.PackageID)})
		}
		sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Hostname) < strings.ToLower(out[b].Hostname) })
		json.NewEncoder(w).Encode(out)
	}))

	// All policies evaluated for one device.
	http.HandleFunc("GET /admin/devices/{id}/compliance", admin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		id := r.PathValue("id")
		if devices[id] == nil {
			http.Error(w, "no such device", http.StatusNotFound)
			return
		}
		snap := snapshotLocked()
		type item struct {
			PackageID string `json:"package_id"`
			Name      string `json:"name"`
			Policy    string `json:"policy"`
			Desired   string `json:"desired"`
			Installed string `json:"installed"`
			Status    string `json:"status"`
			Queued    bool   `json:"queued"`
		}
		out := []item{}
		for _, p := range sortedPolicies() {
			st, inst := evaluate(p, snap[id])
			out = append(out, item{p.PackageID, p.Name, p.Policy, p.Version, inst, st, openJobFor(id, p.PackageID)})
		}
		json.NewEncoder(w).Encode(out)
	}))

	// Push the policy's version to devices. Body (all optional):
	//   {"device_ids": ["dev_..."], "online_only": false}
	// No device_ids = every enrolled device. Devices that are already
	// compliant, newer than desired, without inventory, or that already have
	// the same install/upgrade queued are skipped (and reported).
	http.HandleFunc("POST /admin/app-policies/{pkg}/push", admin(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DeviceIDs  []string `json:"device_ids"`
			OnlineOnly bool     `json:"online_only"`
		}
		if r.ContentLength != 0 {
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		key, ok := findPolicyKey(r.PathValue("pkg"))
		if !ok {
			http.Error(w, "no such policy", http.StatusNotFound)
			return
		}
		p, snap := appPolicies[key], snapshotLocked()

		var targets []string
		type skip struct {
			DeviceID string `json:"device_id"`
			Hostname string `json:"hostname"`
			Reason   string `json:"reason"`
		}
		skipped := []skip{}
		if len(req.DeviceIDs) == 0 {
			for id := range devices {
				targets = append(targets, id)
			}
		} else {
			seen := map[string]bool{}
			for _, id := range req.DeviceIDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				if devices[id] == nil {
					skipped = append(skipped, skip{id, "", "no such device"})
					continue
				}
				targets = append(targets, id)
			}
		}
		sort.Slice(targets, func(a, b int) bool {
			return strings.ToLower(devices[targets[a]].Hostname) < strings.ToLower(devices[targets[b]].Hostname)
		})

		jobIDs := []string{}
		for _, id := range targets {
			d := devices[id]
			no := func(reason string) { skipped = append(skipped, skip{id, d.Hostname, reason}) }
			st, _ := evaluate(p, snap[id])
			switch st {
			case stCompliant:
				no("already compliant")
				continue
			case stAhead:
				no("newer version installed (downgrades are not pushed)")
				continue
			case stUnknown:
				no("no inventory yet - refresh the device's inventory first")
				continue
			}
			if req.OnlineOnly && !isOnline(d) {
				no("offline")
				continue
			}
			if openJobFor(id, p.PackageID) {
				no("install/upgrade already queued")
				continue
			}
			jobType := "winget_upgrade"
			if st == stMissing {
				jobType = "winget_install"
			}
			params := map[string]string{"id": p.PackageID}
			if p.Version != "" {
				params["version"] = p.Version
			}
			j, reason := enqueueJobLocked(id, jobType, params)
			if j == nil {
				no(reason)
				continue
			}
			jobIDs = append(jobIDs, j.ID)
		}
		if len(jobIDs) > 0 {
			p.LastPushAt, p.LastPushCount = time.Now(), len(jobIDs)
			save()
			log.Printf("push %s %s: %d job(s) queued, %d skipped", p.PackageID, p.Version, len(jobIDs), len(skipped))
		}
		json.NewEncoder(w).Encode(map[string]any{"queued": len(jobIDs), "job_ids": jobIDs, "skipped": skipped})
	}))
}