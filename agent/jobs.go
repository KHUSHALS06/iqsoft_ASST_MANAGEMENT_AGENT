package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	wingetTimeout = 20 * time.Minute
	maxOutput     = 4 << 10
)

var (
	httpClient = &http.Client{Timeout: 30 * time.Second}
	jobQueue   = make(chan Job, 64)
	pkgIDRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}$`)
	versionRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,63}$`)
)

type Job struct {
	ID     string            `json:"id"`
	Type   string            `json:"type"`
	Params map[string]string `json:"params"`
}

func doAuthed(creds *Creds, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, serverURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+creds.DeviceID+"."+creds.Secret)
	req.Header.Set("Content-Type", "application/json")
	return httpClient.Do(req)
}

func tail(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if len(s) > maxOutput {
		s = strings.ToValidUTF8(s[len(s)-maxOutput:], "")
	}
	return s
}

func runWinget(action string, params map[string]string) (string, string) {
	id := params["id"]
	if !pkgIDRe.MatchString(id) {
		return "failed", "invalid package id"
	}
	path, err := exec.LookPath("winget.exe")
	if err != nil {
		return "failed", "winget not found for the account the agent runs as"
	}
	args := []string{action, "--id", id, "--exact", "--silent", "--disable-interactivity", "--accept-source-agreements"}
	if action != "uninstall" {
		args = append(args, "--accept-package-agreements")
		if v := params["version"]; v != "" {
			if !versionRe.MatchString(v) {
				return "failed", "invalid version"
			}
			args = append(args, "--version", v)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), wingetTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	text := tail(string(out))
	if ctx.Err() != nil {
		return "failed", "timed out\n" + text
	}
	if err == nil {
		return "done", text
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch uint32(ee.ExitCode()) {
		case 0x8A15002B:
			return "done", "already up to date\n" + text
		case 0x8A150061:
			return "done", "already installed\n" + text
		}
	}
	return "failed", err.Error() + "\n" + text
}

func execute(creds *Creds, j Job) (string, string) {
	switch j.Type {
	case "refresh_inventory":
		if err := sendInventory(creds); err != nil {
			return "failed", err.Error()
		}
		return "done", "inventory refreshed"
	case "winget_install", "winget_upgrade", "winget_uninstall":
		status, out := runWinget(strings.TrimPrefix(j.Type, "winget_"), j.Params)
		if status == "done" {
			if err := sendInventory(creds); err != nil {
				out += "\ninventory refresh failed: " + err.Error()
			}
		}
		return status, out
	}
	return "failed", "unsupported job type: " + j.Type
}

func reportResult(creds *Creds, id, status, output string) {
	body, _ := json.Marshal(map[string]string{"status": status, "output": output})
	for attempt := 1; attempt <= 3; attempt++ {
		resp, err := doAuthed(creds, "POST", "/jobs/"+id+"/result", body)
		if err != nil {
			log.Printf("job %s: result upload failed (attempt %d): %v", id, attempt, err)
			time.Sleep(2 * time.Second)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("job %s: result rejected: %s", id, resp.Status)
			return
		}
		log.Printf("job %s: %s", id, status)
		return
	}
}

func enqueue(creds *Creds, j Job) {
	select {
	case jobQueue <- j:
	default:
		go reportResult(creds, j.ID, "failed", "agent queue full")
	}
}

func jobWorker(creds *Creds) {
	for j := range jobQueue {
		log.Printf("job %s: running %s", j.ID, j.Type)
		status, out := execute(creds, j)
		reportResult(creds, j.ID, status, out)
	}
}