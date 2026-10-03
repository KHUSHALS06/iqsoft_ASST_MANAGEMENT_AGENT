package main

// restrict.go - blocks software install / uninstall on this PC using Windows
// policy registry keys. It works on Windows Pro, so no AppLocker licence
// (Enterprise/Education) is needed.
//
// Three independent switches (see RestrictOptions):
//
//	BlockMSI        Windows Installer policy: stops .msi installs and
//	                per-user installs started by users.
//	BlockUninstall  Sets NoRemove=1 on every app's uninstall entry, which hides
//	                the Uninstall button in Settings and Programs and Features.
//	BlockUserExe    Software Restriction Policy: standard users cannot run
//	                programs from Downloads, Desktop and Temp (where
//	                downloaded installers live). Local admins and SYSTEM are
//	                exempt (PolicyScope=1).
//
// Everything the agent changes is recorded so it can be undone exactly. The
// record lives in HKLM (writable by admins only), NOT in the data folder:
// that folder is writable by all local users, and a user must never be able
// to influence what a SYSTEM process writes to the registry.
//
// The agent must run elevated / as SYSTEM for any of this to work.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	msiPolicyKey   = `SOFTWARE\Policies\Microsoft\Windows\Installer`
	saferParentKey = `SOFTWARE\Policies\Microsoft\Windows\Safer`
	saferKey       = saferParentKey + `\CodeIdentifiers`
	uninstallKey64 = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	uninstallKey32 = `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`

	stateKeyPath   = `SOFTWARE\IQSoft\Agent`
	stateValueName = "RestrictState"
)

// RestrictOptions is the desired state. All false == nothing restricted.
type RestrictOptions struct {
	BlockMSI       bool `json:"block_msi"`
	BlockUninstall bool `json:"block_uninstall"`
	BlockUserExe   bool `json:"block_user_exe"`
}

// RestrictStatus is what is actually in effect right now (read from the
// registry, not from what we last asked for).
type RestrictStatus struct {
	BlockMSI            bool `json:"block_msi"`
	BlockUninstall      bool `json:"block_uninstall"`
	BlockUserExe        bool `json:"block_user_exe"`
	UninstallKeysLocked int  `json:"uninstall_keys_locked"`
}

type priorValue struct {
	Existed bool   `json:"existed"`
	Value   uint32 `json:"value"`
}

type restrictState struct {
	Options    RestrictOptions       `json:"options"`
	Prior      map[string]priorValue `json:"prior"`       // "key|value" -> what it was before we changed it
	NoRemove   []string              `json:"no_remove"`   // "64|<subkey>" / "32|<subkey>": uninstall entries WE locked
	SRPCreated bool                  `json:"srp_created"` // we created the Safer\CodeIdentifiers policy
}

// The only DWORD policy values this file ever writes besides the SRP tree.
// Restore logic walks THIS list, never paths read from saved state.
var trackedDwords = []struct{ path, name string }{
	{msiPolicyKey, "DisableMSI"},          // 1 = users cannot run non-managed MSI installs
	{msiPolicyKey, "DisableUserInstalls"}, // 1 = no per-user installs
}

var srpRules = []struct{ guid, path, desc string }{
	{"{7d3c1a52-0b6e-4f0a-9c21-5a8e1f3b7c01}", `%USERPROFILE%\Downloads`, "IQSoft: block programs run from Downloads"},
	{"{7d3c1a52-0b6e-4f0a-9c21-5a8e1f3b7c02}", `%USERPROFILE%\Desktop`, "IQSoft: block programs run from Desktop"},
	{"{7d3c1a52-0b6e-4f0a-9c21-5a8e1f3b7c03}", `%LOCALAPPDATA%\Temp`, "IQSoft: block programs run from the user Temp folder"},
}

var srpFileTypes = []string{"EXE", "MSI", "MSP", "BAT", "CMD", "COM", "SCR", "PIF", "HTA", "VBS"}

var restrictMu sync.Mutex

// ---- saved state (HKLM, admin-only) ---------------------------------

func loadRestrictState() *restrictState {
	st := &restrictState{}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, stateKeyPath, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue(stateValueName); err == nil {
			json.Unmarshal([]byte(s), st) // a corrupt record just means "start fresh"
		}
		k.Close()
	}
	if st.Prior == nil {
		st.Prior = map[string]priorValue{}
	}
	return st
}

func saveRestrictState(st *restrictState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, stateKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(stateValueName, string(b))
}

// ---- 1. Windows Installer policy --------------------------------------

func setDwordTracked(st *restrictState, path, name string, val uint32) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, path, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	id := path + "|" + name
	if _, seen := st.Prior[id]; !seen { // remember only the ORIGINAL value, once
		cur, _, gerr := k.GetIntegerValue(name)
		st.Prior[id] = priorValue{Existed: gerr == nil, Value: uint32(cur)}
	}
	return k.SetDWordValue(name, val)
}

func restoreTracked(st *restrictState) error {
	for _, t := range trackedDwords {
		id := t.path + "|" + t.name
		prev, ok := st.Prior[id]
		if !ok {
			continue
		}
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, t.path, registry.SET_VALUE)
		if err != nil {
			if errors.Is(err, registry.ErrNotExist) {
				delete(st.Prior, id)
				continue
			}
			return err
		}
		if prev.Existed {
			err = k.SetDWordValue(t.name, prev.Value)
		} else if err = k.DeleteValue(t.name); errors.Is(err, registry.ErrNotExist) {
			err = nil
		}
		k.Close()
		if err != nil {
			return err
		}
		delete(st.Prior, id)
	}
	registry.DeleteKey(registry.LOCAL_MACHINE, msiPolicyKey) // only succeeds if now empty; failure is fine
	return nil
}

func applyMSI(st *restrictState, on bool) (string, error) {
	if !on {
		had := false
		for _, t := range trackedDwords {
			if _, ok := st.Prior[t.path+"|"+t.name]; ok {
				had = true
			}
		}
		if !had {
			return "", nil
		}
		if err := restoreTracked(st); err != nil {
			return "", err
		}
		return "Windows Installer policy removed", nil
	}
	for _, t := range trackedDwords {
		if err := setDwordTracked(st, t.path, t.name, 1); err != nil {
			return "", err
		}
	}
	return "Windows Installer: MSI and per-user installs blocked for users", nil
}

// setMSIValues flips the two policy values WITHOUT touching saved state. Used
// only by allowInstallsDuring.
func setMSIValues(v uint32) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, msiPolicyKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	for _, t := range trackedDwords {
		k.SetDWordValue(t.name, v)
	}
}

// allowInstallsDuring lets the agent's OWN installs (winget, MSI) run while
// BlockMSI is on: it lifts the Installer block, runs fn, and puts it back
// (even if fn panics). Wrap every winget install/upgrade with this.
func allowInstallsDuring(fn func()) {
	restrictMu.Lock()
	blocked := loadRestrictState().Options.BlockMSI
	if blocked {
		setMSIValues(0)
	}
	restrictMu.Unlock()
	defer func() {
		if blocked {
			restrictMu.Lock()
			setMSIValues(1)
			restrictMu.Unlock()
		}
	}()
	fn()
}

// ---- 2. Hide "Uninstall" -----------------------------------------------

var uninstallRoots = []struct{ tag, path string }{
	{"64", uninstallKey64},
	{"32", uninstallKey32},
}

func uninstallEntryPath(entry string) (string, bool) {
	tag, name, ok := strings.Cut(entry, "|")
	if !ok || name == "" || strings.Contains(name, `\`) {
		return "", false
	}
	for _, r := range uninstallRoots {
		if r.tag == tag {
			return r.path + `\` + name, true
		}
	}
	return "", false
}

func applyUninstall(st *restrictState, on bool) (string, error) {
	if !on {
		if len(st.NoRemove) == 0 {
			return "", nil
		}
		n := 0
		for _, e := range st.NoRemove {
			p, ok := uninstallEntryPath(e)
			if !ok {
				continue
			}
			k, err := registry.OpenKey(registry.LOCAL_MACHINE, p, registry.SET_VALUE)
			if err != nil {
				continue // app already gone
			}
			if k.DeleteValue("NoRemove") == nil {
				n++
			}
			k.Close()
		}
		st.NoRemove = nil
		return fmt.Sprintf("Uninstall unlocked on %d app(s)", n), nil
	}

	have := map[string]bool{}
	for _, e := range st.NoRemove {
		have[e] = true
	}
	added := 0
	for _, root := range uninstallRoots {
		rk, err := registry.OpenKey(registry.LOCAL_MACHINE, root.path, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue // e.g. no 32-bit hive
		}
		names, err := rk.ReadSubKeyNames(-1)
		rk.Close()
		if err != nil {
			continue
		}
		for _, n := range names {
			entry := root.tag + "|" + n
			if have[entry] {
				continue
			}
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, root.path+`\`+n, registry.QUERY_VALUE|registry.SET_VALUE)
			if err != nil {
				continue
			}
			if _, _, gerr := sk.GetIntegerValue("NoRemove"); gerr != nil { // not already set by the installer
				if sk.SetDWordValue("NoRemove", 1) == nil {
					st.NoRemove = append(st.NoRemove, entry)
					added++
				}
			}
			sk.Close()
		}
	}
	return fmt.Sprintf("Uninstall hidden on %d new app(s), %d locked in total", added, len(st.NoRemove)), nil
}

// ---- 3. Software Restriction Policy for user-writable folders ------------

func deleteKeyTree(root registry.Key, path string) error {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return err
	}
	names, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := deleteKeyTree(root, path+`\`+n); err != nil {
			return err
		}
	}
	return registry.DeleteKey(root, path)
}

func applySRP(st *restrictState, on bool) (string, error) {
	lm := registry.LOCAL_MACHINE
	if !on {
		if !st.SRPCreated {
			return "", nil
		}
		if err := deleteKeyTree(lm, saferKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return "", err
		}
		registry.DeleteKey(lm, saferParentKey) // only succeeds if empty
		st.SRPCreated = false
		return "Program block (Downloads/Desktop/Temp) removed", nil
	}

	if !st.SRPCreated {
		// Never override a policy somebody else set (e.g. Group Policy).
		if k, err := registry.OpenKey(lm, saferKey, registry.QUERY_VALUE); err == nil {
			k.Close()
			return "", errors.New("a Software Restriction Policy already exists on this PC (probably Group Policy); not overriding it")
		}
	}
	k, _, err := registry.CreateKey(lm, saferKey, registry.SET_VALUE)
	if err != nil {
		return "", err
	}
	st.SRPCreated = true // recorded now so a half-finished apply can still be cleaned up
	err = func() error {
		defer k.Close()
		if err := k.SetDWordValue("DefaultLevel", 0x40000); err != nil { // Unrestricted: only our path rules block
			return err
		}
		if err := k.SetDWordValue("TransparentEnabled", 1); err != nil { // check EXEs, not DLLs
			return err
		}
		if err := k.SetDWordValue("PolicyScope", 1); err != nil { // everyone EXCEPT local administrators
			return err
		}
		if err := k.SetDWordValue("AuthenticodeEnabled", 0); err != nil {
			return err
		}
		return k.SetStringsValue("ExecutableTypes", srpFileTypes)
	}()
	if err != nil {
		return "", err
	}

	filetime := uint64(time.Now().UnixNano()/100) + 116444736000000000
	for _, r := range srpRules {
		rk, _, err := registry.CreateKey(lm, saferKey+`\0\Paths\`+r.guid, registry.SET_VALUE)
		if err != nil {
			return "", err
		}
		err = func() error {
			defer rk.Close()
			if err := rk.SetStringValue("ItemData", r.path); err != nil {
				return err
			}
			if err := rk.SetDWordValue("SaferFlags", 0); err != nil {
				return err
			}
			if err := rk.SetQWordValue("LastModified", filetime); err != nil {
				return err
			}
			return rk.SetStringValue("Description", r.desc)
		}()
		if err != nil {
			return "", err
		}
	}
	return "Program block active: standard users cannot run programs from Downloads, Desktop or Temp", nil
}

// ---- public entry points -----------------------------------------------

// applyRestrictions makes the PC match opts, undoing any switch that is off.
// Calling it with an all-false RestrictOptions removes everything.
func applyRestrictions(opts RestrictOptions) (string, error) {
	restrictMu.Lock()
	defer restrictMu.Unlock()

	st := loadRestrictState()
	var msgs []string
	var errs []error
	step := func(name string, fn func() (string, error)) {
		m, err := fn()
		if m != "" {
			msgs = append(msgs, m)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			msgs = append(msgs, name+" FAILED: "+err.Error())
		}
	}
	step("installer policy", func() (string, error) { return applyMSI(st, opts.BlockMSI) })
	step("uninstall block", func() (string, error) { return applyUninstall(st, opts.BlockUninstall) })
	step("program block", func() (string, error) { return applySRP(st, opts.BlockUserExe) })

	st.Options = opts
	if err := saveRestrictState(st); err != nil {
		errs = append(errs, fmt.Errorf("saving state: %w", err))
		msgs = append(msgs, "saving state FAILED: "+err.Error())
	}
	if len(msgs) == 0 {
		msgs = append(msgs, "nothing to change")
	}
	return strings.Join(msgs, "\n"), errors.Join(errs...)
}

// restrictionStatus reports what is really in effect, read from the registry.
func restrictionStatus() RestrictStatus {
	restrictMu.Lock()
	defer restrictMu.Unlock()
	st := loadRestrictState()
	var s RestrictStatus

	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, msiPolicyKey, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("DisableMSI"); err == nil && v == 1 {
			s.BlockMSI = true
		}
		k.Close()
	}
	s.UninstallKeysLocked = len(st.NoRemove)
	s.BlockUninstall = s.UninstallKeysLocked > 0
	if st.SRPCreated {
		if k, err := registry.OpenKey(registry.LOCAL_MACHINE, saferKey+`\0\Paths\`+srpRules[0].guid, registry.QUERY_VALUE); err == nil {
			s.BlockUserExe = true
			k.Close()
		}
	}
	return s
}

// runRestrictJob is what jobs.go calls for the two new job types.
//
//	apply_restrictions  params: block_msi, block_uninstall, block_user_exe ("1" = on)
//	clear_restrictions  no params; removes everything
func runRestrictJob(jobType string, params map[string]string) (string, string) {
	if !isElevatedAdmin() {
		return "failed", "the agent is not running as SYSTEM or an elevated admin, so it cannot change machine policy"
	}
	var opts RestrictOptions
	if jobType == "apply_restrictions" {
		opts = RestrictOptions{
			BlockMSI:       params["block_msi"] == "1",
			BlockUninstall: params["block_uninstall"] == "1",
			BlockUserExe:   params["block_user_exe"] == "1",
		}
	}
	out, err := applyRestrictions(opts)
	if err != nil {
		return "failed", out
	}
	return "done", out
}