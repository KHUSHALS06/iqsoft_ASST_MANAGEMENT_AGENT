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
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// exeDir returns the folder the agent.exe lives in, so files are always
// found no matter where the agent was launched from (double-click, service, etc).
func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// dataDir is where the agent keeps files it WRITES (credentials, usage queue,
// logs): %ProgramData%\IQSoft. This is deliberately separate from exeDir() so
// agent.exe can live in a folder only administrators can write to (e.g.
// C:\\Program Files\\IQSoft) while the agent, which runs as the logged-in user,
// can still save its data. Falls back to the exe folder if ProgramData is unavailable.
func dataDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		return exeDir()
	}
	d := filepath.Join(base, "IQSoft")
	if _, err := os.Stat(d); err != nil {
		if os.MkdirAll(d, 0o755) != nil {
			return exeDir()
		}
		if isElevatedAdmin() { // SYSTEM (service-launched agent) or an elevated admin
			// Let all local users (S-1-5-32-545) write here, so the agent works in every session.
			runHidden("icacls", d, "/grant", "*S-1-5-32-545:(OI)(CI)M")
		}
	}
	return d
}

// migrateOldFiles moves files an earlier version kept next to the exe.
func migrateOldFiles() {
	for _, n := range []string{"agent-creds.json", "usage-pending.json"} {
		oldP, newP := filepath.Join(exeDir(), n), filepath.Join(dataDir(), n)
		if oldP == newP {
			continue
		}
		if _, err := os.Stat(newP); err == nil {
			continue
		}
		if b, err := os.ReadFile(oldP); err == nil && os.WriteFile(newP, b, 0o600) == nil {
			os.Remove(oldP) // best effort: may be refused if the folder is protected
		}
	}
}

// logCrash appends a panic + stack trace to crash.log (next to the exe) so it
// survives a console window closing itself, and is callable from any goroutine.
func logCrash(r any) {
	f, err := os.OpenFile(filepath.Join(dataDir(), "crash.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s panic: %v\n%s\n", time.Now().Format(time.RFC3339), r, debug.Stack())
}

// Default server. Override without rebuilding: -server flag, or a server.txt
// file next to agent.exe containing e.g.  http://192.168.1.5:8080
var serverURL = "http://192.168.0.100:8080"

const (
	inventoryEvery  = 6 * time.Hour   // ADDED: periodic inventory refresh
	usageFlushEvery = 3 * time.Minute // ADDED: upload app usage
	svcName         = "IQSoftEndpointSvc"
)

func credsPath() string { return filepath.Join(dataDir(), "agent-creds.json") }

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
	resp, err := httpClient.Post(serverURL+"/enroll", "application/json", bytes.NewReader(body)) // httpClient has a timeout; http.Post does not
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

func heartbeat(creds *Creds) bool {
	resp, err := doAuthed(creds, "POST", "/heartbeat", []byte("{}"))
	if err != nil {
		log.Println("cannot reach server:", err)
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		log.Println("server no longer knows this device, re-enrolling")
		return false
	}
	if resp.StatusCode != http.StatusOK {
		log.Println("heartbeat:", resp.Status)
		return true
	}
	var hb struct {
		Jobs []Job `json:"jobs"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&hb) != nil {
		return true
	}
	for _, j := range hb.Jobs {
		log.Printf("job %s queued: %s", j.ID, j.Type)
		enqueue(creds, j)
	}
	return true
}

// ---- ADDED helpers --------------------------------------------------------

// safely runs fn, logging a panic to crash.log instead of killing the agent.
func safely(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logCrash(r)
		}
	}()
	fn()
}

// setupLogging: the exe is built with -H=windowsgui, so stderr goes nowhere.
func setupLogging() {
	p := filepath.Join(dataDir(), "agent.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 2<<20 {
		os.Remove(p + ".old")
		os.Rename(p, p+".old")
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		log.SetOutput(f)
	}
}

// server.txt next to the exe overrides the built-in default (the service
// starts agent.exe with no arguments, so a file is the practical way).
func serverFromFile() string {
	b, err := os.ReadFile(filepath.Join(exeDir(), "server.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
}

var procCreateMutexW = kernel32.NewProc("CreateMutexW")

// acquireSingleInstance: one agent per desktop session. Retries briefly
// because the service may be swapping out the previous process.
func acquireSingleInstance() bool {
	name, _ := syscall.UTF16PtrFromString(`Local\IQSoftEndpointAgent`)
	for i := 0; i < 6; i++ {
		h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
		if h != 0 {
			if errno, ok := err.(syscall.Errno); ok && errno == 183 { // ERROR_ALREADY_EXISTS
				syscall.CloseHandle(syscall.Handle(h))
			} else {
				return true // handle kept open for the life of the process
			}
		}
		time.Sleep(time.Second)
	}
	return false
}

func serviceInstalled() bool {
	cmd := exec.Command("sc", "query", svcName)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Run() == nil
}

// ---- ADDED: admin-only uninstall -------------------------------------------

var procMessageBoxW = user32.NewProc("MessageBoxW")

// msgBox shows a message to the user. The agent is built windowless
// (-H=windowsgui), so printing to stdout would be invisible.
func msgBox(text string) {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString("IQSoft Endpoint Agent")
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x40) // MB_ICONINFORMATION
}

// isElevatedAdmin is true only for a process running with an elevated
// administrator token (or SYSTEM). A standard user cannot obtain one without
// entering an administrator's credentials at the UAC prompt, and an admin who
// has not chosen "Run as administrator" does not have one either.
func isElevatedAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }

func runHidden(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Run()
}

// uninstallAgent returns the process exit code. Refuses unless elevated.
func uninstallAgent() int {
	if !isElevatedAdmin() {
		log.Println("uninstall refused: caller is not an elevated administrator")
		msgBox("Only an administrator can uninstall the IQSoft agent.\n\n" +
			"Right-click agent.exe and choose \"Run as administrator\" with the -uninstall option.")
		return 5 // ERROR_ACCESS_DENIED
	}
	log.Println("uninstall: authorised (elevated administrator)")
	// Stop whatever would relaunch or keep the agent alive, then every running copy.
	if serviceInstalled() {
		runHidden("sc", "stop", svcName)
		time.Sleep(4 * time.Second) // let the service stop and kill its child
		runHidden("sc", "delete", svcName)
	}
	removeAutostart()
	if exe, err := os.Executable(); err == nil {
		runHidden("taskkill", "/F", "/IM", filepath.Base(exe), "/FI", fmt.Sprintf("PID ne %d", os.Getpid()))
	}
	time.Sleep(time.Second)
	os.Remove(credsPath())
	os.Remove(usageFilePath())
	log.Println("uninstall: done. Delete this folder to remove the remaining files.")
	msgBox("The IQSoft agent was uninstalled on this PC.\n\nYou can now delete its folder.")
	return 0
}

// ---------------------------------------------------------------------------

func main() {
	defer func() {
		if r := recover(); r != nil {
			logCrash(r)
			log.Fatalf("panic: %v", r)
		}
	}()

	token := flag.String("enroll", "", "optional enrollment token (not needed if the server allows open enrollment)")
	server := flag.String("server", "", "server base URL (default: server.txt, else "+serverURL+")")
	uninstall := flag.Bool("uninstall", false, "uninstall the agent (administrators only: needs an elevated prompt), then exit") // ADDED
	flag.Parse()
	setupLogging()

	switch {
	case *server != "":
		serverURL = *server
	case serverFromFile() != "":
		serverURL = serverFromFile()
	}
	serverURL = strings.TrimRight(serverURL, "/")
	host, _ := os.Hostname()

	if *uninstall {
		os.Exit(uninstallAgent())
	}

	if !acquireSingleInstance() {
		log.Println("another agent is already running in this session - exiting")
		return
	}
	log.Println("agent starting, server:", serverURL)

	// One launcher should own startup. If the SYSTEM service is installed it
	// relaunches the agent itself; a leftover logon task would fight it.
	if serviceInstalled() {
		removeAutostart()
	} else {
		ensureAutostart()
	}

	// Enroll automatically on first run; keep retrying instead of exiting,
	// so the window never just closes if the server is unreachable.
	migrateOldFiles()
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
	startUsageTracker() // ADDED
	go safely(func() {  // ADDED: upload app usage every few minutes
		for range time.Tick(usageFlushEvery) {
			if err := flushUsage(creds); err != nil {
				log.Println("usage upload failed (will retry):", err)
			}
		}
	})
	go safely(func() { // ADDED: refresh inventory periodically
		for range time.Tick(inventoryEvery) {
			sendInventory(creds)
		}
	})
	sendInventory(creds)

	for {
		if !heartbeat(creds) {
			os.Remove(credsPath())
			for {
				c, err := enroll("", host)
				if err != nil {
					log.Println("re-enroll failed, retrying in 10s:", err)
					time.Sleep(10 * time.Second)
					continue
				}
				*creds = *c
				log.Println("enrolled as", creds.DeviceID)
				sendInventory(creds)
				break
			}
		}
		time.Sleep(5 * time.Second)
	}
}