package main

// accounts.go - two inventory add-ons, both read from THIS PC only:
//
//  1. Licences      Is Microsoft Office / Adobe software on this PC paid, free,
//                   trial or unlicensed?  (Office: Click-to-Run config + ospp.vbs;
//                   Adobe: product names, because Adobe exposes no local licence API.)
//  2. Accounts      Which account is signed in inside each app?  (Office, OneDrive,
//                   Windows/Microsoft account, Entra ID join, Chrome, Edge, Brave,
//                   Firefox, Teams, Dropbox.)
//
// What this file deliberately does NOT do:
//   - It never reads passwords, cookies, tokens or product keys - only the
//     account identifier (e-mail / UPN) the app itself stores in plain text.
//   - It cannot tell who PAYS for a subscription. That lives in the vendor's
//     cloud (Microsoft Graph / Adobe Admin Console), not on the device.
//   - Adobe account e-mail and plan are not readable locally, so Adobe entries
//     are best-effort guesses and say so in their "basis" field.
//
// Per-user data lives in each user's registry hive (HKEY_USERS\<SID>) and profile
// folder. The agent only sees a hive that is currently loaded, so results for
// users who are not signed in are served from a small cache and flagged "stale".

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

// Account is one signed-in account found inside one app for one Windows user.
type Account struct {
	User    string `json:"user"`            // Windows profile name ("(device)" for machine-wide)
	App     string `json:"app"`             // e.g. "Google Chrome"
	Account string `json:"account"`         // e-mail / UPN; empty when the app stores no e-mail
	Name    string `json:"name,omitempty"`  // display name / profile label
	Kind    string `json:"kind,omitempty"`  // "Work or school", "Personal", ...
	Plan    string `json:"plan,omitempty"`  // free/paid plan when the app stores it locally
	Source  string `json:"source"`          // where we read it from
	SeenAt  string `json:"seen_at"`         // RFC3339 UTC, when this user was last readable
	Stale   bool   `json:"stale,omitempty"` // true = user not signed in now, value is cached
}

// License is the licence state of one installed product.
type License struct {
	Vendor  string `json:"vendor"`
	Product string `json:"product"`
	Version string `json:"version,omitempty"`
	Edition string `json:"edition,omitempty"`
	Type    string `json:"type"`             // Subscription | Perpetual | Volume | Trial | Free | Unknown
	Tier    string `json:"tier"`             // Premium | Likely premium | Free | Trial | Unlicensed | Unknown
	Status  string `json:"status,omitempty"` // e.g. "Licensed", "Unlicensed (reduced functionality)"
	Basis   string `json:"basis"`            // how we decided - always shown so nobody over-trusts it
}

// ---- small registry helpers --------------------------------------------------

func acStr(k registry.Key, name string) string {
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func acDword(k registry.Key, name string) (uint32, bool) {
	v, _, err := k.GetIntegerValue(name)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

func acOpen(root registry.Key, path string) (registry.Key, bool) {
	k, err := registry.OpenKey(root, path, registry.READ)
	if err != nil {
		return 0, false
	}
	return k, true
}

func acSubKeys(root registry.Key, path string) []string {
	k, ok := acOpen(root, path)
	if !ok {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	return names
}

func acFirst(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---- Windows profiles --------------------------------------------------------

type acProfile struct{ SID, Name, Path string }

const acProfileList = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`

// acProfiles lists real user profiles: local/domain accounts (S-1-5-21-...) and
// Entra ID accounts (S-1-12-1-...). Service and system profiles are skipped.
func acProfiles() []acProfile {
	var out []acProfile
	for _, sid := range acSubKeys(registry.LOCAL_MACHINE, acProfileList) {
		if !strings.HasPrefix(sid, "S-1-5-21-") && !strings.HasPrefix(sid, "S-1-12-1-") {
			continue
		}
		k, ok := acOpen(registry.LOCAL_MACHINE, acProfileList+`\`+sid)
		if !ok {
			continue
		}
		p := acStr(k, "ProfileImagePath")
		k.Close()
		if p == "" {
			continue
		}
		if x, err := registry.ExpandString(p); err == nil {
			p = x
		}
		out = append(out, acProfile{SID: sid, Name: filepath.Base(p), Path: p})
	}
	return out
}

// ---- per-user account discovery ---------------------------------------------

// acCollectUser returns (accounts, true) when the user's registry hive is loaded
// and therefore readable now; (nil, false) when the user is not signed in or
// this process may not read that hive.
func acCollectUser(p acProfile, now string) ([]Account, bool) {
	if hk, err := registry.OpenKey(registry.USERS, p.SID, registry.READ); err != nil {
		return nil, false
	} else {
		hk.Close()
	}

	var out []Account
	add := func(a Account) {
		if a.Account == "" && a.Plan == "" {
			return
		}
		a.User, a.SeenAt = p.Name, now
		out = append(out, a)
	}

	// Microsoft Office (Microsoft 365 / Office 2016+ identity cache).
	idBase := p.SID + `\Software\Microsoft\Office\16.0\Common\Identity\Identities`
	for _, n := range acSubKeys(registry.USERS, idBase) {
		k, ok := acOpen(registry.USERS, idBase+`\`+n)
		if !ok {
			continue
		}
		email := acFirst(acStr(k, "EmailAddress"), acStr(k, "SignInName"))
		name := acStr(k, "FriendlyName")
		k.Close()
		kind := ""
		switch u := strings.ToUpper(n); {
		case strings.HasSuffix(u, "_ADAL"):
			kind = "Work or school"
		case strings.HasSuffix(u, "_LIVEID"):
			kind = "Personal (Microsoft account)"
		}
		add(Account{App: "Microsoft Office", Account: email, Name: name, Kind: kind, Source: "registry: Office identity cache"})
	}

	// OneDrive.
	odBase := p.SID + `\Software\Microsoft\OneDrive\Accounts`
	for _, n := range acSubKeys(registry.USERS, odBase) {
		k, ok := acOpen(registry.USERS, odBase+`\`+n)
		if !ok {
			continue
		}
		email := acStr(k, "UserEmail")
		biz, _ := acDword(k, "Business")
		k.Close()
		kind := "Personal"
		if biz == 1 {
			kind = "Work or school"
		}
		add(Account{App: "OneDrive", Account: email, Kind: kind, Source: "registry: OneDrive accounts"})
	}

	// Windows sign-in with a Microsoft account (account names are the sub-key names).
	for _, n := range acSubKeys(registry.USERS, p.SID+`\Software\Microsoft\IdentityCRL\UserExtendedProperties`) {
		if strings.Contains(n, "@") {
			add(Account{App: "Windows / Microsoft account", Account: n, Kind: "Personal (Microsoft account)", Source: "registry: IdentityCRL"})
		}
	}

	// Chromium browsers: the signed-in account of every browser profile.
	chromium := []struct{ app, rel, kind string }{
		{"Google Chrome", `AppData\Local\Google\Chrome\User Data\Local State`, "Google account"},
		{"Microsoft Edge", `AppData\Local\Microsoft\Edge\User Data\Local State`, "Microsoft account (work or personal)"},
		{"Brave", `AppData\Local\BraveSoftware\Brave-Browser\User Data\Local State`, "Browser sync account"},
	}
	for _, c := range chromium {
		b, err := os.ReadFile(filepath.Join(p.Path, c.rel))
		if err != nil {
			continue
		}
		var st struct {
			Profile struct {
				InfoCache map[string]struct {
					UserName string `json:"user_name"`
					Name     string `json:"name"`
				} `json:"info_cache"`
			} `json:"profile"`
		}
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		keys := make([]string, 0, len(st.Profile.InfoCache))
		for k := range st.Profile.InfoCache {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			pi := st.Profile.InfoCache[k]
			add(Account{App: c.app, Account: pi.UserName, Name: "Profile: " + acFirst(pi.Name, k), Kind: c.kind, Source: "file: browser Local State"})
		}
	}

	// Firefox sync account.
	ff, _ := filepath.Glob(filepath.Join(p.Path, `AppData\Roaming\Mozilla\Firefox\Profiles\*\signedInUser.json`))
	for _, f := range ff {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			AccountData struct {
				Email string `json:"email"`
			} `json:"accountData"`
		}
		if json.Unmarshal(b, &s) == nil {
			add(Account{App: "Mozilla Firefox", Account: s.AccountData.Email, Kind: "Firefox account", Source: "file: signedInUser.json"})
		}
	}

	// Microsoft Teams (classic desktop client config; best effort).
	if b, err := os.ReadFile(filepath.Join(p.Path, `AppData\Roaming\Microsoft\Teams\desktop-config.json`)); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			upn, _ := m["userUpn"].(string)
			if upn == "" {
				upn, _ = m["upnWindowUserUpn"].(string)
			}
			add(Account{App: "Microsoft Teams", Account: upn, Kind: "Work or school", Source: "file: Teams desktop-config.json"})
		}
	}

	// Dropbox: stores the plan type (Basic = free) but not the e-mail.
	if b, err := os.ReadFile(filepath.Join(p.Path, `AppData\Local\Dropbox\info.json`)); err == nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(b, &m) == nil {
			for _, kind := range []string{"personal", "business"} {
				raw, ok := m[kind]
				if !ok {
					continue
				}
				var d struct {
					Plan string `json:"subscription_type"`
				}
				if json.Unmarshal(raw, &d) == nil {
					add(Account{App: "Dropbox", Kind: strings.Title(kind), Plan: acFirst(d.Plan, "signed in"), Source: "file: Dropbox info.json"})
				}
			}
		}
	}

	return acDedupe(out), true
}

// acEntraJoin reports the work account this PC is joined to (machine-wide).
func acEntraJoin(now string) []Account {
	const base = `SYSTEM\CurrentControlSet\Control\CloudDomainJoin\JoinInfo`
	var out []Account
	for _, n := range acSubKeys(registry.LOCAL_MACHINE, base) {
		k, ok := acOpen(registry.LOCAL_MACHINE, base+`\`+n)
		if !ok {
			continue
		}
		email, tenant := acStr(k, "UserEmail"), acStr(k, "TenantId")
		k.Close()
		if email == "" && tenant == "" {
			continue
		}
		out = append(out, Account{User: "(device)", App: "Windows (Entra ID joined)", Account: email,
			Name: "Tenant " + tenant, Kind: "Work or school", Source: "registry: CloudDomainJoin", SeenAt: now})
	}
	return out
}

func acDedupe(in []Account) []Account {
	seen := map[string]bool{}
	out := make([]Account, 0, len(in))
	for _, a := range in {
		k := strings.ToLower(a.User + "|" + a.App + "|" + a.Account + "|" + a.Kind + "|" + a.Plan + "|" + a.Name)
		if !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

// ---- cache for users who are not signed in right now ------------------------

type acCache struct {
	Users map[string][]Account `json:"users"`
}

func acCachePath() string { return filepath.Join(dataDir(), "accounts-cache.json") }

func acLoadCache() acCache {
	c := acCache{Users: map[string][]Account{}}
	if b, err := os.ReadFile(acCachePath()); err == nil {
		_ = json.Unmarshal(b, &c)
		if c.Users == nil {
			c.Users = map[string][]Account{}
		}
	}
	return c
}

func acSaveCache(c acCache) {
	if b, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(acCachePath(), b, 0o644)
	}
}

// ---- Office licences ---------------------------------------------------------

// Order matters: the first matching key wins, so specific keys come first.
var acOfficeProducts = []struct {
	key, name string
	free      bool
}{
	{"o365proplus", "Microsoft 365 Apps for enterprise", false},
	{"o365business", "Microsoft 365 Apps for business", false},
	{"o365smallbusprem", "Microsoft 365 Apps for business (Business Premium)", false},
	{"o365homeprem", "Microsoft 365 Family / Personal", false},
	{"homebusiness", "Office Home & Business", false},
	{"homestudent", "Office Home & Student", false},
	{"proplus", "Office Professional Plus (LTSC / volume)", false},
	{"professional", "Office Professional", false},
	{"standard", "Office Standard", false},
	{"visiopro", "Visio Professional", false},
	{"visiostd", "Visio Standard", false},
	{"projectpro", "Project Professional", false},
	{"projectstd", "Project Standard", false},
	{"mondo", "Office Mondo (all-apps test/volume suite)", false},
	{"accessruntime", "Access Runtime", true},
	{"proofing", "Office Proofing Tools", true},
	{"languagepack", "Office Language Pack", true},
	{"home", "Office Home", false},
}

var acYearRe = regexp.MustCompile(`20\d\d`)

// acClassifyRelease turns a Click-to-Run release id such as "O365ProPlusRetail"
// into (friendly product, licence type, how we decided).
func acClassifyRelease(id string) (string, string, string) {
	l := strings.ToLower(id)
	product, free := id, false
	for _, m := range acOfficeProducts {
		if strings.Contains(l, m.key) {
			product, free = m.name, m.free
			break
		}
	}
	switch {
	case free:
		return product, "Free", "free Office component (from release id " + id + ")"
	case strings.Contains(l, "o365") || strings.Contains(l, "m365"):
		return product, "Subscription", "release id " + id + " is a Microsoft 365 subscription SKU"
	case strings.Contains(l, "volume"):
		return product, "Volume", "release id " + id + " is a volume-licensed SKU"
	case strings.Contains(l, "retail") && !acYearRe.MatchString(l):
		return product, "Subscription", "retail release id " + id + " has no year, so it is a subscription SKU (e.g. Visio/Project plan)"
	case strings.Contains(l, "retail"):
		return product, "Perpetual", "retail release id " + id + " with a year is a one-time-purchase SKU"
	}
	return product, "Unknown", "unrecognised release id " + id
}

type acOsppBlock struct{ Name, Desc, Status, Grace, SKU string }

func acFindOspp() string {
	for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if pf == "" {
			continue
		}
		for _, rel := range []string{
			`Microsoft Office\Office16\OSPP.VBS`,
			`Microsoft Office\root\Office16\OSPP.VBS`,
			`Microsoft Office\Office15\OSPP.VBS`,
		} {
			p := filepath.Join(pf, rel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func acFriendlyStatus(s string) string {
	switch u := strings.ToUpper(strings.Trim(strings.TrimSpace(s), "- ")); u {
	case "LICENSED":
		return "Licensed"
	case "NOTIFICATIONS":
		return "Unlicensed (reduced functionality)"
	case "UNLICENSED":
		return "Unlicensed"
	case "OOB_GRACE", "OOT_GRACE":
		return "Grace period"
	case "EXTENDED_GRACE":
		return "Extended grace period"
	case "":
		return ""
	default:
		return strings.Title(strings.ToLower(strings.ReplaceAll(u, "_", " ")))
	}
}

// acRunOspp runs "cscript ospp.vbs /dstatus" and parses one block per licence.
// It prints the LAST five characters of the product key, which we ignore.
func acRunOspp() []acOsppBlock {
	script := acFindOspp()
	if script == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cscript.exe", "//Nologo", script, "/dstatus")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		log.Println("ospp /dstatus failed:", err)
		return nil
	}
	var blocks []acOsppBlock
	var cur *acOsppBlock
	flush := func() {
		if cur != nil {
			blocks = append(blocks, *cur)
			cur = nil
		}
	}
	for _, ln := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		i := strings.Index(ln, ":")
		if i <= 0 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(ln[:i]))
		val := strings.TrimSpace(ln[i+1:])
		if key == "SKU ID" {
			flush()
			cur = &acOsppBlock{SKU: val}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "LICENSE NAME":
			cur.Name = val
		case "LICENSE DESCRIPTION":
			cur.Desc = val
		case "LICENSE STATUS":
			cur.Status = acFriendlyStatus(val)
		case "REMAINING GRACE":
			cur.Grace = val
		}
	}
	flush()
	return blocks
}

func acTier(typ, status string) string {
	s := strings.ToLower(status)
	switch {
	case typ == "Free":
		return "Free"
	case typ == "Trial":
		return "Trial"
	case strings.Contains(s, "unlicensed"):
		return "Unlicensed"
	case typ == "Unknown":
		return "Unknown"
	case s == "licensed" || strings.Contains(s, "grace"):
		return "Premium"
	}
	return "Likely premium" // licence state could not be verified
}

func acOfficeLicenses() []License {
	blocks := acRunOspp()

	// Click-to-Run (Microsoft 365 / Office 2016+ from the web) install config.
	var ids, ver string
	for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Office\ClickToRun\Configuration`, registry.QUERY_VALUE|view)
		if err != nil {
			continue
		}
		ids, ver = acStr(k, "ProductReleaseIds"), acStr(k, "VersionToReport")
		k.Close()
		if ids != "" {
			break
		}
	}

	var out []License
	if ids != "" {
		for _, id := range strings.Split(ids, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			product, typ, basis := acClassifyRelease(id)
			status := ""
			// Match this release id to its ospp licence by product name.
			key := strings.ToLower(strings.NewReplacer("Retail", "", "Volume", "").Replace(id))
			for _, b := range blocks {
				if strings.Contains(strings.ToLower(b.Name), key) {
					status = b.Status
					if strings.Contains(strings.ToLower(b.Name+" "+b.Desc), "trial") {
						typ = "Trial"
					}
					break
				}
			}
			if status == "" {
				if len(blocks) == 0 {
					status, basis = "Not verified", basis+"; ospp.vbs unavailable so licence state not verified"
				} else {
					status = "Not listed by ospp"
				}
			} else {
				basis += "; status from ospp.vbs /dstatus"
			}
			out = append(out, License{Vendor: "Microsoft", Product: product, Version: ver, Edition: id,
				Type: typ, Tier: acTier(typ, status), Status: status, Basis: basis})
		}
		return out
	}

	// MSI-based Office (no Click-to-Run): fall back to ospp alone.
	for _, b := range blocks {
		l := strings.ToLower(b.Name + " " + b.Desc)
		typ := "Unknown"
		switch {
		case strings.Contains(l, "trial"):
			typ = "Trial"
		case strings.Contains(l, "subscription") || strings.Contains(l, "o365") || strings.Contains(l, "timebased_sub"):
			typ = "Subscription"
		case strings.Contains(l, "volume") || strings.Contains(l, "kms") || strings.Contains(l, "mak"):
			typ = "Volume"
		case strings.Contains(l, "retail"):
			typ = "Perpetual"
		}
		out = append(out, License{Vendor: "Microsoft", Product: b.Name, Edition: b.Desc, Type: typ,
			Tier: acTier(typ, b.Status), Status: b.Status, Basis: "from ospp.vbs /dstatus licence name and channel"})
	}
	return out
}

// ---- Adobe -------------------------------------------------------------------

var acCSRe = regexp.MustCompile(`\bcs[3-6]\b`)

// Installed Adobe parts that are not products in their own right.
var acAdobeNoise = []string{"genuine", "refresh manager", "update service", "content synchronizer",
	"desktop service", "licensing", "(common)", "cef ", "adobe arm", "creative cloud"}

var acAdobeCC = []string{"photoshop", "illustrator", "indesign", "premiere", "after effects", "lightroom",
	"audition", "animate", "dreamweaver", "adobe xd", "media encoder", "character animator",
	"dimension", "substance", "fresco", "incopy", "prelude", "rush"}

func acClassifyAdobe(name string) (typ, basis string, ok bool) {
	l := strings.ToLower(name)
	for _, n := range acAdobeNoise {
		if strings.Contains(l, n) {
			return "", "", false
		}
	}
	switch {
	case strings.Contains(l, "acrobat reader") || strings.Contains(l, "reader dc"):
		return "Free", "Adobe Acrobat Reader is free", true
	case strings.Contains(l, "digital editions") || strings.Contains(l, "adobe air"):
		return "Free", "free Adobe product", true
	case strings.Contains(l, "acrobat"):
		return "Unknown", "unified Acrobat app runs as free Reader or paid Acrobat depending on sign-in; not readable locally", true
	case acCSRe.MatchString(l):
		return "Perpetual", "legacy Creative Suite (CS3-CS6) was sold as a one-time licence", true
	}
	for _, a := range acAdobeCC {
		if strings.Contains(l, a) {
			return "Subscription", "Creative Cloud app: normally licensed by an Adobe subscription, but a free trial looks identical locally", true
		}
	}
	return "Unknown", "Adobe product with no known licence rule", true
}

func acAdobeLicenses() []License {
	var out []License
	seen := map[string]bool{}
	for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
		rk, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstallKey64, registry.ENUMERATE_SUB_KEYS|view)
		if err != nil {
			continue
		}
		names, _ := rk.ReadSubKeyNames(-1)
		rk.Close()
		for _, n := range names {
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstallKey64+`\`+n, registry.QUERY_VALUE|view)
			if err != nil {
				continue
			}
			name, ver, pub := acStr(sk, "DisplayName"), acStr(sk, "DisplayVersion"), acStr(sk, "Publisher")
			sk.Close()
			if name == "" || !(strings.Contains(strings.ToLower(pub), "adobe") || strings.HasPrefix(strings.ToLower(name), "adobe")) {
				continue
			}
			typ, basis, ok := acClassifyAdobe(name)
			if !ok {
				continue
			}
			k := strings.ToLower(name + "|" + ver)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, License{Vendor: "Adobe", Product: name, Version: ver, Type: typ,
				Tier: acTier(typ, ""), Status: "Not readable locally", Basis: basis + "; Adobe account and plan are not stored on the device"})
		}
	}
	return out
}

// ---- entry point -------------------------------------------------------------

// collectAccounts never fails the inventory: any panic or error just yields
// fewer rows. Both slices are non-nil so they serialise as [] not null.
func collectAccounts() (accts []Account, lics []License) {
	accts, lics = []Account{}, []License{}
	defer func() {
		if r := recover(); r != nil {
			logCrash(r)
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339)

	cache := acLoadCache()
	fresh := map[string]bool{}
	profiles := acProfiles()
	live := map[string]bool{}
	for _, p := range profiles {
		live[p.Name] = true
		if list, ok := acCollectUser(p, now); ok {
			cache.Users[p.Name] = list
			fresh[p.Name] = true
		}
	}
	if len(profiles) > 0 { // forget users whose profile was deleted
		for u := range cache.Users {
			if !live[u] {
				delete(cache.Users, u)
			}
		}
	}
	acSaveCache(cache)

	users := make([]string, 0, len(cache.Users))
	for u := range cache.Users {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		for _, a := range cache.Users[u] {
			a.Stale = !fresh[u]
			accts = append(accts, a)
		}
	}
	accts = append(accts, acEntraJoin(now)...)
	sort.SliceStable(accts, func(i, j int) bool {
		if accts[i].App != accts[j].App {
			return accts[i].App < accts[j].App
		}
		if accts[i].User != accts[j].User {
			return accts[i].User < accts[j].User
		}
		return accts[i].Account < accts[j].Account
	})

	lics = append(lics, acOfficeLicenses()...)
	lics = append(lics, acAdobeLicenses()...)
	log.Printf("accounts: %d account(s), %d licence(s)", len(accts), len(lics))
	return accts, lics
}