package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type wingetHit struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Version string `json:"version"`
	Source  string `json:"source"`
}

var wingetIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

func runWingetCLI(args ...string) (string, error) {
	path, err := exec.LookPath("winget")
	if err != nil {
		path, err = exec.LookPath("winget.exe")
		if err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	args = append(args, "--accept-source-agreements", "--disable-interactivity")
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return string(out), err
}

func tableRows(out string) (header string, rows []string) {
	lines := strings.Split(strings.ReplaceAll(out, "\r", "\n"), "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" && strings.Trim(t, "-") == "" && i > 0 {
			for j := i - 1; j >= 0; j-- {
				if strings.TrimSpace(lines[j]) != "" {
					header = lines[j]
					break
				}
			}
			for _, r := range lines[i+1:] {
				if strings.TrimSpace(r) != "" {
					rows = append(rows, r)
				}
			}
			return
		}
	}
	return
}

func colSlice(s string, start, end int) string {
	r := []rune(s)
	if start >= len(r) {
		return ""
	}
	if end < 0 || end > len(r) {
		end = len(r)
	}
	return strings.TrimSpace(string(r[start:end]))
}

func parseWingetSearch(out string) []wingetHit {
	header, rows := tableRows(out)
	hr := []rune(header)
	idx := func(name string) int {
		i := strings.Index(header, name)
		if i < 0 {
			return -1
		}
		return utf8.RuneCountInString(header[:i])
	}
	iName, iID, iVer, iMatch, iSrc := idx("Name"), idx("Id"), idx("Version"), idx("Match"), idx("Source")
	if iName < 0 || iID < 0 || iVer < 0 || len(hr) == 0 {
		return []wingetHit{}
	}
	verEnd := iMatch
	if verEnd < 0 {
		verEnd = iSrc
	}
	if verEnd < 0 {
		verEnd = -1
	}
	hits := []wingetHit{}
	for _, r := range rows {
		h := wingetHit{
			Name:    colSlice(r, iName, iID),
			ID:      colSlice(r, iID, iVer),
			Version: colSlice(r, iVer, verEnd),
		}
		if iSrc >= 0 {
			h.Source = colSlice(r, iSrc, -1)
		}
		if h.ID == "" || strings.Contains(h.ID, " ") {
			continue
		}
		hits = append(hits, h)
		if len(hits) >= 30 {
			break
		}
	}
	return hits
}

var (
	githubAPI  = "https://api.github.com"
	wingetRepo = "microsoft/winget-pkgs"
	ghClient   = &http.Client{Timeout: 20 * time.Second}

	ghCacheMu sync.Mutex
	ghCache   = map[string]ghCacheEntry{}
)

type ghCacheEntry struct {
	at   time.Time
	data any
}

const ghCacheTTL = 10 * time.Minute

func ghCacheGet(key string) (any, bool) {
	ghCacheMu.Lock()
	defer ghCacheMu.Unlock()
	e, ok := ghCache[key]
	if !ok || time.Since(e.at) > ghCacheTTL {
		return nil, false
	}
	return e.data, true
}

func ghCacheSet(key string, data any) {
	ghCacheMu.Lock()
	defer ghCacheMu.Unlock()
	if len(ghCache) > 500 {
		ghCache = map[string]ghCacheEntry{}
	}
	ghCache[key] = ghCacheEntry{at: time.Now(), data: data}
}

func wingetCLIAvailable() bool {
	if _, err := exec.LookPath("winget"); err == nil {
		return true
	}
	_, err := exec.LookPath("winget.exe")
	return err == nil
}

func ghGet(p string, out any) (int, error) {
	req, err := http.NewRequest("GET", githubAPI+p, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "iqsoft-endpoint-manager")
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := ghClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func manifestDir(id string) string {
	parts := strings.Split(id, ".")
	first := strings.ToLower(string([]rune(parts[0])[:1]))
	return "manifests/" + first + "/" + strings.Join(parts, "/")
}

type ghEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func ghList(dir string) ([]ghEntry, int, error) {
	var entries []ghEntry
	status, err := ghGet("/repos/"+wingetRepo+"/contents/"+dir, &entries)
	return entries, status, err
}

func ghVersions(id string) ([]string, error) {
	key := "v|" + strings.ToLower(id)
	if c, ok := ghCacheGet(key); ok {
		return c.([]string), nil
	}
	entries, status, err := ghList(manifestDir(id))
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return []string{}, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github returned %d (rate limit? set GITHUB_TOKEN)", status)
	}
	dirs := []string{}
	for _, e := range entries {
		if e.Type == "dir" {
			dirs = append(dirs, e.Name)
		}
	}
	if len(dirs) == 0 {
		return []string{}, nil
	}
	probe, status, err := ghList(manifestDir(id) + "/" + dirs[0])
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github returned %d (rate limit? set GITHUB_TOKEN)", status)
	}
	isVersionDir := false
	for _, e := range probe {
		if e.Type == "file" && strings.HasSuffix(e.Name, ".yaml") {
			isVersionDir = true
			break
		}
	}
	if !isVersionDir {
		return []string{}, nil
	}
	sort.Slice(dirs, func(a, b int) bool { return compareVersions(dirs[a], dirs[b]) > 0 })
	ghCacheSet(key, dirs)
	return dirs, nil
}

func ghSearch(q string) ([]wingetHit, error) {
	if os.Getenv("GITHUB_TOKEN") == "" {
		return nil, fmt.Errorf("package search needs GITHUB_TOKEN set on the server (GitHub code search requires a token)")
	}
	key := "s|" + strings.ToLower(q)
	if c, ok := ghCacheGet(key); ok {
		return c.([]wingetHit), nil
	}
	var res struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
	}
	query := q + " in:path repo:" + wingetRepo
	status, err := ghGet("/search/code?per_page=100&q="+url.QueryEscape(query), &res)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github returned %d (rate limit or bad token)", status)
	}
	seen := map[string]bool{}
	hits := []wingetHit{}
	for _, it := range res.Items {
		parts := strings.Split(it.Path, "/")
		if len(parts) < 6 || parts[0] != "manifests" {
			continue
		}
		idParts := parts[2 : len(parts)-2]
		id := strings.Join(idParts, ".")
		if !wingetIDRe.MatchString(id) || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		hits = append(hits, wingetHit{Name: path.Base(strings.Join(idParts, "/")), ID: id, Source: "winget"})
	}
	sort.Slice(hits, func(a, b int) bool { return strings.ToLower(hits[a].ID) < strings.ToLower(hits[b].ID) })
	if len(hits) > 30 {
		hits = hits[:30]
	}
	ghCacheSet(key, hits)
	return hits, nil
}

func registerWingetSearch() {
	http.HandleFunc("GET /admin/winget/search", admin(func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" || len(q) > 100 || strings.HasPrefix(q, "-") {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		if !wingetCLIAvailable() {
			if len(q) < 3 {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte("[]"))
				return
			}
			hits, err := ghSearch(q)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(hits)
			return
		}
		out, err := runWingetCLI("search", q, "--source", "winget")
		hits := parseWingetSearch(out)
		if err != nil && len(hits) == 0 {
			if strings.Contains(out, "No package found") {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte("[]"))
				return
			}
			http.Error(w, "winget search unavailable on the server", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(hits)
	}))

	http.HandleFunc("GET /admin/winget/versions", admin(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if !wingetIDRe.MatchString(id) {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if !wingetCLIAvailable() {
			vs, err := ghVersions(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(vs)
			return
		}
		out, _ := runWingetCLI("show", "--id", id, "--exact", "--versions", "--source", "winget")
		_, rows := tableRows(out)
		vs := []string{}
		for _, r := range rows {
			if v := strings.TrimSpace(r); v != "" && !strings.Contains(v, " ") {
				vs = append(vs, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(vs)
	}))
}