package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTG is a minimal Bot API: getMe, getUpdates (a queue), sendMessage and
// the upload methods, recording what İndirgen sends.
type fakeTG struct {
	mu      sync.Mutex
	srv     *httptest.Server
	updates []tgUpdate
	sent    []string // "chat|reply|text" or "chat|reply|method:filename:size"
}

const tgTestToken = "123456789:AAH-abcdefghijklmnopqrstuvwxyz012345"

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/bot"+tgTestToken+"/")
		if !ok {
			w.WriteHeader(401)
			w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		ok200 := func(v any) { b, _ := json.Marshal(map[string]any{"ok": true, "result": v}); w.Write(b) }
		switch method {
		case "getMe":
			ok200(map[string]any{"id": 1, "username": "indirgen_test_bot"})
		case "getUpdates":
			var p struct{ Offset int64 }
			json.NewDecoder(r.Body).Decode(&p)
			var out []tgUpdate
			for _, u := range f.updates {
				if u.ID >= p.Offset {
					out = append(out, u)
				}
			}
			if len(out) == 0 {
				f.mu.Unlock()
				time.Sleep(50 * time.Millisecond) // short "long poll"
				f.mu.Lock()
			}
			ok200(out)
		case "sendMessage":
			var p struct {
				ChatID int64 `json:"chat_id"`
				Text   string
				Reply  struct {
					MessageID int `json:"message_id"`
				} `json:"reply_parameters"`
			}
			json.NewDecoder(r.Body).Decode(&p)
			f.sent = append(f.sent, strings.Join([]string{itoa(p.ChatID), itoa(int64(p.Reply.MessageID)), p.Text}, "|"))
			ok200(map[string]any{"message_id": 99})
		default: // sendVideo / sendDocument / sendPhoto / sendAudio
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				w.WriteHeader(400)
				return
			}
			var rp struct {
				MessageID int `json:"message_id"`
			}
			json.Unmarshal([]byte(r.FormValue("reply_parameters")), &rp)
			for field, fhs := range r.MultipartForm.File {
				fh := fhs[0]
				rc, _ := fh.Open()
				n, _ := io.Copy(io.Discard, rc)
				rc.Close()
				f.sent = append(f.sent, strings.Join([]string{r.FormValue("chat_id"), itoa(int64(rp.MessageID)),
					method + ":" + field + ":" + fh.Filename + ":" + itoa(n)}, "|"))
			}
			ok200(map[string]any{"message_id": 100})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (f *fakeTG) push(id int64, chat int64, msgID int, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := &tgMessage{ID: msgID, Text: text}
	m.Chat.ID = chat
	f.updates = append(f.updates, tgUpdate{ID: id, Message: m})
}

func (f *fakeTG) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 100 {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTelegramPairingAndLinks(t *testing.T) {
	fake := newFakeTG(t)
	s, h := testServer(t, "never", "127.0.0.1:8899")
	s.tg = NewTelegram(s.cfg, s.queue, s.lib, fake.srv.URL)
	ctx := t.Context()
	s.tg.Start(ctx)

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if r := post("/api/telegram", `{"token":"123:short"}`); r.Code != 400 {
		t.Fatalf("malformed token accepted: %d", r.Code)
	}
	if r := post("/api/telegram", `{"token":"987654321:AAH-wrongwrongwrongwrongwrongwrong12"}`); r.Code != 400 || !strings.Contains(r.Body.String(), "geçersiz") {
		t.Fatalf("invalid token accepted: %d %s", r.Code, r.Body)
	}
	r := post("/api/telegram", `{"token":"`+tgTestToken+`"}`)
	var st TGStatus
	json.Unmarshal(r.Body.Bytes(), &st)
	if r.Code != 200 || !st.HasToken || st.PairCode == "" || st.PairLink != "https://t.me/indirgen_test_bot?start="+st.PairCode {
		t.Fatalf("setup: %d %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), tgTestToken) {
		t.Fatal("token leaked to the UI")
	}

	// A stranger is ignored; the right code pairs the chat.
	fake.push(1, 555, 1, "https://youtu.be/stranger")
	fake.push(2, 777, 2, "/start 000000")
	fake.push(3, 42, 3, "/start "+st.PairCode)
	waitFor(t, "pairing", func() bool { return len(s.cfg.TelegramConf().Chats) == 1 })
	if c := s.cfg.TelegramConf().Chats; c[0] != 42 {
		t.Fatalf("paired %v", c)
	}
	// Links from the paired chat are queued and acknowledged as a reply.
	fake.push(4, 42, 10, "Şuna bak https://www.youtube.com/watch?v=abc123 ve https://youtu.be/def456")
	waitFor(t, "queued reply", func() bool {
		for _, l := range fake.log() {
			if strings.HasPrefix(l, "42|10|⏳ 2 bağlantı") {
				return true
			}
		}
		return false
	})
	jobs := s.queue.List()
	if len(jobs) != 2 || jobs[0].Source != "telegram" || jobs[0].Reply == nil || jobs[0].Reply.Msg != 10 {
		t.Fatalf("jobs %+v", jobs)
	}
	for _, l := range fake.log() {
		if strings.HasPrefix(l, "555|") || strings.HasPrefix(l, "777|") {
			t.Fatalf("answered a stranger: %s", l)
		}
	}
	if s.tg.Status().PairCode != "" {
		t.Fatal("pairing code reusable")
	}
	// The offset is stored: a restart does not handle the same messages again.
	var raw []byte
	for range 100 { // written after the batch is handled
		if raw, _ = os.ReadFile(filepath.Join(s.cfg.dataDir, "telegram.json")); strings.Contains(string(raw), `"offset":5`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(raw), `"offset":5`) {
		t.Fatalf("offset not saved: %s", raw)
	}
}

func TestTelegramResults(t *testing.T) {
	fake := newFakeTG(t)
	s, _ := testServer(t, "never", "127.0.0.1:8899")
	s.cfg.Telegram = TelegramConfig{Token: tgTestToken, Chats: []int64{42, 43}}
	s.tg = NewTelegram(s.cfg, s.queue, s.lib, fake.srv.URL)

	dir := t.TempDir()
	small := filepath.Join(dir, "klip.mp4")
	os.WriteFile(small, []byte("small video"), 0o644)
	big := filepath.Join(dir, "film.mkv")
	fh, _ := os.Create(big)
	fh.Truncate(tgMaxUpload + 1) // sparse: no real 50 MB written
	fh.Close()

	// From Telegram: reply in that chat only, with the file that fits.
	s.tg.jobFinished(Job{Title: "Klip", Status: StDone, Source: "telegram", Reply: &ChatRef{Chat: 42, Msg: 7}, Files: []string{small, big}})
	log := strings.Join(fake.log(), "\n")
	for _, want := range []string{"42|7|✓ Klip · 2 dosya", "42|7|sendVideo:video:klip.mp4:11", "42|7|ℹ 1 dosya 50 MB'tan büyük"} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in\n%s", want, log)
		}
	}
	if strings.Contains(log, "43|") || strings.Contains(log, "film.mkv") {
		t.Fatalf("sent to the wrong chat or a file over 50 MB:\n%s", log)
	}

	// From the Shortcut: every chat, message only. Errors are reported.
	fake.sent = nil
	s.tg.jobFinished(Job{Title: "Video", Status: StError, Error: "ERROR: Private video", Source: "api"})
	if l := fake.log(); len(l) != 2 || !strings.Contains(l[0], "✕ Video\nERROR: Private video") {
		t.Fatalf("error report: %v", l)
	}
	// Quiet cases: web UI jobs, subscriptions with nothing new, canceled, Quiet on.
	fake.sent = nil
	s.tg.jobFinished(Job{Title: "UI", Status: StDone, Files: []string{small}})
	s.tg.jobFinished(Job{Title: "Abone", Status: StDone, Source: "sub"})
	s.tg.jobFinished(Job{Title: "İptal", Status: StCanceled, Source: "telegram"})
	s.cfg.Telegram.Quiet = true
	s.tg.jobFinished(Job{Title: "Sessiz", Status: StDone, Source: "api", Files: []string{small}})
	if l := fake.log(); len(l) != 0 {
		t.Fatalf("should stay quiet: %v", l)
	}
	s.cfg.Telegram.Quiet = false
	s.tg.jobFinished(Job{Title: "Kanal", Status: StDone, Source: "sub", Files: []string{small}})
	if l := fake.log(); len(l) != 2 || !strings.HasPrefix(strings.SplitN(l[0], "|", 3)[2], "✓ 🔔 Kanal") {
		t.Fatalf("subscription report: %v", l)
	}
}

func TestAddWaitReturnsFile(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	file := filepath.Join(s.cfg.Dir("temp"), "x", "video.mp4")
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.WriteFile(file, []byte("MP4DATA"), 0o644)
	// Stand-in for the engine: finish whatever job shows up.
	go func() {
		for range 200 {
			time.Sleep(20 * time.Millisecond)
			s.queue.mu.Lock()
			for _, j := range s.queue.jobs {
				if j.Status == StQueued {
					if j.Opts.Preset != "compat" || j.Opts.Mode != "temp" || j.Source != "api" {
						j.Status, j.Error = StError, "wrong defaults"
					} else {
						j.Status, j.Files = StDone, []string{file}
					}
				}
			}
			s.queue.mu.Unlock()
		}
	}()
	req := httptest.NewRequest("POST", "/api/add?wait=1", strings.NewReader(`{"text":"https://youtu.be/w1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "MP4DATA" || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("%d %q %v", rec.Code, rec.Body, rec.Header())
	}
}
