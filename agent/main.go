package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

const (
	serverURL = "http://localhost:8080"
	credsFile = "agent-creds.json"
)

type Creds struct {
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

func loadCreds() (*Creds, error) {
	b, err := os.ReadFile(credsFile)
	if err != nil {
		return nil, err
	}
	var c Creds
	return &c, json.Unmarshal(b, &c)
}

func enroll(token, host string) (*Creds, error) {
	body, _ := json.Marshal(map[string]string{"token": token, "hostname": host})
	resp, err := http.Post(serverURL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server said: %s", resp.Status)
	}
	var c Creds
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return &c, os.WriteFile(credsFile, b, 0o600)
}

func main() {
	token := flag.String("enroll", "", "one-time enrollment token")
	flag.Parse()
	host, _ := os.Hostname()

	creds, err := loadCreds()
	if err != nil {
		if *token == "" {
			log.Fatal("not enrolled yet. Run: go run ./agent -enroll <TOKEN>")
		}
		creds, err = enroll(*token, host)
		if err != nil {
			log.Fatal("enroll failed: ", err)
		}
		log.Println("enrolled as", creds.DeviceID)
	}

	sendInventory(creds) // <-- Step 3: collects and uploads inventory (from inventory.go)

	for {
		req, _ := http.NewRequest("POST", serverURL+"/heartbeat", bytes.NewReader([]byte("{}")))
		req.Header.Set("Authorization", "Bearer "+creds.DeviceID+"."+creds.Secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Println("cannot reach server:", err)
		} else {
			log.Println("heartbeat:", resp.Status)
			resp.Body.Close()
		}
		time.Sleep(5 * time.Second)
	}
}