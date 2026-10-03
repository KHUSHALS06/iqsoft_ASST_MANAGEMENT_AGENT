package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	usageSampleEvery = 5 * time.Second
	usageIdleAfter   = 5 * time.Minute
	usageMinSession  = 5 * time.Second
	usageMaxSessions = 2000
)

var (
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetLastInputInfo         = user32.NewProc("GetLastInputInfo")
	procGetTickCount             = kernel32.NewProc("GetTickCount")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageW   = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandleUsage         = kernel32.NewProc("CloseHandle")
)

type UsageSession struct {
	App   string    `json:"app"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type usageData struct {
	Days     map[string]map[string]int `json:"days"`
	Sessions []UsageSession            `json:"sessions"`
}

var (
	usageMu      sync.Mutex
	usagePending = usageData{Days: map[string]map[string]int{}}
	usageCur     string
	usageCurFrom time.Time
	usageCurLast time.Time
)

func usageFilePath() string { return filepath.Join(dataDir(), "usage-pending.json") }

func loadUsagePending() {
	b, err := os.ReadFile(usageFilePath())
	if err != nil {
		return
	}
	var d usageData
	if json.Unmarshal(b, &d) != nil {
		return
	}
	if d.Days == nil {
		d.Days = map[string]map[string]int{}
	}
	usagePending = d
}

func saveUsagePendingLocked() {
	b, err := json.Marshal(usagePending)
	if err != nil {
		return
	}
	tmp := usageFilePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	os.Rename(tmp, usageFilePath())
}

func idleDuration() time.Duration {
	var lii struct {
		Size uint32
		Time uint32
	}
	lii.Size = uint32(unsafe.Sizeof(lii))
	r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii)))
	if r == 0 {
		return 0
	}
	tick, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(tick)-lii.Time) * time.Millisecond
}

func foregroundApp() string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}
	h, _, _ := procOpenProcess.Call(0x1000, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandleUsage.Call(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return ""
	}
	full := syscall.UTF16ToString(buf[:n])
	name := strings.ToLower(filepath.Base(strings.ReplaceAll(full, "\\", "/")))
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "logonui", "lockapp", "":
		return ""
	}
	return name
}

func closeSessionLocked() {
	if usageCur != "" && usageCurLast.Sub(usageCurFrom) >= usageMinSession {
		usagePending.Sessions = append(usagePending.Sessions, UsageSession{App: usageCur, Start: usageCurFrom, End: usageCurLast})
		if len(usagePending.Sessions) > usageMaxSessions {
			usagePending.Sessions = usagePending.Sessions[len(usagePending.Sessions)-usageMaxSessions:]
		}
	}
	usageCur = ""
}

func usageSample(now time.Time) {
	app := ""
	if idleDuration() < usageIdleAfter {
		app = foregroundApp()
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	if app != usageCur {
		closeSessionLocked()
		if app != "" {
			usageCur, usageCurFrom = app, now
		}
	}
	if app != "" {
		usageCurLast = now.Add(usageSampleEvery)
		day := now.Format("2006-01-02")
		m := usagePending.Days[day]
		if m == nil {
			m = map[string]int{}
			usagePending.Days[day] = m
		}
		m[app] += int(usageSampleEvery / time.Second)
	}
}

func startUsageTracker() {
	usageMu.Lock()
	loadUsagePending()
	usageMu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logCrash(r)
			}
		}()
		sample := time.NewTicker(usageSampleEvery)
		save := time.NewTicker(time.Minute)
		for {
			select {
			case now := <-sample.C:
				usageSample(now)
			case <-save.C:
				usageMu.Lock()
				saveUsagePendingLocked()
				usageMu.Unlock()
			}
		}
	}()
}

func flushUsage(creds *Creds) error {
	usageMu.Lock()
	snap := usageData{Days: map[string]map[string]int{}, Sessions: append([]UsageSession(nil), usagePending.Sessions...)}
	for d, m := range usagePending.Days {
		c := make(map[string]int, len(m))
		for k, v := range m {
			c[k] = v
		}
		snap.Days[d] = c
	}
	usageMu.Unlock()
	if len(snap.Days) == 0 && len(snap.Sessions) == 0 {
		return nil
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	resp, err := doAuthed(creds, "POST", "/usage", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("usage upload: %s", resp.Status)
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	for d, m := range snap.Days {
		cur := usagePending.Days[d]
		for k, v := range m {
			if cur[k] -= v; cur[k] <= 0 {
				delete(cur, k)
			}
		}
		if len(cur) == 0 {
			delete(usagePending.Days, d)
		}
	}
	if n := len(snap.Sessions); n <= len(usagePending.Sessions) {
		usagePending.Sessions = append([]UsageSession(nil), usagePending.Sessions[n:]...)
	}
	saveUsagePendingLocked()
	return nil
}