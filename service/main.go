// Package main is the IQSoft Endpoint Service: a Windows service running as
// SYSTEM whose only job is to keep agent.exe alive on whatever desktop is
// currently on screen - the sign-in screen, the lock screen, or a logged-in
// user's own desktop - and relaunch it whenever that changes.
//
// Why this is needed: Windows isolates services in "session 0", separate
// from the session shown on the monitor (session isolation, since Vista). A
// service cannot just start agent.exe normally and expect it to appear on
// screen - it has to explicitly hand the new process a target session and
// desktop. That's what this file does, using CreateProcessAsUserW.
//
// Known limit: this gets you the sign-in screen and the lock screen. It does
// NOT get you into UAC consent prompts - those run on a further-locked-down
// "secure desktop" that only Winlogon itself is allowed to draw into, even
// for SYSTEM processes. No commercial RMM tool can click a UAC prompt either;
// that's a Windows security boundary, not a bug in this code.
package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	svcName    = "IQSoftEndpointSvc"
	svcDisplay = "IQSoft Endpoint Management Service"
)

// ---- Win32 bindings -------------------------------------------------
// Hand-rolled via syscall.NewLazyDLL, same approach already used in
// agent/remote_windows.go, so this file has no extra DLL dependencies.

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	wtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")
	userenv  = syscall.NewLazyDLL("userenv.dll")

	procGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	procGetCurrentProcess         = kernel32.NewProc("GetCurrentProcess")
	procCloseHandle               = kernel32.NewProc("CloseHandle")
	procTerminateProcess          = kernel32.NewProc("TerminateProcess")
	procGetExitCodeProcess        = kernel32.NewProc("GetExitCodeProcess")

	procWTSQueryUserToken = wtsapi32.NewProc("WTSQueryUserToken")

	procOpenProcessToken     = advapi32.NewProc("OpenProcessToken")
	procDuplicateTokenEx     = advapi32.NewProc("DuplicateTokenEx")
	procSetTokenInformation  = advapi32.NewProc("SetTokenInformation")
	procCreateProcessAsUserW = advapi32.NewProc("CreateProcessAsUserW")

	procCreateEnvironmentBlock  = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock = userenv.NewProc("DestroyEnvironmentBlock")
)

const (
	tokenDuplicate         = 0x0002
	tokenQuery             = 0x0008
	tokenAdjustDefault     = 0x0080
	tokenAdjustSessionID   = 0x0100
	tokenPrimary           = 1
	securityImpersonation  = 2
	tokenInfoClassSession  = 12 // TokenSessionId
	createUnicodeEnv       = 0x00000400
	createNoWindow         = 0x08000000
	stillActive            = 259
)

// STARTUPINFOW / PROCESS_INFORMATION - field order matches the real Win32
// structs; Go's own alignment rules reproduce the correct C layout as long
// as the field order and sizes are right (same principle used for the
// SendInput structs in agent/remote_windows.go).
type startupInfo struct {
	Cb              uint32
	lpReserved      *uint16
	LpDesktop       *uint16
	lpTitle         *uint16
	dwX, dwY        uint32
	dwXSize         uint32
	dwYSize         uint32
	dwXCountChars   uint32
	dwYCountChars   uint32
	dwFillAttribute uint32
	dwFlags         uint32
	wShowWindow     uint16
	cbReserved2     uint16
	lpReserved2     uintptr
	hStdInput       uintptr
	hStdOutput      uintptr
	hStdError       uintptr
}

type processInformation struct {
	hProcess    uintptr
	hThread     uintptr
	dwProcessId uint32
	dwThreadId  uint32
}

func utf16ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

// activeSessionID returns the session ID currently shown on the physical
// monitor - this is "what's on screen right now", not "who is logged in".
// It changes as the machine moves between the sign-in screen, a locked
// session, and a logged-in desktop.
func activeSessionID() uint32 {
	r, _, _ := procGetActiveConsoleSessionId.Call()
	return uint32(r)
}

// buildUserToken impersonates the user logged into sessionID, if any.
// Returns ok=false (not an error) when nobody is logged in there yet - that
// is the normal, expected case at the sign-in and lock screens.
func buildUserToken(sessionID uint32) (syscall.Handle, bool) {
	var userToken syscall.Handle
	r, _, _ := procWTSQueryUserToken.Call(uintptr(sessionID), uintptr(unsafe.Pointer(&userToken)))
	if r == 0 {
		return 0, false
	}
	defer procCloseHandle.Call(uintptr(userToken))

	var dup syscall.Handle
	ok, _, err := procDuplicateTokenEx.Call(
		uintptr(userToken), 0, 0,
		uintptr(securityImpersonation), uintptr(tokenPrimary),
		uintptr(unsafe.Pointer(&dup)),
	)
	if ok == 0 {
		log.Println("DuplicateTokenEx (user):", err)
		return 0, false
	}
	return dup, true
}

// buildSystemTokenForSession duplicates THIS service's own SYSTEM token and
// retargets it at sessionID via SetTokenInformation(TokenSessionId). This is
// what lets a session-0 service put a window on the sign-in/lock screen,
// which lives in a different session and has nobody logged in yet.
func buildSystemTokenForSession(sessionID uint32) (syscall.Handle, error) {
	self, _, _ := procGetCurrentProcess.Call()
	var procToken syscall.Handle
	access := uint32(tokenDuplicate | tokenQuery | tokenAdjustDefault | tokenAdjustSessionID)
	ok, _, err := procOpenProcessToken.Call(self, uintptr(access), uintptr(unsafe.Pointer(&procToken)))
	if ok == 0 {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer procCloseHandle.Call(uintptr(procToken))

	var dup syscall.Handle
	ok, _, err = procDuplicateTokenEx.Call(
		uintptr(procToken), 0, 0,
		uintptr(securityImpersonation), uintptr(tokenPrimary),
		uintptr(unsafe.Pointer(&dup)),
	)
	if ok == 0 {
		return 0, fmt.Errorf("DuplicateTokenEx (system): %w", err)
	}

	sid := sessionID
	ok, _, err = procSetTokenInformation.Call(
		uintptr(dup), uintptr(tokenInfoClassSession),
		uintptr(unsafe.Pointer(&sid)), unsafe.Sizeof(sid),
	)
	if ok == 0 {
		procCloseHandle.Call(uintptr(dup))
		return 0, fmt.Errorf("SetTokenInformation(TokenSessionId): %w", err)
	}
	return dup, nil
}

// launchInSession starts exe on whichever desktop matches sessionID right
// now: the logged-in user's own desktop if someone is signed in there, or
// the secure sign-in/lock desktop ("Winlogon") as SYSTEM if nobody is.
func launchInSession(sessionID uint32, exe string) (uintptr, error) {
	var token syscall.Handle
	desktop := `winsta0\Winlogon`

	if t, ok := buildUserToken(sessionID); ok {
		token = t
		desktop = `winsta0\Default`
	} else {
		t, err := buildSystemTokenForSession(sessionID)
		if err != nil {
			return 0, err
		}
		token = t
	}
	defer procCloseHandle.Call(uintptr(token))

	var envBlock uintptr
	procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&envBlock)), uintptr(token), 0)
	if envBlock != 0 {
		defer procDestroyEnvironmentBlock.Call(envBlock)
	}

	si := startupInfo{LpDesktop: utf16ptr(desktop)}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi processInformation
	cmdLine := utf16ptr(`"` + exe + `"`)

	ok, _, err := procCreateProcessAsUserW.Call(
		uintptr(token), 0, uintptr(unsafe.Pointer(cmdLine)),
		0, 0, 0,
		uintptr(createUnicodeEnv|createNoWindow),
		envBlock, 0,
		uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)),
	)
	if ok == 0 {
		return 0, fmt.Errorf("CreateProcessAsUserW (desktop=%s): %w", desktop, err)
	}
	procCloseHandle.Call(pi.hThread)
	log.Printf("launched agent in session %d on %s (pid %d)", sessionID, desktop, pi.dwProcessId)
	return pi.hProcess, nil
}

func processAlive(hProcess uintptr) bool {
	if hProcess == 0 {
		return false
	}
	var code uint32
	ok, _, _ := procGetExitCodeProcess.Call(hProcess, uintptr(unsafe.Pointer(&code)))
	return ok != 0 && code == stillActive
}

func killProcess(hProcess uintptr) {
	if hProcess == 0 {
		return
	}
	procTerminateProcess.Call(hProcess, 0)
	procCloseHandle.Call(hProcess)
}

// ---- Windows service plumbing ----------------------------------------

type iqsoftService struct{ agentExe string }

func (s *iqsoftService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	var curSession uint32 = 0xFFFFFFFF // force a launch on the first tick
	var curProc uintptr

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			sess := activeSessionID()
			if sess != curSession || !processAlive(curProc) {
				killProcess(curProc)
				curProc = 0
				if p, err := launchInSession(sess, s.agentExe); err != nil {
					log.Println("launch failed:", err)
				} else {
					curProc = p
					curSession = sess
				}
			}
		case req := <-r:
			switch req.Cmd {
			case svc.Stop, svc.Shutdown:
				break loop
			}
		}
	}

	killProcess(curProc)
	changes <- svc.Status{State: svc.StopPending}
	return false, 0
}

func installService(exe string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(svcName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", svcName)
	}
	s, err = m.CreateService(svcName, exe, mgr.Config{
		DisplayName: svcDisplay,
		StartType:   mgr.StartAutomatic,
		Description: "Keeps the IQSoft endpoint agent running, including on the sign-in and lock screen.",
	})
	if err != nil {
		return err
	}
	defer s.Close()
	return nil
}

func removeService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(svcName)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Delete()
}

func main() {
	logFile, err := os.OpenFile(
		filepath.Join(filepath.Dir(mustExe()), "iqsoftsvc.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644,
	)
	if err == nil {
		log.SetOutput(logFile)
	}

	if len(os.Args) > 1 {
		exe := mustExe()
		switch os.Args[1] {
		case "install":
			if err := installService(exe); err != nil {
				fmt.Println("install failed:", err)
				os.Exit(1)
			}
			fmt.Println("service installed - start it with: iqsoftsvc.exe start  (or: net start", svcName+")")
			return
		case "remove":
			if err := removeService(); err != nil {
				fmt.Println("remove failed:", err)
				os.Exit(1)
			}
			fmt.Println("service removed")
			return
		case "start":
			runCmd("sc", "start", svcName)
			return
		case "stop":
			runCmd("sc", "stop", svcName)
			return
		}
	}

	isSvc, err := svc.IsWindowsService()
	if err != nil {
		fmt.Println("could not determine session type:", err)
		os.Exit(1)
	}
	if !isSvc {
		fmt.Println("run as: iqsoftsvc.exe install | start | stop | remove  (as Administrator)")
		return
	}

	agentExe := filepath.Join(filepath.Dir(mustExe()), "agent.exe")
	_ = svc.Run(svcName, &iqsoftService{agentExe: agentExe})
}

func mustExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "iqsoftsvc.exe"
	}
	return exe
}

func runCmd(name string, args ...string) {
	out, err := exec.Command(name, args...).CombinedOutput()
	fmt.Println(string(out))
	if err != nil {
		fmt.Println(err)
	}
}