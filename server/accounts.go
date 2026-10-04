package main

// accounts.go - fleet-wide view of the licence + signed-in-account data that each
// agent reports inside its inventory (agent/accounts.go).
//
//	GET /admin/accounts   -> {"summary":[...], "licenses":[...], "accounts":[...]}
//
// summary counts devices per vendor/product/tier, e.g. "Microsoft 365 Apps for
// business: Premium on 12 devices, Unlicensed on 2". Nothing is stored separately:
// it is computed from the latest inventory of every device, so it is always current.
// Remember this is what the DEVICE can see. Who pays, and the Adobe account, are
// not visible locally - see agent/accounts.go.

import (
	"encoding/json"
	"net/http"
	"sort"
)

type fleetAccount struct {
	DeviceID string `json:"device_id"`
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	App      string `json:"app"`
	Account  string `json:"account"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Plan     string `json:"plan,omitempty"`
	SeenAt   string `json:"seen_at,omitempty"`
	Stale    bool   `json:"stale,omitempty"`
}

type fleetLicense struct {
	DeviceID string `json:"device_id"`
	Hostname string `json:"hostname"`
	Vendor   string `json:"vendor"`
	Product  string `json:"product"`
	Version  string `json:"version,omitempty"`
	Edition  string `json:"edition,omitempty"`
	Type     string `json:"type"`
	Tier     string `json:"tier"`
	Status   string `json:"status,omitempty"`
	Basis    string `json:"basis,omitempty"`
}

type licenseCount struct {
	Vendor  string `json:"vendor"`
	Product string `json:"product"`
	Tier    string `json:"tier"`
	Devices int    `json:"devices"`
}

func registerAccounts() {
	http.HandleFunc("GET /admin/accounts", admin(func(w http.ResponseWriter, r *http.Request) {
		out := struct {
			Summary  []licenseCount `json:"summary"`
			Licenses []fleetLicense `json:"licenses"`
			Accounts []fleetAccount `json:"accounts"`
		}{Summary: []licenseCount{}, Licenses: []fleetLicense{}, Accounts: []fleetAccount{}}

		counts := map[[3]string]int{}
		mu.Lock()
		for id, d := range devices {
			raw, ok := inventories[id]
			if !ok {
				continue
			}
			var inv struct {
				Accounts []fleetAccount `json:"accounts"`
				Licenses []fleetLicense `json:"licenses"`
			}
			if json.Unmarshal(raw, &inv) != nil {
				continue
			}
			for _, a := range inv.Accounts {
				a.DeviceID, a.Hostname = id, d.Hostname
				out.Accounts = append(out.Accounts, a)
			}
			for _, l := range inv.Licenses {
				l.DeviceID, l.Hostname = id, d.Hostname
				out.Licenses = append(out.Licenses, l)
				counts[[3]string{l.Vendor, l.Product, l.Tier}]++
			}
		}
		mu.Unlock()

		for k, n := range counts {
			out.Summary = append(out.Summary, licenseCount{Vendor: k[0], Product: k[1], Tier: k[2], Devices: n})
		}
		sort.Slice(out.Summary, func(i, j int) bool {
			a, b := out.Summary[i], out.Summary[j]
			if a.Vendor != b.Vendor {
				return a.Vendor < b.Vendor
			}
			if a.Product != b.Product {
				return a.Product < b.Product
			}
			return a.Tier < b.Tier
		})
		sort.Slice(out.Licenses, func(i, j int) bool {
			a, b := out.Licenses[i], out.Licenses[j]
			if a.Hostname != b.Hostname {
				return a.Hostname < b.Hostname
			}
			return a.Product < b.Product
		})
		sort.Slice(out.Accounts, func(i, j int) bool {
			a, b := out.Accounts[i], out.Accounts[j]
			if a.Hostname != b.Hostname {
				return a.Hostname < b.Hostname
			}
			if a.App != b.App {
				return a.App < b.App
			}
			return a.Account < b.Account
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
}