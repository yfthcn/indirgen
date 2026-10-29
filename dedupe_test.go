package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMediaKey(t *testing.T) {
	same := map[string][]string{
		"youtube:dQw4w9WgXcQ": {
			"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			"https://youtu.be/dQw4w9WgXcQ?si=AbCdEf123",
			"https://m.youtube.com/watch?v=dQw4w9WgXcQ&list=RDdQw4w9WgXcQ&t=42",
			"https://www.youtube.com/shorts/dQw4w9WgXcQ",
			"https://music.youtube.com/watch?v=dQw4w9WgXcQ&feature=share",
		},
		"instagram:DeHgyYjjK3o": {
			"https://www.instagram.com/p/DeHgyYjjK3o/",
			"https://www.instagram.com/reel/DeHgyYjjK3o/?igsh=MWx0",
			"https://instagram.com/natgeo/p/DeHgyYjjK3o",
		},
		"tiktok:7700364571122486550": {
			"https://www.tiktok.com/@ramona2002/video/7700364571122486550",
			"https://www.tiktok.com/@ramona2002/video/7700364571122486550?is_from_webapp=1&sender_device=pc",
		},
		"x:1850000000000000000": {
			"https://x.com/someone/status/1850000000000000000",
			"https://twitter.com/someone/status/1850000000000000000?s=20",
			"https://x.com/i/web/status/1850000000000000000",
			"https://x.com/someone/status/1850000000000000000/photo/1",
		},
	}
	for want, links := range same {
		for _, l := range links {
			if k, _, ok := mediaKey(l); !ok || k != want {
				t.Errorf("%s → %q (%v), want %s", l, k, ok, want)
			}
		}
	}
	// Profiles, channels and playlists may have new items: never "already downloaded".
	for _, l := range []string{
		"https://www.youtube.com/@kanal/videos", "https://www.youtube.com/playlist?list=PLx",
		"https://www.instagram.com/natgeo/", "https://www.tiktok.com/@ramona2002", "https://x.com/someone",
		"https://example.com/video.mp4", "https://www.youtube.com/watch?v=short",
	} {
		if k, _, ok := mediaKey(l); ok {
			t.Errorf("%s treated as one item (%s)", l, k)
		}
	}
}

func TestTelegramAlreadyDownloaded(t *testing.T) {
	fake := newFakeTG(t)
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	s.cfg.Telegram = TelegramConfig{Token: tgTestToken, Chats: []int64{42}}
	s.tg = NewTelegram(s.cfg, s.queue, s.lib, fake.srv.URL)
	ctx := context.Background()
	msg := func(id int, text string) *tgMessage { m := &tgMessage{ID: id, Text: text}; m.Chat.ID = 42; return m }

	// 1) In the history, file on disk → shown, not queued again.
	f := filepath.Join(s.cfg.Library, "Youtube", "Kanal", "2021-03-20 - Kuş [dQw4w9WgXcQ].mp4")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("video"), 0o644)
	s.queue.jobs["old"] = &Job{ID: "old", URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", Title: "Kuş",
		Status: StDone, Files: []string{f}, Finished: time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local), Created: time.Now().Add(-time.Hour)}
	s.tg.handle(ctx, tgTestToken, msg(1, "https://youtu.be/dQw4w9WgXcQ?si=xyz"))
	log := strings.Join(fake.log(), "\n")
	if !strings.Contains(log, "42|1|✓ Zaten indirilmiş: Kuş · 5 B · 20.10.2026") || !strings.Contains(log, "/yeniden https://youtu.be/dQw4w9WgXcQ?si=xyz") ||
		!strings.Contains(log, "42|1|sendVideo:video:2021-03-20 - Kuş [dQw4w9WgXcQ].mp4:5") {
		t.Fatalf("not shown as already downloaded:\n%s", log)
	}
	if len(s.queue.jobs) != 1 {
		t.Fatal("queued again")
	}

	// 2) /yeniden forces a new download.
	fake.sent = nil
	s.tg.handle(ctx, tgTestToken, msg(2, "/yeniden https://youtu.be/dQw4w9WgXcQ"))
	if len(s.queue.jobs) != 2 || !strings.Contains(strings.Join(fake.log(), "\n"), "⏳ Kuyruğa alındı") {
		t.Fatalf("force did not queue: %v", fake.log())
	}

	// 3) That new job is queued now → a third send says "already in the queue".
	fake.sent = nil
	s.tg.handle(ctx, tgTestToken, msg(3, "https://www.youtube.com/watch?v=dQw4w9WgXcQ"))
	if l := fake.log(); len(l) != 1 || !strings.Contains(l[0], "⏳ Bu zaten kuyrukta") || len(s.queue.jobs) != 2 {
		t.Fatalf("active duplicate: %v", l)
	}

	// 4) Record cleared but the file is in the archive: found by "[ID]" in the name.
	s.queue.jobs = map[string]*Job{}
	fake.sent = nil
	s.tg.handle(ctx, tgTestToken, msg(4, "https://www.youtube.com/shorts/dQw4w9WgXcQ"))
	if l := strings.Join(fake.log(), "\n"); !strings.Contains(l, "✓ Zaten indirilmiş: 2021-03-20 - Kuş [dQw4w9WgXcQ]") || len(s.queue.jobs) != 0 {
		t.Fatalf("archive match: %s", l)
	}

	// 5) File deleted from disk → downloaded again.
	os.Remove(f)
	s.lib.Invalidate()
	fake.sent = nil
	s.tg.handle(ctx, tgTestToken, msg(5, "https://youtu.be/dQw4w9WgXcQ"))
	if len(s.queue.jobs) != 1 {
		t.Fatalf("deleted file still counted as downloaded: %v", fake.log())
	}

	// 6) Mixed message: one known, one new → one shown, one queued.
	os.WriteFile(f, []byte("video"), 0o644)
	s.lib.Invalidate()
	s.queue.jobs = map[string]*Job{}
	fake.sent = nil
	s.tg.handle(ctx, tgTestToken, msg(6, "https://youtu.be/dQw4w9WgXcQ https://youtu.be/aaaaaaaaaaa"))
	l := strings.Join(fake.log(), "\n")
	if len(s.queue.jobs) != 1 || !strings.Contains(l, "Zaten indirilmiş") || !strings.Contains(l, "⏳ Kuyruğa alındı: youtu.be") {
		t.Fatalf("mixed: %s", l)
	}
}

func TestAddSkipsExisting(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	f := filepath.Join(s.cfg.Library, "Youtube", "Kanal", "Kuş [dQw4w9WgXcQ].mp4")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("video"), 0o644)
	s.queue.jobs["old"] = &Job{ID: "old", URL: "https://youtu.be/dQw4w9WgXcQ", Title: "Kuş", Status: StDone,
		Files: []string{"/gone.mp4", f}, Finished: time.Now().Add(-time.Hour), Created: time.Now().Add(-2 * time.Hour)}
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Plain add: not queued, the message says so and points at the file.
	r := post("/api/add", "bak https://www.youtube.com/watch?v=dQw4w9WgXcQ&si=x")
	var res addResponse
	json.Unmarshal(r.Body.Bytes(), &res)
	if r.Code != 200 || res.Added != 0 || res.Message != "Zaten indirilmiş: Kuş" || len(res.Existing) != 1 ||
		res.Existing[0].Files[0].URL != "/api/jobs/old/file/1" || res.Existing[0].Size != 5 || len(s.queue.jobs) != 1 {
		t.Fatalf("add existing: %d %s", r.Code, r.Body)
	}
	// Mixed: one new, one known.
	r = post("/api/add", "https://youtu.be/dQw4w9WgXcQ https://youtu.be/aaaaaaaaaaa")
	json.Unmarshal(r.Body.Bytes(), &res)
	if res.Added != 1 || !strings.Contains(res.Message, "Kuyruğa alındı") || !strings.Contains(res.Message, "zaten indirilmiş") {
		t.Fatalf("mixed: %s", r.Body)
	}
	// force=1 downloads again.
	n := len(s.queue.jobs)
	if r = post("/api/add?force=1", "https://youtu.be/dQw4w9WgXcQ"); r.Code != 200 || len(s.queue.jobs) != n+1 {
		t.Fatalf("force: %d %s", r.Code, r.Body)
	}

	// Wait mode ("Telefona indir"): the file comes back at once, nothing queued.
	s.queue.jobs = map[string]*Job{"old": s.queue.jobs["old"]}
	r = post("/api/add?wait=1", "https://youtu.be/dQw4w9WgXcQ")
	if r.Code != 200 || r.Body.String() != "video" || r.Header().Get("X-Indirgen-Existing") != "1" || len(s.queue.jobs) != 1 {
		t.Fatalf("wait existing: %d %q %v", r.Code, r.Body, r.Header())
	}

	// Archive-only match (history cleared): link points into the library.
	s.queue.jobs = map[string]*Job{}
	ex, ok := FindExisting(s.queue, s.lib, "https://youtu.be/dQw4w9WgXcQ")
	if v := ex.View(s.lib); !ok || len(v.Files) != 1 || v.Files[0].URL != "/api/library/file?path=Youtube%2FKanal%2FKu%C5%9F+%5BdQw4w9WgXcQ%5D.mp4" || v.Files[0].Kind != "video" {
		t.Fatalf("archive view: %+v", ex.View(s.lib))
	}
}

// X: yt-dlp's id is the media ID, so the tweet ID now goes into the file name
// too. With the history cleared the archive must still know the tweet.
func TestXArchiveMatchByTweetID(t *testing.T) {
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	f := filepath.Join(s.cfg.Library, "Twitter", "Captain America",
		"2016-04-12 - Captain America - Are you sure [719944021058060289] [717462543795523584].mp4")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("video"), 0o644)
	for _, l := range []string{
		"https://x.com/captainamerica/status/719944021058060289",
		"https://twitter.com/captainamerica/status/719944021058060289?s=20",
		"https://mobile.twitter.com/captainamerica/status/719944021058060289/video/1",
	} {
		if ex, ok := FindExisting(s.queue, s.lib, l); !ok || len(ex.Files) != 1 {
			t.Errorf("%s: not found in the archive", l)
		}
	}
	// The media ID alone is not a tweet: no false match on another tweet.
	if _, ok := FindExisting(s.queue, s.lib, "https://x.com/a/status/717462543795523585"); ok {
		t.Error("matched an unrelated tweet")
	}
}

func TestTikTokLinkForms(t *testing.T) {
	for _, l := range []string{
		"https://m.tiktok.com/v/7047596209028074758.html",
		"https://www.tiktok.com/v/7047596209028074758.html?lang=tr",
		"https://www.tiktok.com/@hankgreen1/video/7047596209028074758",
	} {
		if k, _, ok := mediaKey(l); !ok || k != "tiktok:7047596209028074758" {
			t.Errorf("%s → %q (%v)", l, k, ok)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubShortLinks sends shortLinkClient's requests to h and records each one
// as "host+path UA".
func stubShortLinks(t *testing.T, h http.HandlerFunc) func() []string {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	to, _ := url.Parse(srv.URL)
	var mu sync.Mutex
	var seen []string
	old := shortLinkClient.Transport
	shortLinkClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.URL.Host+r.URL.Path+" "+r.UserAgent())
		mu.Unlock()
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host, r2.Host = to.Scheme, to.Host, r.URL.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
	t.Cleanup(func() { shortLinkClient.Transport = old })
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func TestResolveShortLink(t *testing.T) {
	const video = "https://www.tiktok.com/@hankgreen1/video/7047596209028074758?_r=1&_t=ZN-123"
	seen := stubShortLinks(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Host + r.URL.Path {
		case "vm.tiktok.com/ZMabc/":
			http.Redirect(w, r, "https://vt.tiktok.com/ZSdef/", http.StatusMovedPermanently)
		case "vt.tiktok.com/ZSdef/":
			http.Redirect(w, r, video, http.StatusFound)
		case "www.tiktok.com/t/ZTghi/":
			http.Redirect(w, r, video, http.StatusMovedPermanently)
		case "vm.tiktok.com/gone/":
			http.Redirect(w, r, "https://www.tiktok.com/", http.StatusFound) // deleted video: home page
		case "vm.tiktok.com/loop/":
			http.Redirect(w, r, "https://vm.tiktok.com/loop/", http.StatusFound)
		case "vm.tiktok.com/slow/":
			time.Sleep(time.Second)
		default:
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()

	// Two hops, ending on the video: the full link comes back.
	if got := resolveShortLink(ctx, "https://vm.tiktok.com/ZMabc/"); got != video {
		t.Fatalf("vm → vt → video: %s", got)
	}
	if got := resolveShortLink(ctx, "https://www.tiktok.com/t/ZTghi/"); got != video {
		t.Fatalf("/t/ link: %s", got)
	}
	// The video page itself is never fetched, and a browser User-Agent is sent.
	for _, s := range seen() {
		if strings.HasPrefix(s, "www.tiktok.com/@") {
			t.Fatalf("followed into the video page: %v", seen())
		}
		if !strings.Contains(s, "Mozilla/5.0") {
			t.Fatalf("no browser User-Agent: %s", s)
		}
	}
	// Not a single video, a redirect loop, a 404, a timeout: the link stays as sent.
	shortLinkTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shortLinkTimeout = 6 * time.Second })
	for _, l := range []string{"https://vm.tiktok.com/gone/", "https://vm.tiktok.com/loop/",
		"https://vm.tiktok.com/missing/", "https://vm.tiktok.com/slow/"} {
		if got := resolveShortLink(ctx, l); got != l {
			t.Errorf("%s → %s, want unchanged", l, got)
		}
	}
	if n := len(seen()); n != 2+1+1+3+1+1 {
		t.Errorf("loop must stop after 3 requests; %d requests: %v", n, seen())
	}
	// Any other link: no request at all.
	before := len(seen())
	for _, l := range []string{"https://www.tiktok.com/@a/video/7047596209028074758", "https://youtu.be/dQw4w9WgXcQ",
		"https://example.com/t/abc", "https://vm.tiktok.com.evil.example/ZMabc/"} {
		if got := resolveShortLink(ctx, l); got != l {
			t.Errorf("%s changed to %s", l, got)
		}
	}
	if len(seen()) != before {
		t.Fatalf("contacted a host that is not a share-link host: %v", seen()[before:])
	}
}

// A share link to a video already in the archive is recognised in /api/add
// (and its wait mode); a new one is queued with the resolved link.
func TestAddResolvesShareLinks(t *testing.T) {
	stubShortLinks(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ZMold/":
			http.Redirect(w, r, "https://www.tiktok.com/@hankgreen1/video/7047596209028074758?_r=1", http.StatusMovedPermanently)
		case "/ZMnew/":
			http.Redirect(w, r, "https://www.tiktok.com/@hankgreen1/video/7000000000000000001?_r=1", http.StatusMovedPermanently)
		}
	})
	s, h := testServer(t, "never", "127.0.0.1:8899")
	f := filepath.Join(s.cfg.Library, "TikTok", "hankgreen1", "2021-12-30 - TikTok video [7047596209028074758].mp4")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("video"), 0o644)
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "text/plain")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	var res addResponse
	r := post("/api/add", "https://vm.tiktok.com/ZMold/")
	json.Unmarshal(r.Body.Bytes(), &res)
	if r.Code != 200 || res.Added != 0 || !strings.HasPrefix(res.Message, "Zaten indirilmiş") {
		t.Fatalf("share link to a downloaded video: %d %s", r.Code, r.Body)
	}
	if r = post("/api/add?wait=1", "https://vm.tiktok.com/ZMold/"); r.Body.String() != "video" || r.Header().Get("X-Indirgen-Existing") != "1" {
		t.Fatalf("wait mode: %d %q", r.Code, r.Body)
	}
	r = post("/api/add", "https://vm.tiktok.com/ZMnew/")
	json.Unmarshal(r.Body.Bytes(), &res)
	if res.Added != 1 || res.Jobs[0].URL != "https://www.tiktok.com/@hankgreen1/video/7000000000000000001?_r=1" {
		t.Fatalf("new video not queued with its full link: %s", r.Body)
	}
}

func TestTelegramResolvesShareLinks(t *testing.T) {
	stubShortLinks(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://www.tiktok.com/@hankgreen1/video/7047596209028074758", http.StatusMovedPermanently)
	})
	fake := newFakeTG(t)
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	s.cfg.Telegram = TelegramConfig{Token: tgTestToken, Chats: []int64{42}}
	s.tg = NewTelegram(s.cfg, s.queue, s.lib, fake.srv.URL)
	m := &tgMessage{ID: 1, Text: "bak https://vm.tiktok.com/ZMabc/"}
	m.Chat.ID = 42

	s.tg.handle(context.Background(), tgTestToken, m)
	if l := s.queue.List(); len(l) != 1 || l[0].URL != "https://www.tiktok.com/@hankgreen1/video/7047596209028074758" {
		t.Fatalf("queued: %+v", l)
	}
	// The same share link again: already in the queue, not a second job.
	fake.sent = nil
	s.tg.handle(context.Background(), tgTestToken, m)
	if l := fake.log(); len(s.queue.List()) != 1 || len(l) != 1 || !strings.Contains(l[0], "zaten kuyrukta") {
		t.Fatalf("second send: %v", l)
	}
}

// The web UI's probe resolves share links too: the engine sees the video's
// own link, the card is queued with it, and "already downloaded" shows.
func TestProbeResolvesShareLinks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses shell scripts as fake engines")
	}
	const full = "https://www.tiktok.com/@hankgreen1/video/7047596209028074758?_r=1"
	stubShortLinks(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, full, http.StatusMovedPermanently) })
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(`#!/bin/sh
[ "$1" = "--version" ] && { echo 2026.10.26; exit 0; }
for a; do last="$a"; done
printf '{"id":"7047596209028074758","title":"%s","extractor_key":"TikTok"}' "$last"
`), 0o755)
	os.WriteFile(filepath.Join(bin, "gallery-dl"), []byte("#!/bin/sh\n[ \"$1\" = \"--version\" ] && { echo 1.32.18; exit 0; }\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	s, h := testServer(t, "never", "127.0.0.1:8899")
	s.eng = NewEngineManager(s.cfg)
	s.router = &Router{eng: s.eng, cookies: s.cookies}
	f := filepath.Join(s.cfg.Library, "TikTok", "hankgreen1", "2021-12-30 - TikTok video [7047596209028074758].mp4")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("video"), 0o644)

	req := httptest.NewRequest("POST", "/api/probe", strings.NewReader(`{"url":"https://vm.tiktok.com/ZMabc/"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res ProbeResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res.URL != full || res.Title != full || res.Existing == nil || len(res.Existing.Files) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
