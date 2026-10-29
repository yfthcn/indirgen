package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExtractURLs(t *testing.T) {
	got := extractURLs("Şuna bak! https://youtu.be/abc123. Bir de (https://www.instagram.com/p/XyZ12345/) ve tekrar https://youtu.be/abc123 ftp://x javascript:alert(1)", 10)
	want := []string{"https://youtu.be/abc123", "https://www.instagram.com/p/XyZ12345/"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}

func testServer(t *testing.T, auth, listen string) (*Server, http.Handler) {
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	cfg.Auth, cfg.Listen, cfg.Library = auth, listen, t.TempDir()
	cfg.fileListen = listen
	cfg.AllowedHosts = []string{"example.com"} // httptest's default Host
	hub := NewHub()
	q := NewQueue(cfg, nil, nil, nil, hub)
	s := &Server{cfg: cfg, queue: q, hub: hub, lib: NewLibrary(cfg.Library), cookies: NewCookieStore(cfg.Dir("cookies")), ctx: context.Background(),
		sessions: NewSessionStore(cfg.Dir("sessions.json"))}
	s.subs = NewSubs(cfg, q, hub)
	return s, s.Handler()
}

func TestAddEndpoint(t *testing.T) {
	s, h := testServer(t, "auto", "0.0.0.0:8899") // network open: token needed
	post := func(ct, body, token, origin, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/add", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if remote != "" {
			req.RemoteAddr = remote
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if r := post("text/plain", "https://youtu.be/a1", "", "", ""); r.Code != 401 {
		t.Fatalf("no token from the network: %d", r.Code)
	}
	// iOS Shortcut: plain text from the share sheet, token, no Origin.
	r := post("text/plain", "Bak: https://youtu.be/a1 ve https://youtu.be/b2", s.cfg.Token, "", "")
	var res addResponse
	json.Unmarshal(r.Body.Bytes(), &res)
	if r.Code != 200 || res.Added != 2 || res.Jobs[0].Engine != "" || !res.Jobs[0].Opts.Embed {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	// Extension: JSON, extension origin, temp mode.
	r = post("application/json", `{"url":"https://x.com/a/status/1","mode":"temp"}`, s.cfg.Token, "chrome-extension://abcdef", "")
	json.Unmarshal(r.Body.Bytes(), &res)
	if r.Code != 200 || res.Jobs[0].Opts.Mode != "temp" {
		t.Fatalf("extension: %d %s", r.Code, r.Body)
	}
	// This computer's browser needs no token, but a web page's Origin is refused.
	if r := post("text/plain", "https://youtu.be/c3", "", "", "127.0.0.1:5555"); r.Code != 200 {
		t.Fatalf("local without token: %d", r.Code)
	}
	if r := post("text/plain", "https://youtu.be/c3", "", "https://evil.example", "127.0.0.1:5555"); r.Code != 403 {
		t.Fatalf("cross-site POST from a web page: %d", r.Code)
	}
	if r := post("text/plain", "https://youtu.be/c3", "", "moz-extension://uuid", "127.0.0.1:5555"); r.Code != 200 {
		t.Fatalf("firefox extension: %d", r.Code)
	}
	// A proxy on this machine (cloudflared) is not "local".
	req := httptest.NewRequest("POST", "/api/add", strings.NewReader("https://youtu.be/d4"))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Cf-Connecting-Ip", "203.0.113.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("proxied request treated as local: %d", rec.Code)
	}
	if r := post("text/plain", "link yok", s.cfg.Token, "", ""); r.Code != 400 {
		t.Fatalf("no link: %d", r.Code)
	}
}

func TestConnectInfoAndToggle(t *testing.T) {
	orig := inDocker
	inDocker = func() bool { return false }
	defer func() { inDocker = orig }()
	s, _ := testServer(t, "auto", "127.0.0.1:8899")
	if ci := s.connectInfo(); ci.LAN || len(ci.URLs) != 1 || !ci.CanEdit {
		t.Fatalf("%+v", ci)
	}
	if err := s.setLAN(true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.cfg.dataDir, "config.json"))
	if !strings.Contains(string(raw), `"listen": "0.0.0.0:8899"`) {
		t.Fatalf("config not updated: %s", raw)
	}
	s.cfg.Listen = "0.0.0.0:8899"
	if ci := s.connectInfo(); !ci.LAN {
		t.Fatalf("%+v", ci)
	}
	s.cfg.Listen = "0.0.0.0:9999" // -listen flag in use
	if !s.cfg.ListenOverridden() {
		t.Fatal("flag override not detected")
	}
}

func TestSubscriptionArgs(t *testing.T) {
	a := strings.Join(ytdlpArgs(JobOptions{Preset: "best", NewOnly: true, Limit: 10, Playlist: true},
		ytdlpPaths{Home: "/h", Temp: "/t", Archive: "/d/archive/yt-dlp.txt"}, "https://www.youtube.com/@kanal"), " ")
	for _, want := range []string{"--download-archive /d/archive/yt-dlp.txt", "--playlist-items 1:10", "--yes-playlist"} {
		if !strings.Contains(a, want) {
			t.Fatalf("missing %q in %s", want, a)
		}
	}
	if strings.Contains(a, "--break-on-existing") {
		t.Fatal("break-on-existing stops a channel's other tabs")
	}
	if a := strings.Join(ytdlpArgs(JobOptions{Preset: "best"}, ytdlpPaths{Archive: "/x"}, "u"), " "); strings.Contains(a, "--download-archive") {
		t.Fatal("archive used without only-new")
	}
	g := strings.Join(gallerydlArgs(JobOptions{NewOnly: true, Limit: 25, Sections: []string{"posts"}},
		gallerydlRun{Dir: "/h", Archive: "/a.sqlite3", URLs: []string{"u"}, Pace: Pacing{MaxItems: 100}}), " ")
	if !strings.Contains(g, "--post-range 1-25") || !strings.Contains(g, "--download-archive /a.sqlite3") {
		t.Fatalf("gallery-dl profile: %s", g)
	}
	if g := strings.Join(gallerydlArgs(JobOptions{Limit: 5}, gallerydlRun{Dir: "/h", URLs: []string{"u"}}), " "); !strings.Contains(g, "--range 1-5") {
		t.Fatalf("gallery-dl generic: %s", g)
	}
}

func TestSubscriptionSchedule(t *testing.T) {
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	clock := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s.subs.now = func() time.Time { return clock }

	x, err := s.subs.Add(NewSub{URL: "https://www.youtube.com/@kanal/videos", Engine: EngYTDLP, Title: "Kanal", IntervalH: 5,
		Opts: JobOptions{Preset: "compat", Limit: 12}})
	if err != nil {
		t.Fatal(err)
	}
	if x.IntervalH != 6 || x.Opts.Limit != 10 || !x.Opts.NewOnly || !x.Opts.Playlist || x.Opts.Preset != "compat" {
		t.Fatalf("normalized: %+v", x)
	}
	jobsOf := func() []*Job {
		var out []*Job
		for _, j := range s.queue.jobs {
			if j.Sub == x.ID {
				out = append(out, j)
			}
		}
		return out
	}
	s.subs.tick()
	if js := jobsOf(); len(js) != 1 || !js[0].Opts.NewOnly || js[0].Opts.Limit != 10 {
		t.Fatalf("first check: %+v", js)
	}
	// While that check is queued, no second one.
	clock = clock.Add(7 * time.Hour)
	s.subs.tick()
	if len(jobsOf()) != 1 {
		t.Fatal("second check while the first is still queued")
	}
	// It finishes with 3 new files; the next due tick queues another check.
	j := jobsOf()[0]
	j.Status, j.Files = StDone, []string{"a", "b", "c"}
	s.subs.jobFinished(*j)
	s.subs.tick()
	if len(jobsOf()) != 2 {
		t.Fatal("due check not queued")
	}
	got := s.subs.List()[0]
	if got.LastNew != 3 || got.TotalNew != 3 || !got.Running {
		t.Fatalf("stats: %+v", got)
	}
	// Paused: nothing is queued; same URL again updates instead of duplicating.
	paused := true
	s.subs.Patch(x.ID, SubPatch{Paused: &paused})
	for _, j := range jobsOf() {
		j.Status = StDone
	}
	clock = clock.Add(48 * time.Hour)
	s.subs.tick()
	if len(jobsOf()) != 2 {
		t.Fatal("paused subscription checked")
	}
	s.subs.Add(NewSub{URL: "https://www.youtube.com/@kanal/videos", Engine: EngYTDLP, Title: "Kanal"})
	if n := len(s.subs.List()); n != 1 {
		t.Fatalf("duplicate subscription: %d", n)
	}
	// Survives a restart.
	again := NewSubs(s.cfg, s.queue, s.hub)
	if l := again.List(); len(l) != 1 || l[0].TotalNew != 3 {
		t.Fatalf("not persisted: %+v", l)
	}
}

func TestOnlyNewEmptyYTDLPIsDone(t *testing.T) {
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	ctx := t.Context()
	s.queue.jobs["y"] = &Job{ID: "y", Engine: EngYTDLP, Status: StRunning, Opts: JobOptions{NewOnly: true}}
	s.queue.finish(ctx, "y", nil)
	if j := s.queue.jobs["y"]; j.Status != StDone || j.Note != "no_new" {
		t.Fatalf("%+v", j)
	}
}

func TestBulkDelete(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	lib := s.cfg.Library
	for _, p := range []string{"A/1.mp4", "A/2.mp4", "B/3.mp4", "B/4.mp4", "C/5.mp4"} {
		os.MkdirAll(filepath.Join(lib, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(lib, p), []byte("12345"), 0o644)
	}
	body := `{"paths":["A","A/1.mp4","B/3.mp4","../C","yok.mp4"]}`
	req := httptest.NewRequest("POST", "/api/library/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res struct {
		Files  int
		Bytes  int64
		Failed []struct{ Path, Error string }
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	// A (2 files) + B/3.mp4; "../C" cleans to "C" inside the library; yok.mp4 fails.
	if rec.Code != 200 || res.Files != 4 || res.Bytes != 20 || len(res.Failed) != 1 || res.Failed[0].Path != "yok.mp4" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(lib, "B", "4.mp4")); err != nil {
		t.Fatal("unselected file deleted")
	}
}

func TestErrorsCarryMessage(t *testing.T) {
	_, h := testServer(t, "auto", "0.0.0.0:8899")
	req := httptest.NewRequest("POST", "/api/add", strings.NewReader("https://youtu.be/a"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]string
	json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != 401 || m["error"] == "" || !strings.HasPrefix(m["message"], "İndirgen: ") {
		t.Fatalf("%d %v", rec.Code, m)
	}
}
