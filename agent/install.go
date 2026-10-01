package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

const taskName = "IQSoft Endpoint Agent"

// ensureAutostart registers a per-user Scheduled Task that launches this exe
// at logon, inside that user's own desktop session, with no console window
// (the exe itself is built with -H=windowsgui, so no window ever appears).
// Safe to call on every run: it's a no-op once the task already points at
// this exe's current path.
func ensureAutostart() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if taskPointsTo(exe) {
		return
	}
	if err := installTask(exe); err != nil {
		log.Println("could not register startup task:", err)
	} else {
		log.Println("registered startup task:", taskName)
	}
}

func taskPointsTo(exe string) bool {
	out, err := exec.Command("schtasks", "/Query", "/TN", taskName, "/XML").Output()
	if err != nil {
		return false // task missing (or query failed) -> (re)install
	}
	return strings.Contains(string(out), exe)
}

// installTask creates (or replaces) a logon-triggered task for the CURRENT
// user. /IT = run with the interactive token, i.e. inside the user's real
// desktop session - that's what lets screen capture and the remote-control
// indicator work. /RL LIMITED keeps it a standard (non-admin) task, so this
// does not need elevation and does not trigger a UAC prompt.
func installTask(exe string) error {
	return exec.Command("schtasks", "/Create", "/TN", taskName,
		"/TR", fmt.Sprintf(`"%s"`, exe),
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/IT",
		"/F").Run()
}

// removeAutostart deletes the scheduled task. Called by "-uninstall".
func removeAutostart() error {
	return exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").Run()
}