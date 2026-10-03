package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

func registerInventory() {
	// Agent uploads its inventory
	http.HandleFunc("POST /inventory", func(w http.ResponseWriter, r *http.Request) {
		d := authDevice(r)
		if d == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
		var check struct {
			Software []json.RawMessage `json:"software"`
		}
		if err != nil || json.Unmarshal(body, &check) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		recordHistory(d.ID, inventories[d.ID], body)
		inventories[d.ID] = body
		save()
		mu.Unlock()
		log.Printf("inventory from %s: %d apps", d.ID, len(check.Software))
		w.Write([]byte(`{"ok":true}`))
	})

	// Admin reads a device's full inventory
	http.HandleFunc("GET /admin/devices/{id}/inventory", admin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inv, ok := inventories[r.PathValue("id")]
		mu.Unlock()
		if !ok {
			http.Error(w, "no inventory yet", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(inv)
	}))
}