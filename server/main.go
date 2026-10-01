package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var adminKey = func() string {
	if v := os.Getenv("ADMIN_KEY"); v != "" {
		return v
	}
	return "change-me"
}()

const (
	dataFile     = "data.json"
	onlineWindow = 20 * time.Second // agent heartbeats every 5s
)

type Device struct {
	ID         string    `json:"id"`
	Hostname   string    `json:"hostname"`
	SecretHash string    `json:"secret_hash"` // we never store the secret itself
	EnrolledAt time.Time `json:"enrolled_at"`
	LastSeen   time.Time `json:"last_seen"`
}

// All state below is protected by mu.
var (
	mu          sync.Mutex
	tokens      = map[string]bool{}
	devices     = map[string]*Device{}
	inventories = map[string]json.RawMessage{}
)

type persisted struct {
	Tokens      map[string]bool            `json:"tokens"`
	Devices     map[string]*Device         `json:"devices"`
	Inventories map[string]json.RawMessage `json:"inventories"`
	Jobs        map[string]*Job            `json:"jobs"`
}

// save writes everything to data.json. The caller must hold mu.
func save() {
	b, err := json.Marshal(persisted{tokens, devices, inventories, jobs})
	if err != nil {
		log.Println("save failed:", err)
		return
	}
	tmp := dataFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Println("save failed:", err)
		return
	}
	if err := os.Rename(tmp, dataFile); err != nil {
		log.Println("save failed:", err)
	}
}

func load() {
	b, err := os.ReadFile(dataFile)
	if err != nil {
		return // first run: nothing saved yet
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		log.Println("could not read", dataFile, "- starting empty:", err)
		return
	}
	if p.Tokens != nil {
		tokens = p.Tokens
	}
	if p.Devices != nil {
		devices = p.Devices
	}
	if p.Inventories != nil {
		inventories = p.Inventories
	}
	if p.Jobs != nil {
		jobs = p.Jobs
	}
	log.Printf("loaded %d devices from %s", len(devices), dataFile)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// authDevice checks the "Authorization: Bearer <id>.<secret>" header.
func authDevice(r *http.Request) *Device {
	cred := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, secret, ok := strings.Cut(cred, ".")
	if !ok {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	d := devices[id]
	if d == nil || subtle.ConstantTimeCompare([]byte(d.SecretHash), []byte(hashHex(secret))) != 1 {
		return nil
	}
	return d
}

// admin wraps a handler so it only runs with the correct X-Admin-Key header.
func admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(adminKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func main() {
	load()

	// The dashboard page
	http.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})

	// Admin: create a one-time enrollment token
	http.HandleFunc("POST /admin/token", admin(func(w http.ResponseWriter, r *http.Request) {
		tok := "enr_" + randHex(8)
		mu.Lock()
		tokens[tok] = true
		save()
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"token": tok})
	}))

	// Agent: exchange a token for an ID + secret
	http.HandleFunc("POST /enroll", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token    string `json:"token"`
			Hostname string `json:"hostname"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if req.Token != "" { // tokens are optional now (open enrollment); a given token must still be valid
			if !tokens[req.Token] {
				http.Error(w, "invalid or used token", http.StatusForbidden)
				return
			}
			delete(tokens, req.Token) // single use
		}
		secret := randHex(16)
		d := &Device{ID: "dev_" + randHex(4), Hostname: req.Hostname, SecretHash: hashHex(secret), EnrolledAt: time.Now()}
		devices[d.ID] = d
		save()
		log.Printf("enrolled %s (%s)", d.ID, d.Hostname)
		json.NewEncoder(w).Encode(map[string]string{"device_id": d.ID, "secret": secret})
	})

	// Agent: heartbeat (must be authenticated).
	// We don't save to disk here: it happens every 5 seconds per device.
	http.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, r *http.Request) {
		d := authDevice(r)
		if d == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		d.LastSeen = time.Now()
		orders := dispatchJobs(d.ID)
		mu.Unlock()
		log.Printf("heartbeat from %s (%s), %d job(s) dispatched", d.ID, d.Hostname, len(orders))
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "jobs": orders})
	})

	// Admin: list devices with a short summary (never includes secrets)
	http.HandleFunc("GET /admin/devices", admin(func(w http.ResponseWriter, r *http.Request) {
		type view struct {
			ID       string    `json:"id"`
			Hostname string    `json:"hostname"`
			Online   bool      `json:"online"`
			LastSeen time.Time `json:"last_seen"`
			Model    string    `json:"model"`
			OS       string    `json:"os"`
			User     string    `json:"user"`
			Apps     int       `json:"apps"`
		}
		mu.Lock()
		out := []view{}
		for _, d := range devices {
			v := view{ID: d.ID, Hostname: d.Hostname, LastSeen: d.LastSeen, Online: time.Since(d.LastSeen) < onlineWindow}
			if raw, ok := inventories[d.ID]; ok {
				var inv struct {
					Hardware struct {
						Model  string `json:"model"`
						OSName string `json:"os_name"`
					} `json:"hardware"`
					User     string            `json:"logged_in_user"`
					Software []json.RawMessage `json:"software"`
				}
				if json.Unmarshal(raw, &inv) == nil {
					v.Model, v.OS, v.User, v.Apps = inv.Hardware.Model, inv.Hardware.OSName, inv.User, len(inv.Software)
				}
			}
			out = append(out, v)
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(out)
	}))

	registerInventory()
	registerJobs()
	registerRemote()

	log.Println("server listening on :8080  (dashboard: http://localhost:8080)")
	log.Fatal(http.ListenAndServe(":8080", nil))
}