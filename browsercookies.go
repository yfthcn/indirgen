package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Cookies for one engine run.
//
// Every run gets a private cookies.txt holding only the cookies of the site
// it works on. yt-dlp and gallery-dl rewrite their cookie file when they
// exit; with a private copy two runs can no longer mangle the stored file,
// and what the site renewed is written back (atomically, own site only).
//
// Source: an imported cookies.txt for the site, else (if enabled) the
// browser. For the browser, yt-dlp exports the Firefox cookie jar (at most
// every few minutes) and İndirgen keeps only the site's lines. The engines
// never see the whole jar, so a link that redirects to YouTube (t.co,
// bit.ly) or an embedded player cannot take the Google login along.
// YouTube and Google cookies are never used: a signed-in account is what
// gets flagged, and YouTube works without one.
// ---------------------------------------------------------------------------

type CookieArg struct {
	File string // private cookies.txt for this run
}

func (a CookieArg) YTDLP() []string {
	if a.File == "" {
		return nil
	}
	return []string{"--cookies", a.File}
}

func (a CookieArg) GalleryDL() []string {
	if a.File == "" {
		return nil
	}
	return []string{"-C", a.File}
}

// noBrowserCookies: sites that never get the browser's cookies.
var noBrowserCookies = []string{"youtube.com", "youtu.be", "youtube-nocookie.com", "googlevideo.com", "google.com", "ytimg.com"}

const browserExportTTL = 5 * time.Minute

// Use returns the cookies for one run on host and a cleanup to call after it.
func (c *CookieStore) Use(host string) (CookieArg, func()) {
	none := func() {}
	if c == nil {
		return CookieArg{}, none
	}
	site := siteOf(host)
	if src := c.For(host); src != "" {
		raw, err := os.ReadFile(src)
		if err != nil {
			return CookieArg{}, none
		}
		cp, err := c.writeRunCopy(raw)
		if err != nil {
			return CookieArg{File: src}, none // copy failed (disk full?): old behaviour
		}
		return CookieArg{File: cp}, func() { c.writeBack(cp, src, site); _ = os.Remove(cp) }
	}
	if hostMatch(host, noBrowserCookies) {
		return CookieArg{}, none
	}
	all, err := c.browserExport()
	if err != nil || len(all) == 0 {
		return CookieArg{}, none
	}
	only := filterCookies(all, site)
	if only == nil {
		return CookieArg{}, none
	}
	cp, err := c.writeRunCopy(only)
	if err != nil {
		return CookieArg{}, none
	}
	return CookieArg{File: cp}, func() { _ = os.Remove(cp) }
}

// Has: a run on host would carry cookies (for "login needed" hints).
func (c *CookieStore) Has(host string) bool {
	if c == nil {
		return false
	}
	if c.For(host) != "" {
		return true
	}
	return c.browser != nil && !hostMatch(host, noBrowserCookies) && c.browser() != ""
}

func (c *CookieStore) writeRunCopy(data []byte) (string, error) {
	dir := c.dir + ".run"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "*.txt")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// writeBack keeps what the site renewed during the run: the copy's lines for
// the site replace the stored file in one rename (last run wins, nothing is
// ever half-written). A copy without the site's cookies is ignored.
func (c *CookieStore) writeBack(copyPath, stored, site string) {
	raw, err := os.ReadFile(copyPath)
	if err != nil {
		return
	}
	only := filterCookies(raw, site)
	if only == nil {
		return
	}
	old, _ := os.ReadFile(stored)
	if bytes.Equal(old, only) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = writeFileAtomic(stored, only, 0o600)
}

func (c *CookieStore) cleanRunCopies() { _ = os.RemoveAll(c.dir + ".run") }

// browserExport returns the browser's cookie jar in Netscape format, exported
// by yt-dlp and cached for a few minutes. It never leaves the data folder.
func (c *CookieStore) browserExport() ([]byte, error) {
	if c.browser == nil || c.ytdlp == nil {
		return nil, errors.New("tarayıcı çerezleri kapalı")
	}
	spec := c.browser()
	if spec == "" {
		return nil, errors.New("tarayıcı çerezleri kapalı")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.export != nil && c.exportSpec == spec && time.Since(c.exportAt) < browserExportTTL {
		return c.export, nil
	}
	bin := c.ytdlp()
	if bin == "" {
		return nil, errors.New("yt-dlp hazır değil")
	}
	dir := c.dir + ".run"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "browser-export.txt")
	_ = os.Remove(out)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// No URL: yt-dlp reads the browser, writes the jar to --cookies and stops
	// with "provide at least one URL" (exit 2) without touching the network.
	cmd := exec.CommandContext(ctx, bin, "--ignore-config", "--no-warnings", "--cookies-from-browser", spec, "--cookies", out)
	setProcAttrs(cmd)
	_ = cmd.Run()
	raw, err := os.ReadFile(out)
	_ = os.Remove(out)
	if err != nil || !bytes.Contains(raw, []byte("Netscape")) {
		return nil, errors.New("Firefox çerezleri okunamadı")
	}
	c.export, c.exportSpec, c.exportAt = raw, spec, time.Now()
	return raw, nil
}

// filterCookies keeps the Netscape header and the lines whose domain is site
// or below it; nil when none match.
func filterCookies(raw []byte, site string) []byte {
	var b bytes.Buffer
	b.WriteString("# Netscape HTTP Cookie File\n")
	n := 0
	for line := range strings.Lines(string(raw)) {
		f := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
		if len(f) < 7 {
			continue
		}
		d := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(f[0], "#HttpOnly_"), "."))
		if strings.HasPrefix(f[0], "#") && !strings.HasPrefix(f[0], "#HttpOnly_") {
			continue
		}
		if d == site || strings.HasSuffix(d, "."+site) {
			b.WriteString(strings.TrimRight(line, "\r\n") + "\n")
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return b.Bytes()
}

// ---- Firefox profile ---------------------------------------------------------

// FirefoxProfile is the profile in use: the one whose cookies changed last.
// Covers the usual, the Snap (Ubuntu/Kubuntu default) and the Flatpak install.
type FirefoxProfile struct {
	Dir      string    `json:"dir"`
	Install  string    `json:"install"` // deb | snap | flatpak
	Modified time.Time `json:"modified"`
}

func findFirefoxProfile() (FirefoxProfile, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return FirefoxProfile{}, false
	}
	roots := []struct{ dir, kind string }{
		{filepath.Join(home, ".mozilla", "firefox"), "deb"},
		{filepath.Join(home, "snap", "firefox", "common", ".mozilla", "firefox"), "snap"},
		{filepath.Join(home, ".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"), "flatpak"},
		{filepath.Join(home, ".config", "mozilla", "firefox"), "deb"}, // newer XDG layout
	}
	var found []FirefoxProfile
	for _, r := range roots {
		entries, _ := os.ReadDir(r.dir)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(r.dir, e.Name())
			if st, err := os.Stat(filepath.Join(p, "cookies.sqlite")); err == nil {
				found = append(found, FirefoxProfile{Dir: p, Install: r.kind, Modified: st.ModTime()})
			}
		}
	}
	if len(found) == 0 {
		return FirefoxProfile{}, false
	}
	slices.SortFunc(found, func(a, b FirefoxProfile) int { return b.Modified.Compare(a.Modified) })
	return found[0], true
}

// ---- settings -----------------------------------------------------------------

func (c *Config) Channel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.YtdlpChannel == "nightly" {
		return "nightly"
	}
	return "stable"
}

func (c *Config) SetChannel(ch string) error {
	if ch != "stable" && ch != "nightly" {
		return errors.New("kanal stable ya da nightly olmalı")
	}
	c.mu.Lock()
	c.YtdlpChannel = ch
	c.mu.Unlock()
	return c.Save()
}

func (c *Config) BrowserCookiesOn() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BrowserCookies == "firefox"
}

func (c *Config) SetBrowserCookies(on bool) error {
	c.mu.Lock()
	c.BrowserCookies = map[bool]string{true: "firefox", false: ""}[on]
	c.mu.Unlock()
	return c.Save()
}

// browserCookieSource is CookieStore.browser: the Firefox profile to read,
// looked up at most once a minute.
func browserCookieSource(cfg *Config) func() string {
	var mu sync.Mutex
	var at time.Time
	var val string
	return func() string {
		if !cfg.BrowserCookiesOn() || inDocker() {
			return ""
		}
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > time.Minute {
			val, at = "", time.Now()
			if p, ok := findFirefoxProfile(); ok {
				val = "firefox:" + p.Dir
			}
		}
		return val
	}
}

type BrowserCookieInfo struct {
	Enabled   bool            `json:"enabled"`
	Available bool            `json:"available"` // not in Docker: no browser there
	Profile   *FirefoxProfile `json:"profile,omitempty"`
}

func (s *Server) browserCookieInfo() BrowserCookieInfo {
	bi := BrowserCookieInfo{Enabled: s.cfg.BrowserCookiesOn(), Available: !inDocker()}
	if bi.Available {
		if p, ok := findFirefoxProfile(); ok {
			bi.Profile = &p
		}
	}
	return bi
}

func (s *Server) handleBrowserCookies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if body.Enabled && inDocker() {
		writeErr(w, 400, "Docker'da tarayıcı yok: cookies.txt içe aktar")
		return
	}
	if err := s.cfg.SetBrowserCookies(body.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, s.browserCookieInfo())
}

func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel string `json:"channel"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if err := s.cfg.SetChannel(body.Channel); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// A rolled-back yt-dlp is pinned; choosing a channel means "follow it".
	s.eng.setState(EngYTDLP, func(st *EngineState) { st.Held = false })
	go s.eng.UpdateAllWait(s.ctx) // the other channel's release differs: yt-dlp is swapped
	writeJSON(w, 202, map[string]string{"channel": body.Channel})
}
