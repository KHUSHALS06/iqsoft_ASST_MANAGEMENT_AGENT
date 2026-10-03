package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
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

func registerWingetSearch() {
	http.HandleFunc("GET /admin/winget/search", admin(func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" || len(q) > 100 || strings.HasPrefix(q, "-") {
			http.Error(w, "bad query", http.StatusBadRequest)
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