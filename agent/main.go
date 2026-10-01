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
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// exeDir returns the folder the agent.exe lives in, so files are always
// found no matter where the agent was launched from (double-click, service, etc).
func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// logCrash appends a panic + stack trace to crash.log (next to the exe) so it
// survives a console window closing itself, and is callable from any goroutine.
func logCrash(r any) {
	f, err := os.OpenFile(filepath.Join(exeDir(), "crash.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s panic: %v\n%s\n", time.Now().Format(time.RFC3339), r, debug.Stack())
}

var serverURL = "http://192.168.1.5:8080"

func credsPath() string { return filepath.Join(exeDir(), "agent-creds.json") }

type Creds struct {
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

func loadCreds() (*Creds, error) {
	b, err := os.ReadFile(credsPath())
	if err != nil {
		return nil, err
	}
	var c Creds
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.DeviceID == "" || c.Secret == "" {
		return nil, fmt.Errorf("creds file is empty")
	}
	return &c, nil
}

// enroll registers this PC with the server. The token is optional: an empty
// token means "open enrollment" (server must allow it).
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
	return &c, os.WriteFile(credsPath(), b, 0o600)
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

	token := flag.String("enroll", "", "optional enrollment token (not needed if the server allows open enrollment)")
	server := flag.String("server", serverURL, "server base URL")
	flag.Parse()
	serverURL = strings.TrimRight(*server, "/")
	host, _ := os.Hostname()

	// Enroll automatically on first run; keep retrying instead of exiting,
	// so the window never just closes if the server is unreachable.
	creds, err := loadCreds()
	for err != nil {
		log.Println("not enrolled yet, contacting", serverURL)
		creds, err = enroll(*token, host)
		if err != nil {
			log.Println("enroll failed, retrying in 10s:", err)
			time.Sleep(10 * time.Second)
			continue
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