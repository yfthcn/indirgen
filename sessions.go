package main

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Signed-in browsers ("connected devices").
//
// The login page and phone pairing give a browser a random 128-bit session
// ID in the indirgen_session cookie. sessions.json (0600) keeps only its
// SHA-256, with what the device list shows: created, last seen, address, a
// short device name from the User-Agent and how it signed in. Each session
// can be ended on its own; a new token ends them all. Last seen reaches the
// disk at most once a minute; a session unused for 90 days is removed.
// ---------------------------------------------------------------------------

const sessionTTL = 90 * 24 * time.Hour

type Session struct {
	ID       string    `json:"id"` // SHA-256 of the cookie value, hex
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	IP       string    `json:"ip"`
	Name     string    `json:"name"` // "iPhone · Safari"; "" = unknown
	Via      string    `json:"via"`  // qr | token

	done chan struct{} // closed when the session ends: its event stream stops
}

type SessionStore struct {
	mu       sync.Mutex
	path     string
	list     map[string]*Session
	now      func() time.Time // tests set the clock
	saves    int              // writes to disk, for tests
	onChange func()           // the device list changed
}

func NewSessionStore(path string) *SessionStore {
	st := &SessionStore{path: path, list: map[string]*Session{}}
	if raw, err := os.ReadFile(path); err == nil {
		var l []*Session
		if err := json.Unmarshal(raw, &l); err != nil {
			slog.Warn("sessions.json okunamadı", "hata", err)
		}
		for _, x := range l {
			if x.ID != "" {
				x.done = make(chan struct{})
				st.list[x.ID] = x
			}
		}
	}
	st.prune()
	return st
}

func (st *SessionStore) clock() time.Time {
	if st.now != nil {
		return st.now()
	}
	return time.Now()
}

func sessionID(cookie string) string {
	h := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(h[:])
}

// Create starts a session for the browser of r and returns its cookie value.
func (st *SessionStore) Create(r *http.Request, via string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	val := hex.EncodeToString(b)
	now := st.clock()
	x := &Session{ID: sessionID(val), Created: now, LastSeen: now, IP: clientAddr(r),
		Name: deviceName(r.UserAgent()), Via: via, done: make(chan struct{})}
	st.mu.Lock()
	st.list[x.ID] = x
	st.saveLocked()
	st.mu.Unlock()
	st.changed()
	return val
}

// Lookup finds the session of a cookie value and notes that it was seen.
func (st *SessionStore) Lookup(cookie string, r *http.Request) (Session, bool) {
	if cookie == "" {
		return Session{}, false
	}
	id := sessionID(cookie)
	st.mu.Lock()
	x, ok := st.list[id]
	if !ok {
		st.mu.Unlock()
		return Session{}, false
	}
	now := st.clock()
	if now.Sub(x.LastSeen) > sessionTTL {
		st.removeLocked(id)
		st.saveLocked()
		st.mu.Unlock()
		st.changed()
		return Session{}, false
	}
	if now.Sub(x.LastSeen) >= time.Minute {
		x.LastSeen, x.IP = now, clientAddr(r)
		st.saveLocked()
	}
	s := *x
	st.mu.Unlock()
	return s, true
}

// List returns the sessions, most recently seen first.
func (st *SessionStore) List() []Session {
	st.mu.Lock()
	out := make([]Session, 0, len(st.list))
	for _, x := range st.list {
		out = append(out, *x)
	}
	st.mu.Unlock()
	slices.SortFunc(out, func(a, b Session) int { return cmp.Or(b.LastSeen.Compare(a.LastSeen), strings.Compare(a.ID, b.ID)) })
	return out
}

// Delete ends one session.
func (st *SessionStore) Delete(id string) bool {
	return st.deleteWhere(func(x *Session) bool { return x.ID == id }) > 0
}

// DeleteOthers ends every session but keep ("" ends them all).
func (st *SessionStore) DeleteOthers(keep string) int {
	return st.deleteWhere(func(x *Session) bool { return x.ID != keep })
}

func (st *SessionStore) DeleteAll() { st.deleteWhere(func(*Session) bool { return true }) }

// prune removes the sessions unused for 90 days.
func (st *SessionStore) prune() {
	now := st.clock()
	st.deleteWhere(func(x *Session) bool { return now.Sub(x.LastSeen) > sessionTTL })
}

func (st *SessionStore) deleteWhere(match func(*Session) bool) int {
	st.mu.Lock()
	n := 0
	for id, x := range st.list {
		if match(x) {
			st.removeLocked(id)
			n++
		}
	}
	if n > 0 {
		st.saveLocked()
	}
	st.mu.Unlock()
	if n > 0 {
		st.changed()
	}
	return n
}

func (st *SessionStore) removeLocked(id string) {
	if x, ok := st.list[id]; ok {
		close(x.done)
		delete(st.list, id)
	}
}

func (st *SessionStore) saveLocked() {
	l := make([]*Session, 0, len(st.list))
	for _, x := range st.list {
		l = append(l, x)
	}
	slices.SortFunc(l, func(a, b *Session) int { return a.Created.Compare(b.Created) })
	raw, _ := json.MarshalIndent(l, "", "  ")
	if err := writeFileAtomic(st.path, raw, 0o600); err != nil {
		slog.Warn("sessions.json yazılamadı", "hata", err)
	}
	st.saves++
}

func (st *SessionStore) changed() {
	if st.onChange != nil {
		st.onChange()
	}
}

// deviceName makes "iPhone · Safari" of a User-Agent ("" when it names
// neither a known system nor a known browser).
func deviceName(ua string) string {
	has := func(s ...string) bool {
		return slices.ContainsFunc(s, func(x string) bool { return strings.Contains(ua, x) })
	}
	var system, browser string
	switch {
	case has("iPhone"):
		system = "iPhone"
	case has("iPad"):
		system = "iPad"
	case has("Android"):
		system = "Android"
	case has("Windows"):
		system = "Windows"
	case has("Macintosh", "Mac OS X"):
		system = "macOS"
	case has("Linux", "X11"):
		system = "Linux"
	}
	switch { // the order matters: Edge and Chrome also say "Safari"
	case has("Edg/", "EdgiOS/", "EdgA/"):
		browser = "Edge"
	case has("Firefox/", "FxiOS/"):
		browser = "Firefox"
	case has("Chrome/", "CriOS/"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	}
	if system != "" && browser != "" {
		return system + " · " + browser
	}
	return system + browser
}

// needBrowser answers 403 unless the request comes from this computer or a
// signed-in browser; what names the thing managed.
func (s *Server) needBrowser(w http.ResponseWriter, r *http.Request, what string) bool {
	if s.browserOrLocal(r) {
		return true
	}
	writeErr(w, 403, what+" yalnızca bu bilgisayardan ya da oturum açmış bir tarayıcıdan yönetilir")
	return false
}

// handleSessions (GET /api/sessions): the connected devices, with the one
// asking marked.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !s.needBrowser(w, r, "bağlı cihazlar") {
		return
	}
	cur, _ := s.currentSession(r)
	writeJSON(w, 200, map[string]any{"current": cur.ID, "sessions": s.sessions.List()})
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if !s.needBrowser(w, r, "bağlı cihazlar") {
		return
	}
	if !s.sessions.Delete(r.PathValue("id")) {
		writeErr(w, 404, "oturum bulunamadı")
		return
	}
	writeOK(w, 200)
}

func (s *Server) handleSessionsRevokeOthers(w http.ResponseWriter, r *http.Request) {
	if !s.needBrowser(w, r, "bağlı cihazlar") {
		return
	}
	cur, _ := s.currentSession(r)
	writeJSON(w, 200, map[string]int{"removed": s.sessions.DeleteOthers(cur.ID)})
}

// handleTokenRotate (POST /api/token/rotate): a new token, every session
// ended. Only this computer gets the new token in the answer.
func (s *Server) handleTokenRotate(w http.ResponseWriter, r *http.Request) {
	if !s.needBrowser(w, r, "token") {
		return
	}
	tok, err := s.cfg.RotateToken()
	if err != nil {
		writeErr(w, 500, "yeni token kaydedilemedi: "+err.Error())
		return
	}
	s.sessions.DeleteAll()
	slog.Info("token yenilendi, bütün oturumlar kapatıldı")
	if isLocal(r) {
		writeJSON(w, 200, map[string]string{"token": tok})
		return
	}
	writeOK(w, 200)
}
