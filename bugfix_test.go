package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// "Delete with files" on a second run of the same profile must not take the
// first run's files.
func TestRemoveKeepsFilesOfOtherJobs(t *testing.T) {
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	shared := filepath.Join(s.cfg.Library, "IG", "a.jpg")
	own := filepath.Join(s.cfg.Library, "IG", "b.jpg")
	os.MkdirAll(filepath.Dir(shared), 0o755)
	os.WriteFile(shared, []byte("x"), 0o644)
	os.WriteFile(own, []byte("y"), 0o644)
	s.queue.jobs["first"] = &Job{ID: "first", Status: StDone, Files: []string{shared}, Opts: JobOptions{Mode: "library"}}
	s.queue.jobs["second"] = &Job{ID: "second", Status: StDone, Files: []string{shared, own}, Opts: JobOptions{Mode: "library"}}
	if err := s.queue.Remove("second", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatal("deleted a file another job owns")
	}
	if _, err := os.Stat(own); err == nil {
		t.Fatal("own file kept")
	}
}

func TestGallerySkippedLine(t *testing.T) {
	if p, skipped := parseGalleryLine("# /lib/IG/a.jpg"); p != "/lib/IG/a.jpg" || !skipped {
		t.Fatalf("%q %v", p, skipped)
	}
	if p, skipped := parseGalleryLine("/lib/IG/b.jpg"); p != "/lib/IG/b.jpg" || skipped {
		t.Fatalf("%q %v", p, skipped)
	}
}

// A crash during a download must not cost a retry.
func TestCrashRestartKeepsAttempts(t *testing.T) {
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	raw, _ := json.Marshal([]*Job{{ID: "x", URL: "https://a.b/c", Status: StRunning, Attempts: 2}})
	os.WriteFile(cfg.Dir("jobs.json"), raw, 0o600)
	q := NewQueue(cfg, nil, nil, nil, NewHub())
	if j := q.jobs["x"]; j.Status != StQueued || j.Attempts != 1 {
		t.Fatalf("%+v", j)
	}
}

// "Download to phone" answers at once when the job is deleted meanwhile.
func TestWaitEndsWhenJobDeleted(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		req := httptest.NewRequest("POST", "/api/add?wait=1", strings.NewReader("https://youtu.be/cccccccccc1"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		done <- rec
	}()
	var id string
	for range 50 {
		time.Sleep(20 * time.Millisecond)
		if l := s.queue.List(); len(l) == 1 {
			id = l[0].ID
			break
		}
	}
	s.queue.Remove(id, false)
	select {
	case rec := <-done:
		if rec.Code != 410 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still waiting after the job was deleted")
	}
}

// Parallel saves must leave every change on disk.
func TestConcurrentSavesKeepAllChanges(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			cfg.SetTelegram(func(tc *TelegramConfig) { tc.Chats = append(tc.Chats, int64(i)) })
		}()
		go func() { defer wg.Done(); cfg.SetLayout([]string{"site", "flat"}[i%2]) }()
	}
	wg.Wait()
	again, _ := LoadConfig(cfg.dataDir)
	if len(again.Telegram.Chats) != 20 {
		t.Fatalf("lost chats: %v", again.Telegram.Chats)
	}
}

// Each run gets its own copy of the cookies; the stored file is never written.
func TestCookieRunCopies(t *testing.T) {
	c := NewCookieStore(filepath.Join(t.TempDir(), "cookies"))
	os.WriteFile(c.path("instagram.com"), []byte("# Netscape HTTP Cookie File\n"), 0o600)
	a, doneA := c.Use("www.instagram.com")
	b, doneB := c.Use("instagram.com")
	if a.File == "" || a.File == b.File || a.File == c.path("instagram.com") {
		t.Fatalf("no private copies: %q %q", a.File, b.File)
	}
	os.WriteFile(a.File, []byte("mangled by the engine"), 0o600)
	doneA()
	doneB()
	if raw, _ := os.ReadFile(c.path("instagram.com")); !strings.HasPrefix(string(raw), "# Netscape") {
		t.Fatal("stored cookies changed")
	}
	if _, err := os.Stat(a.File); err == nil {
		t.Fatal("copy left behind")
	}
	// What the site renewed during the run is kept, other domains are not.
	a, doneA = c.Use("instagram.com")
	os.WriteFile(a.File, []byte("# Netscape HTTP Cookie File\n.instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tNEW\n.tracker.example\tTRUE\t/\tFALSE\t0\tid\tX\n"), 0o600)
	doneA()
	if raw, _ := os.ReadFile(c.path("instagram.com")); !strings.Contains(string(raw), "NEW") || strings.Contains(string(raw), "tracker") {
		t.Fatalf("write-back: %s", raw)
	}
}

// A forced reinstall of the same release keeps the working copy as rollback.
func TestForcedReinstallKeepsRollback(t *testing.T) {
	gh, m := setup(t)
	publishAll(t, gh, "2026.10.01", false)
	m.UpdateAll(context.Background(), false)
	first := m.States()[0]
	m.UpdateAll(context.Background(), true)
	s := m.States()[0]
	if s.Dir == first.Dir || s.Previous != first.Dir || !s.Available {
		t.Fatalf("forced reinstall: %+v", s)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatal("previous copy deleted")
	}
	if _, err := os.Stat(s.Path); err != nil {
		t.Fatal("new copy missing")
	}
}

// Browser cookies: the run gets only its site's lines from the exported jar,
// never Google/YouTube ones, and nothing for YouTube itself.
func TestBrowserCookiesFiltered(t *testing.T) {
	dir := t.TempDir()
	c := NewCookieStore(filepath.Join(dir, "cookies"))
	fake := filepath.Join(dir, "yt-dlp")
	// Like yt-dlp without a URL: writes the jar to --cookies, exits 2.
	os.WriteFile(fake, []byte(`#!/bin/sh
while [ $# -gt 0 ]; do [ "$1" = "--cookies" ] && out="$2"; shift; done
printf '# Netscape HTTP Cookie File\n.instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tIG\n#HttpOnly_.x.com\tTRUE\t/\tTRUE\t0\tauth_token\tXT\n.youtube.com\tTRUE\t/\tTRUE\t0\tSID\tYT\n.google.com\tTRUE\t/\tTRUE\t0\tSID\tG\n' > "$out"
echo calls >> "$(dirname "$0")/calls"
exit 2
`), 0o755)
	c.ytdlp = func() string { return fake }
	c.browser = func() string { return "firefox:/p" }
	read := func(host string) string {
		a, done := c.Use(host)
		defer done()
		if a.File == "" {
			return ""
		}
		b, _ := os.ReadFile(a.File)
		return string(b)
	}
	ig := read("www.instagram.com")
	if !strings.Contains(ig, "sessionid\tIG") || strings.Contains(ig, "YT") || strings.Contains(ig, "XT") || strings.Contains(ig, "\tG\n") {
		t.Fatalf("instagram run got: %q", ig)
	}
	if x := read("x.com"); !strings.Contains(x, "#HttpOnly_.x.com") || strings.Contains(x, "IG") {
		t.Fatalf("x run got: %q", x)
	}
	if y := read("www.youtube.com"); y != "" {
		t.Fatalf("youtube run got cookies: %q", y)
	}
	if o := read("vimeo.com"); o != "" {
		t.Fatalf("site without cookies got a file: %q", o)
	}
	if calls, _ := os.ReadFile(filepath.Join(dir, "calls")); strings.Count(string(calls), "calls") != 1 {
		t.Fatalf("export not cached: %d runs", strings.Count(string(calls), "calls"))
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "cookies.run"))
	if len(entries) != 0 {
		t.Fatalf("run copies left: %v", entries)
	}
}

// With a real yt-dlp and a Firefox profile (INDIRGEN_TEST_FIREFOX=<profile dir>).
func TestBrowserCookiesRealYTDLP(t *testing.T) {
	prof := os.Getenv("INDIRGEN_TEST_FIREFOX")
	bin, err := exec.LookPath("yt-dlp")
	if prof == "" || err != nil {
		t.Skip("INDIRGEN_TEST_FIREFOX and yt-dlp needed")
	}
	c := NewCookieStore(filepath.Join(t.TempDir(), "cookies"))
	c.ytdlp = func() string { return bin }
	c.browser = func() string { return "firefox:" + prof }
	a, done := c.Use("instagram.com")
	defer done()
	b, _ := os.ReadFile(a.File)
	if !strings.Contains(string(b), "sessionid") || strings.Contains(string(b), "youtube") {
		t.Fatalf("%q", b)
	}
}

// A failed or canceled yt-dlp run removes the empty folders it created, and
// only those: a folder the user made (even an empty one) stays.
func TestFailedRunRemovesOnlyItsEmptyFolders(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses a shell script as a fake yt-dlp")
	}
	bin := t.TempDir()
	// Prints the planned paths like --print pre_process, then creates their
	// folders (as yt-dlp does before downloading), maybe writes a file, exits.
	script := `#!/bin/sh
[ "$1" = "--version" ] && { echo 2026.10.26; exit 0; }
for p in $FAKE_TARGETS; do echo "SGR:D|$p"; done
sleep 0.3
for p in $FAKE_TARGETS; do mkdir -p "$(dirname "$p")"; done
[ -n "$FAKE_WRITE" ] && echo x > "$FAKE_WRITE"
sleep ${FAKE_SLEEP:-0}
exit ${FAKE_EXIT:-1}
`
	if err := os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	cfg.Library = t.TempDir()
	eng := NewEngineManager(cfg)
	q := NewQueue(cfg, eng, &Router{eng: eng}, NewCookieStore(cfg.Dir("cookies")), NewHub())
	lib := func(rel string) string { return filepath.Join(cfg.Library, filepath.FromSlash(rel)) }
	isDir := func(rel string) bool { st, err := os.Stat(lib(rel)); return err == nil && st.IsDir() }
	mkdir := func(rel string) { os.MkdirAll(lib(rel), 0o755) }
	run := func(ctx context.Context, env map[string]string) error {
		for k, v := range env {
			t.Setenv(k, v)
		}
		j := Job{ID: "j1", URL: "https://example.com/v", Host: "example.com", Opts: JobOptions{Preset: "best", Mode: "library"}}
		return q.runYTDLP(ctx, j, cfg.Library)
	}

	mkdir("Kendi/Bos")                 // the user's empty folder, used as a target
	mkdir("Bos2")                      // the user's empty folder, a new one goes inside
	os.MkdirAll(lib("Youtube"), 0o755) // a site folder with a file in it
	os.WriteFile(lib("Youtube/eski.mkv"), []byte("x"), 0o644)
	targets := []string{lib("Generic/Unknown/a [x].mp4"), lib("Kendi/Bos/b [y].mp4"),
		lib("Youtube/Blender/c [z].mkv"), lib("Bos2/Yeni/d [w].mp4")}
	if err := run(context.Background(), map[string]string{"FAKE_TARGETS": strings.Join(targets, " "), "FAKE_EXIT": "1"}); err == nil {
		t.Fatal("fake failure not reported")
	}
	for rel, want := range map[string]bool{
		"Generic": false, "Generic/Unknown": false, // created by the run: gone
		"Youtube/Blender": false, "Youtube": true, // the run's subfolder goes, the site folder stays
		"Kendi/Bos": true,                // the user's empty folder stays
		"Bos2/Yeni": false, "Bos2": true, // the user's empty parent stays
	} {
		if isDir(rel) != want {
			t.Errorf("%s exists=%v, want %v", rel, isDir(rel), want)
		}
	}

	// Success: the folder holds the file and stays.
	ok := lib("Vimeo/Kanal/e [v].mp4")
	if err := run(context.Background(), map[string]string{"FAKE_TARGETS": ok, "FAKE_WRITE": ok, "FAKE_EXIT": "0"}); err != nil {
		t.Fatal(err)
	}
	if !isDir("Vimeo/Kanal") {
		t.Fatal("folder of a downloaded file removed")
	}

	// Canceled while downloading: its new folders go too.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	if err := run(ctx, map[string]string{"FAKE_TARGETS": lib("Iptal/Kanal/f [u].mp4"), "FAKE_WRITE": "", "FAKE_SLEEP": "10"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if isDir("Iptal") {
		t.Fatal("canceled run left its folder")
	}
}

func TestClaimNewDir(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "A"), 0o755)
	if c, ok := claimNewDir(root, filepath.Join(root, "A", "B", "C", "f.mp4")); !ok ||
		c.dir != filepath.Join(root, "A", "B", "C") || c.stop != filepath.Join(root, "A") {
		t.Fatalf("%+v %v", c, ok)
	}
	for _, f := range []string{
		filepath.Join(root, "A", "f.mp4"),   // folder exists already
		filepath.Join(root, "f.mp4"),        // the root itself
		"/elsewhere/X/f.mp4",                // outside the root
		filepath.Join(root, "..", "X", "f"), // escapes the root
	} {
		if c, ok := claimNewDir(root, f); ok {
			t.Errorf("%s claimed: %+v", f, c)
		}
	}
}

// Every published change raises the job's rev; the web UI keeps the higher
// one when a request's reply and a live event cross.
func TestJobRevisionGrowsWithEachEvent(t *testing.T) {
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)
	v, err := s.queue.Add(NewJob{URL: "https://youtu.be/dQw4w9WgXcQ", Engine: EngYTDLP})
	if err != nil {
		t.Fatal(err)
	}
	s.queue.update(v.ID, true, func(j *Job) { j.Status, j.Phase = StRunning, "starting" })
	s.queue.update(v.ID, true, func(j *Job) { j.Phase = "downloading" })
	var revs []int64
	for len(revs) < 3 {
		select {
		case msg := <-ch:
			if data, ok := strings.CutPrefix(string(msg), "event: job\ndata: "); ok {
				var j JobView
				json.Unmarshal([]byte(strings.TrimSpace(data)), &j)
				revs = append(revs, j.Rev)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("events: %v", revs)
		}
	}
	if v.Rev != revs[0] || revs[1] <= revs[0] || revs[2] <= revs[1] {
		t.Fatalf("reply rev %d, event revs %v", v.Rev, revs)
	}
	if l := s.queue.List(); l[0].Rev != revs[2] {
		t.Fatalf("list rev %d, last event %d", l[0].Rev, revs[2])
	}
}
