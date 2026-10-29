package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// login signs a browser at remote in with the token, as the login page does.
func login(t *testing.T, s *Server, h http.Handler, remote, ua string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"token":"`+s.cfg.Token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	c := sessionOf(rec)
	if rec.Code != 200 || c == nil {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	return c
}

// call sends method path from remote with an optional cookie, Bearer token
// and Origin (POSTs from a page carry their own).
func call(h http.Handler, method, path, remote string, c *http.Cookie, bearer, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	if c != nil {
		req.AddCookie(c)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type sessionList struct {
	Current  string    `json:"current"`
	Sessions []Session `json:"sessions"`
}

func listSessions(t *testing.T, h http.Handler, remote string, c *http.Cookie) sessionList {
	t.Helper()
	r := call(h, "GET", "/api/sessions", remote, c, "", "")
	if r.Code != 200 {
		t.Fatalf("list: %d %s", r.Code, r.Body)
	}
	var l sessionList
	json.Unmarshal(r.Body.Bytes(), &l)
	return l
}

const (
	uaIPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1"
	uaLinux  = "Mozilla/5.0 (X11; Linux x86_64; rv:144.0) Gecko/20100101 Firefox/144.0"
)

// The cookie holds a random ID; the file only its SHA-256, mode 0600, and
// a new store reads it back.
func TestSessionCreateStoresOnlyHash(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	c := login(t, s, h, "192.168.1.50:1", uaIPhone)
	if raw, _ := hex.DecodeString(c.Value); len(raw) < 16 {
		t.Fatalf("cookie %q: want 128 random bits or more, hex", c.Value)
	}
	path := s.cfg.Dir("sessions.json")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("sessions.json: %v %v", st, err)
	}
	raw, _ := os.ReadFile(path)
	sum := sha256.Sum256([]byte(c.Value))
	if strings.Contains(string(raw), c.Value) || !strings.Contains(string(raw), hex.EncodeToString(sum[:])) {
		t.Fatalf("file holds the cookie or lacks its hash:\n%s", raw)
	}
	l := listSessions(t, h, "192.168.1.50:1", c)
	if len(l.Sessions) != 1 || l.Current != l.Sessions[0].ID {
		t.Fatalf("%+v", l)
	}
	x := l.Sessions[0]
	if x.Name != "iPhone · Safari" || x.Via != "token" || x.IP != "192.168.1.50" || x.Created.IsZero() || x.LastSeen.IsZero() {
		t.Fatalf("%+v", x)
	}
	// A restart: the same cookie still works.
	s.sessions = NewSessionStore(path)
	if r := call(h, "GET", "/api/jobs", "192.168.1.50:1", c, "", ""); r.Code != 200 {
		t.Fatalf("after reload: %d", r.Code)
	}
}

func TestSessionRevokeOneAndOthers(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	phone := login(t, s, h, "192.168.1.50:1", uaIPhone)
	laptop := login(t, s, h, "192.168.1.60:1", uaLinux)
	third := login(t, s, h, "192.168.1.70:1", uaLinux)
	if phone.Value == laptop.Value || laptop.Value == third.Value {
		t.Fatal("two devices got the same cookie")
	}
	l := listSessions(t, h, "192.168.1.60:1", laptop)
	if len(l.Sessions) != 3 {
		t.Fatalf("%+v", l)
	}
	var phoneID string
	for _, x := range l.Sessions {
		if x.IP == "192.168.1.50" {
			phoneID = x.ID
		}
	}
	// The laptop signs the phone out.
	if r := call(h, "DELETE", "/api/sessions/"+phoneID, "192.168.1.60:1", laptop, "", ""); r.Code != 200 {
		t.Fatalf("revoke: %d %s", r.Code, r.Body)
	}
	if r := call(h, "GET", "/api/jobs", "192.168.1.50:1", phone, "", ""); r.Code != 401 {
		t.Fatalf("revoked phone: %d", r.Code)
	}
	if r := call(h, "DELETE", "/api/sessions/"+phoneID, "192.168.1.60:1", laptop, "", ""); r.Code != 404 {
		t.Fatalf("revoke twice: %d", r.Code)
	}
	// "All other devices": the laptop stays, the third goes.
	r := call(h, "POST", "/api/sessions/revoke-others", "192.168.1.60:1", laptop, "", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"removed":1`) {
		t.Fatalf("revoke-others: %d %s", r.Code, r.Body)
	}
	if r := call(h, "GET", "/api/jobs", "192.168.1.70:1", third, "", ""); r.Code != 401 {
		t.Fatalf("third: %d", r.Code)
	}
	if l := listSessions(t, h, "192.168.1.60:1", laptop); len(l.Sessions) != 1 || l.Sessions[0].ID != l.Current {
		t.Fatalf("left: %+v", l)
	}
	// Logging out ends the session on the server too.
	if r := call(h, "POST", "/api/logout", "192.168.1.60:1", laptop, "", ""); r.Code != 200 {
		t.Fatalf("logout: %d", r.Code)
	}
	if r := call(h, "GET", "/api/jobs", "192.168.1.60:1", laptop, "", ""); r.Code != 401 {
		t.Fatalf("after logout: %d", r.Code)
	}
}

// A new token ends every session; only this computer sees it in the reply.
func TestTokenRotateEndsAllSessions(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	phone := login(t, s, h, "192.168.1.50:1", uaIPhone)
	laptop := login(t, s, h, "192.168.1.60:1", uaLinux)
	oldTok := s.cfg.Token

	r := call(h, "POST", "/api/token/rotate", "192.168.1.60:1", laptop, "", "")
	if r.Code != 200 || strings.Contains(r.Body.String(), `"token"`) {
		t.Fatalf("rotate from the network: %d %s", r.Code, r.Body)
	}
	for _, c := range []*http.Cookie{phone, laptop} {
		if r := call(h, "GET", "/api/jobs", "192.168.1.50:1", c, "", ""); r.Code != 401 {
			t.Fatalf("session after rotate: %d", r.Code)
		}
	}
	if r := call(h, "GET", "/api/jobs", "192.168.1.50:1", nil, oldTok, ""); r.Code != 401 {
		t.Fatalf("old token: %d", r.Code)
	}
	raw, _ := os.ReadFile(s.cfg.Dir("config.json"))
	if strings.Contains(string(raw), oldTok) {
		t.Fatal("old token still in config.json")
	}

	r = call(h, "POST", "/api/token/rotate", "127.0.0.1:1", nil, "", "")
	var res struct{ Token string }
	json.Unmarshal(r.Body.Bytes(), &res)
	if r.Code != 200 || !isLowerHex(res.Token) || len(res.Token) < 32 || res.Token == oldTok {
		t.Fatalf("rotate on this computer: %d %s", r.Code, r.Body)
	}
	if r := call(h, "GET", "/api/jobs", "192.168.1.50:1", nil, res.Token, ""); r.Code != 200 {
		t.Fatalf("new token: %d", r.Code)
	}
}

// The device list and its controls need a signed-in browser or this
// computer: the token alone (Shortcut, extension) is not enough, and a web
// page cannot use them (CSRF).
func TestSessionsAPINeedsBrowser(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	c := login(t, s, h, "192.168.1.50:1", uaIPhone)
	id := listSessions(t, h, "192.168.1.50:1", c).Current
	for _, x := range []struct{ method, path string }{
		{"GET", "/api/sessions"}, {"DELETE", "/api/sessions/" + id},
		{"POST", "/api/sessions/revoke-others"}, {"POST", "/api/token/rotate"},
	} {
		if r := call(h, x.method, x.path, "192.168.1.70:1", nil, s.cfg.Token, ""); r.Code != 403 {
			t.Errorf("bearer %s %s: %d", x.method, x.path, r.Code)
		}
		if r := call(h, x.method, x.path, "192.168.1.70:1", nil, "", ""); r.Code != 401 {
			t.Errorf("stranger %s %s: %d", x.method, x.path, r.Code)
		}
		if x.method != "GET" {
			if r := call(h, x.method, x.path, "192.168.1.50:1", c, "", "https://evil.example"); r.Code != 403 {
				t.Errorf("cross-site %s %s: %d", x.method, x.path, r.Code)
			}
		}
	}
	if r := call(h, "GET", "/api/sessions", "127.0.0.1:1", nil, "", ""); r.Code != 200 {
		t.Fatalf("this computer: %d", r.Code)
	}
}

// "Last seen" reaches the disk at most once a minute, whatever the traffic.
func TestLastSeenWrittenAtMostOncePerMinute(t *testing.T) {
	st := NewSessionStore(t.TempDir() + "/sessions.json")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	req := httptest.NewRequest("GET", "/", nil)
	val := st.Create(req, "qr")
	base := st.saves
	for range 50 { // a busy minute: many requests
		now = now.Add(time.Second)
		if _, ok := st.Lookup(val, req); !ok {
			t.Fatal("lookup")
		}
	}
	if st.saves-base != 0 {
		t.Fatalf("%d writes within the first minute", st.saves-base)
	}
	now = now.Add(15 * time.Second) // 65 s after creation
	st.Lookup(val, req)
	if st.saves-base != 1 {
		t.Fatalf("%d writes after a minute, want 1", st.saves-base)
	}
	if l := st.List(); !l[0].LastSeen.Equal(now) {
		t.Fatalf("last seen %v, want %v", l[0].LastSeen, now)
	}
}

func TestSessionsExpireAfter90DaysUnused(t *testing.T) {
	path := t.TempDir() + "/sessions.json"
	st := NewSessionStore(path)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	req := httptest.NewRequest("GET", "/", nil)
	idle, busy := st.Create(req, "token"), st.Create(req, "qr")
	now = now.Add(60 * 24 * time.Hour)
	st.Lookup(busy, req) // used on day 60
	now = now.Add(31 * 24 * time.Hour)
	if _, ok := st.Lookup(idle, req); ok {
		t.Fatal("unused for 91 days, still valid")
	}
	if _, ok := st.Lookup(busy, req); !ok {
		t.Fatal("used 31 days ago, gone")
	}
	// Removed from the file too, also when nobody asks for it: on load.
	st2 := NewSessionStore(path)
	st2.now = func() time.Time { return now.Add(90 * 24 * time.Hour) }
	st2.prune()
	if len(st2.List()) != 0 {
		t.Fatalf("left after 90 more days: %+v", st2.List())
	}
}

func TestDeviceName(t *testing.T) {
	for ua, want := range map[string]string{
		uaIPhone: "iPhone · Safari",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0.7390.96 Mobile/15E148 Safari/604.1": "iPhone · Chrome",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/144.0 Mobile/15E148 Safari/605.1.15":      "iPhone · Firefox",
		"Mozilla/5.0 (iPad; CPU OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1":                 "iPad · Safari",
		"Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.7390.122 Mobile Safari/537.36":                     "Android · Chrome",
		"Mozilla/5.0 (Android 15; Mobile; rv:144.0) Gecko/144.0 Firefox/144.0":                                                                           "Android · Firefox",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.3537.85":              "Windows · Edge",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36":                                "Windows · Chrome",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15":                          "macOS · Safari",
		uaLinux: "Linux · Firefox",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36": "Linux · Chrome",
		"curl/8.14.1": "",
		"":            "",
	} {
		if got := deviceName(ua); got != want {
			t.Errorf("%.60s…: %q, want %q", ua, got, want)
		}
	}
}

// A revoked browser's open event stream ends at once.
func TestRevokedSessionEndsEventStream(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	s.eng = NewEngineManager(s.cfg)
	c := login(t, s, h, "192.168.1.50:1", uaIPhone)
	id := listSessions(t, h, "192.168.1.50:1", c).Current
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr, r.Host = "192.168.1.50:1", "example.com"
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream: %v %v", err, resp)
	}
	defer resp.Body.Close()
	ended := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				close(ended)
				return
			}
		}
	}()
	if !s.sessions.Delete(id) {
		t.Fatal("delete")
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("stream still open after the session was revoked")
	}
}

// Sessions stay on this installation: a backup never carries them, and a
// backup that does is refused without writing anything.
func TestBackupLeavesSessionsOut(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	c := login(t, s, h, "192.168.1.50:1", uaIPhone)
	os.WriteFile(s.cfg.Dir("subscriptions.json"), []byte(`[{"id":"s1","url":"https://www.youtube.com/@k"}]`), 0o600)
	sessBefore, err := os.ReadFile(s.cfg.Dir("sessions.json"))
	if err != nil || len(sessBefore) == 0 {
		t.Fatalf("no sessions.json to leave out: %v", err)
	}
	subsBefore, _ := os.ReadFile(s.cfg.Dir("subscriptions.json"))

	r := call(h, "GET", "/api/backup", "127.0.0.1:1", nil, "", "")
	if r.Code != 200 {
		t.Fatalf("backup: %d", r.Code)
	}
	backup := r.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(backup), int64(len(backup)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "sessions.json" {
			t.Fatal("backup holds sessions.json")
		}
	}

	// The same backup with sessions.json (and a changed file) added.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		if f.Name == "subscriptions.json" {
			continue
		}
		rc, _ := f.Open()
		w, _ := zw.Create(f.Name)
		io.Copy(w, rc)
		rc.Close()
	}
	w, _ := zw.Create("subscriptions.json")
	w.Write([]byte(`[]`))
	w, _ = zw.Create("sessions.json")
	w.Write([]byte(`[{"id":"` + strings.Repeat("a", 64) + `","via":"token"}]`))
	zw.Close()

	req := httptest.NewRequest("POST", "/api/backup", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/zip")
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "sessions.json") {
		t.Fatalf("restore with sessions.json: %d %s", rec.Code, rec.Body)
	}
	if after, _ := os.ReadFile(s.cfg.Dir("sessions.json")); !bytes.Equal(after, sessBefore) {
		t.Fatal("sessions.json changed by a refused restore")
	}
	if after, _ := os.ReadFile(s.cfg.Dir("subscriptions.json")); !bytes.Equal(after, subsBefore) {
		t.Fatal("a refused restore wrote other files")
	}
	if r := call(h, "GET", "/api/jobs", "192.168.1.50:1", c, "", ""); r.Code != 200 {
		t.Fatalf("session lost: %d", r.Code)
	}
}
