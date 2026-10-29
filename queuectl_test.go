package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNightWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 27, h, m, 0, 0, time.Local) }
	cases := []struct {
		h, m       int
		start, end string
		in         bool
	}{
		{0, 30, "01:00", "07:00", false}, {1, 0, "01:00", "07:00", true}, {6, 59, "01:00", "07:00", true}, {7, 0, "01:00", "07:00", false},
		{23, 30, "23:00", "06:00", true}, {3, 0, "23:00", "06:00", true}, {12, 0, "23:00", "06:00", false},
	}
	for _, c := range cases {
		if got := inWindow(at(c.h, c.m), c.start, c.end); got != c.in {
			t.Errorf("%02d:%02d in %s–%s = %v", c.h, c.m, c.start, c.end, got)
		}
	}
	if n := nextStart(at(12, 0), "01:00", "07:00"); !n.Equal(at(1, 0).AddDate(0, 0, 1)) {
		t.Fatalf("next %v", n)
	}
	if n := nextStart(at(0, 10), "01:00", "07:00"); !n.Equal(at(1, 0)) {
		t.Fatalf("next same day %v", n)
	}
	// Inside the window the value must not move with the clock.
	if a, b := nextStart(at(3, 0), "01:00", "07:00"), nextStart(at(3, 1), "01:00", "07:00"); !a.Equal(b) || !a.Equal(at(1, 0)) {
		t.Fatalf("moving while open: %v %v", a, b)
	}
	if n := nextStart(at(2, 0), "23:00", "06:00"); !n.Equal(at(23, 0).AddDate(0, 0, -1)) {
		t.Fatalf("across midnight %v", n)
	}
	if _, ok := parseHM("24:00"); ok {
		t.Fatal("24:00 accepted")
	}
}

func TestRateLimitArgs(t *testing.T) {
	if r := rateFor(0, 3); r != "" {
		t.Fatal(r)
	}
	if r := rateFor(6, 3); r != "2048K" {
		t.Fatal(r)
	}
	if r := rateFor(1, 100); r != "64K" {
		t.Fatal("floor", r)
	}
	a := strings.Join(ytdlpArgs(JobOptions{Preset: "best"}, ytdlpPaths{RateLimit: "2048K"}, "https://a.b/c"), " ")
	g := strings.Join(gallerydlArgs(JobOptions{}, gallerydlRun{Dir: "/d", RateLimit: "2048K", URLs: []string{"https://a.b"}}), " ")
	if !strings.Contains(a, "--limit-rate 2048K") || !strings.Contains(g, "-r 2048K") {
		t.Fatalf("%s\n%s", a, g)
	}
}

func TestSchedulerHolds(t *testing.T) {
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	q := s.queue
	now := time.Now()
	// A window that is surely closed now: it opens in two hours for one hour.
	start, end := now.Add(2*time.Hour).Format("15:04"), now.Add(3*time.Hour).Format("15:04")
	if err := s.cfg.SetQueueSettings(QueueSettings{NightStart: start, NightEnd: end, MinFreeMB: 100}); err != nil {
		t.Fatal(err)
	}
	v, _ := q.Add(NewJob{URL: "https://youtu.be/aaaaaaaaaaa", Opts: JobOptions{Night: true}})
	q.schedule()
	if j, _ := q.Get(v.ID); j.Status != StQueued {
		t.Fatalf("night job started: %s", j.Status)
	}
	if st := q.State(); st.Waiting != 1 || st.Night || st.DiskLow || !st.NextNight.After(now) {
		t.Fatalf("state %+v", st)
	}
	if err := q.StartNow(v.ID); err != nil {
		t.Fatal(err)
	}
	if j, _ := q.Get(v.ID); j.Opts.Night {
		t.Fatal("StartNow kept the hold")
	}

	// Disk: a minimum no disk has → held, banner state set, hook called once.
	called := make(chan QueueState, 2)
	q.onDiskLow = func(st QueueState) { called <- st }
	s.cfg.SetQueueSettings(QueueSettings{NightStart: start, NightEnd: end, MinFreeMB: 1 << 20})
	q.schedule()
	q.schedule()
	if j, _ := q.Get(v.ID); j.Status != StQueued {
		t.Fatalf("started on a full disk: %s", j.Status)
	}
	select {
	case st := <-called:
		if !st.DiskLow || st.MinFreeMB != 1<<20 {
			t.Fatalf("%+v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("no disk warning")
	}
	if len(called) != 0 {
		t.Fatal("warned twice")
	}

	// A run that finds the disk full waits instead of failing.
	q.mu.Lock()
	q.jobs[v.ID].Status, q.jobs[v.ID].Attempts = StRunning, 1
	q.mu.Unlock()
	q.finish(context.Background(), v.ID, errDiskLow)
	if j, _ := q.Get(v.ID); j.Status != StQueued || j.Attempts != 0 || j.Error != "" {
		t.Fatalf("disk-low finish: %+v", j)
	}
}

func TestQueueSettingsAPI(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/queue", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if r := post(`{"rate_limit_mb":5,"night_start":"02:00","night_end":"06:30","min_free_mb":2048}`); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if qs := s.cfg.QueueSettings(); qs.RateLimitMB != 5 || qs.NightEnd != "06:30" || qs.MinFreeMB != 2048 {
		t.Fatalf("%+v", qs)
	}
	for _, bad := range []string{`{"rate_limit_mb":-1,"night_start":"02:00","night_end":"06:00","min_free_mb":2048}`,
		`{"night_start":"2:00","night_end":"06:00","min_free_mb":2048}`, `{"night_start":"02:00","night_end":"02:00","min_free_mb":2048}`,
		`{"night_start":"02:00","night_end":"06:00","min_free_mb":5}`} {
		if r := post(bad); r.Code != 400 {
			t.Errorf("accepted %s", bad)
		}
	}
	// Shortcut / extension: night=1
	req := httptest.NewRequest("POST", "/api/add?night=1", strings.NewReader("https://youtu.be/bbbbbbbbbbb"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res addResponse
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || !res.Jobs[0].Opts.Night || !strings.HasPrefix(res.Message, "Gece indirilecek") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestBackupRestore(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	s.cfg.Telegram.Chats = []int64{42}
	s.cfg.Save()
	os.MkdirAll(s.cfg.Dir("archive"), 0o755)
	os.WriteFile(s.cfg.Dir("archive", "yt-dlp.txt"), []byte("youtube abc\n"), 0o600)
	os.WriteFile(s.cfg.Dir("subscriptions.json"), []byte(`[{"id":"s1","url":"https://www.youtube.com/@k"}]`), 0o600)
	os.MkdirAll(s.cfg.Dir("cookies"), 0o700)
	os.WriteFile(s.cookies.path("instagram.com"), []byte("# Netscape HTTP Cookie File\n"), 0o600)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/backup", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Disposition"), "indirgen-yedek-") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	backup := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(backup), int64(len(backup)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "manifest.json,config.json,subscriptions.json,archive/yt-dlp.txt,cookies/instagram.com.txt" {
		t.Fatalf("contents %s", got)
	}

	// Another installation (Docker): different listen/library, nothing else.
	s2, h2 := testServer(t, "never", "0.0.0.0:8899")
	os.MkdirAll(s2.cfg.Dir("cookies"), 0o700)
	os.WriteFile(s2.cookies.path("x.com"), []byte("old"), 0o600)
	req := httptest.NewRequest("POST", "/api/backup", bytes.NewReader(backup))
	req.Header.Set("Content-Type", "application/zip")
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("restore %d %s", rec.Code, rec.Body)
	}
	cfg2, err := LoadConfig(s2.cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Listen != "0.0.0.0:8899" || cfg2.Library != s2.cfg.Library || cfg2.Token != s.cfg.Token || len(cfg2.Telegram.Chats) != 1 {
		t.Fatalf("merged config: listen %s library %s token ok %v chats %v", cfg2.Listen, cfg2.Library, cfg2.Token == s.cfg.Token, cfg2.Telegram.Chats)
	}
	if b, _ := os.ReadFile(s2.cfg.Dir("archive", "yt-dlp.txt")); string(b) != "youtube abc\n" {
		t.Fatal("archive not restored")
	}
	if _, err := os.Stat(s2.cookies.path("x.com")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old cookies kept")
	}
	if _, err := os.Stat(s2.cookies.path("instagram.com")); err != nil {
		t.Fatal("cookies not restored")
	}

	// Hostile or foreign ZIPs write nothing.
	bad := func(files map[string]string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for n, c := range files {
			f, _ := zw.Create(n)
			io.WriteString(f, c)
		}
		zw.Close()
		return buf.Bytes()
	}
	man := `{"app":"indirgen"}`
	for name, b := range map[string][]byte{
		"traversal": bad(map[string]string{"manifest.json": man, "../../evil": "x"}),
		"cookieDir": bad(map[string]string{"manifest.json": man, "cookies/../config.json": "{}"}),
		"engines":   bad(map[string]string{"manifest.json": man, "engines/yt-dlp": "x"}),
		"foreign":   bad(map[string]string{"config.json": "{}"}),
		"badSubs":   bad(map[string]string{"manifest.json": man, "subscriptions.json": "{"}),
		"notZip":    []byte("hello"),
	} {
		if _, err := s2.restore(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(s2.cfg.dataDir, "..", "evil")); err == nil {
		t.Fatal("wrote outside")
	}
}
