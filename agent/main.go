package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// logCrash appends a panic + stack trace to crash.log so it survives a
// console window closing itself, and is callable from any goroutine (unlike
// a bare recover() in main, which only catches panics on its own goroutine).
func logCrash(r any) {
	f, err := os.OpenFile("crash.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s panic: %v\n%s\n", time.Now().Format(time.RFC3339), r, debug.Stack())
}

var serverURL = "http://192.168.1.5:8080"

const credsFile = "agent-creds.json"

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

func heartbeat(creds *Creds) {
	resp, err := doAuthed(creds, "POST", "/heartbeat", []byte("{}"))
	if err != nil {
		log.Println("cannot reach server:", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Println("heartbeat:", resp.Status)
		return
	}
	var hb struct {
		Jobs []Job `json:"jobs"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&hb) != nil {
		return
	}
	for _, j := range hb.Jobs {
		log.Printf("job %s queued: %s", j.ID, j.Type)
		enqueue(creds, j)
	}
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			logCrash(r)
			log.Fatalf("panic: %v", r)
		}
	}()

	token := flag.String("enroll", "", "one-time enrollment token")
	server := flag.String("server", serverURL, "server base URL")
	flag.Parse()
	serverURL = strings.TrimRight(*server, "/")
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

	go jobWorker(creds)
	sendInventory(creds)

	for {
		heartbeat(creds)
		time.Sleep(5 * time.Second)
	}
}