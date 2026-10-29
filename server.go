package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// ---- SSE hub --------------------------------------------------------------

type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

func (h *Hub) Publish(typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	msg := []byte("event: " + typ + "\ndata: " + string(raw) + "\n\n")
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default: // slow client: drop it, the browser reconnects and resyncs
			delete(h.subs, ch)
			close(ch)
			slog.Warn("canlı bağlantı yetişemedi, kesildi (sayfa yeniden bağlanır)")
		}
	}
}

func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 1024)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// ---- server ---------------------------------------------------------------

type Server struct {
	cfg      *Config
	eng      *EngineManager
	router   *Router
	queue    *Queue
	cookies  *CookieStore
	hub      *Hub
	upd      *Updater
	lib      *Library
	subs     *Subs
	tg       *Telegram
	ctx      context.Context
	restart  func() // clean shutdown + re-exec after a self-update
	pair     pairing
	sessions *SessionStore
}

const sessionCookie = "indirgen_session"

func (s *Server) authed(r *http.Request) bool {
	if !s.cfg.AuthRequired() {
		return true
	}
	// This computer's own browser does not need the token, even when the
	// network is open for the phone ("always" turns this off).
	if s.cfg.Auth != "always" && isLocal(r) {
		return true
	}
	if s.hasSession(r) {
		return true
	}
	if a, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.tokenMatches(a) {
		return true
	}
	return false
}

// tokenMatches compares a sent token with ours in constant time. The
// generated token is lowercase hex, so for it case does not count: phones
// capitalise what is typed. A token set by hand in config.json is compared
// as it is.
func (s *Server) tokenMatches(given string) bool {
	want := s.cfg.AccessToken()
	if isLowerHex(want) {
		given = strings.ToLower(given)
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(want)) == 1
}

func isLowerHex(v string) bool {
	return v != "" && strings.Trim(v, "0123456789abcdef") == ""
}

// currentSession: the session of the request's login cookie, if it is one.
func (s *Server) currentSession(r *http.Request) (Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Session{}, false
	}
	return s.sessions.Lookup(c.Value, r)
}

// hasSession: the request comes from a signed-in browser.
func (s *Server) hasSession(r *http.Request) bool {
	_, ok := s.currentSession(r)
	return ok
}

// browserOrLocal: this computer, or a signed-in browser. Pairing and the
// device list need one of them; the token alone (Shortcut, extension) is
// not enough.
func (s *Server) browserOrLocal(r *http.Request) bool { return isLocal(r) || s.hasSession(r) }

// setSession signs this browser in for 90 days; via is "token" or "qr".
func (s *Server) setSession(w http.ResponseWriter, r *http.Request, via string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.sessions.Create(r, via), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 90 * 86400})
}

// sameOrigin blocks cross-site state changes (CSRF) in addition to the
// SameSite=Strict session cookie.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	// Browser extensions (İndirgen'e gönder): web pages cannot fake these.
	switch u.Scheme {
	case "chrome-extension", "moz-extension":
		return true
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, "giriş gerekli")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "farklı kaynaktan istek reddedildi")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr also sets "message", the one line an iOS Shortcut shows as its
// notification: without it a failed send would show an empty banner.
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg, "message": "İndirgen: " + msg})
}

func readJSON(r *http.Request, v any) error { return readJSONLimit(r, v, 1<<20) }

func readJSONLimit(r *http.Request, v any, limit int64) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("Content-Type application/json olmalı")
	}
	return json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(v)
}

// readBody reads the JSON body into v; when it cannot, it answers 400 and
// returns false.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := readJSON(r, v); err != nil {
		writeErr(w, 400, err.Error())
		return false
	}
	return true
}

func writeOK(w http.ResponseWriter, code int) { writeJSON(w, code, map[string]bool{"ok": true}) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", staticHandler())
	s.sessionRoutes(mux)
	s.engineRoutes(mux)
	s.jobRoutes(mux)
	s.settingsRoutes(mux)
	s.cookieRoutes(mux)
	s.subRoutes(mux)
	s.telegramRoutes(mux)
	s.libraryRoutes(mux)
	mux.HandleFunc("GET /api/events", s.api(s.handleEvents))
	return logRequests(s.guardHost(mux))
}

// versionedAssets are the files index.html loads. The page names each with
// the hash of its content (/app.js?v=…), so after an update a browser never
// runs a cached old file against the new page; those URLs are cached for a
// year. Everything else, the page included, is revalidated (no-cache).
var versionedAssets = []string{"app.css", "i18n.js", "app.js"}

// webCSP: no inline script or style anywhere in the UI.
const webCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' https: http: data:; " +
	"connect-src 'self'; media-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// webAssets: index.html with the versioned asset URLs, and the versions;
// worked out once.
var webAssets = sync.OnceValues(func() ([]byte, map[string]string) {
	static, _ := fs.Sub(webFS, "web")
	return versionedIndex(static)
})

// assetURL is the versioned URL of a web asset ("/app.css?v=…"), for pages
// built outside index.html.
func assetURL(name string) string {
	_, v := webAssets()
	return "/" + name + "?v=" + v[name]
}

// staticHandler serves the web UI with its security headers.
func staticHandler() http.HandlerFunc {
	static, _ := fs.Sub(webFS, "web")
	index, versions := webAssets()
	fileServer := http.FileServer(http.FS(static))
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", webCSP)
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" || name == "index.html" {
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		if v := r.URL.Query().Get("v"); v != "" && v == versions[name] {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		if strings.HasSuffix(name, ".webmanifest") {
			h.Set("Content-Type", "application/manifest+json")
		}
		fileServer.ServeHTTP(w, r)
	}
}

// versionedIndex returns index.html with ?v=<hash> on the versioned assets,
// and those hashes.
func versionedIndex(static fs.FS) ([]byte, map[string]string) {
	page, err := fs.ReadFile(static, "index.html")
	if err != nil {
		panic(err) // embedded: cannot happen in a built binary
	}
	versions := map[string]string{}
	for _, name := range versionedAssets {
		raw, err := fs.ReadFile(static, name)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(raw)
		versions[name] = hex.EncodeToString(sum[:])[:12]
		page = bytes.Replace(page, []byte(`"/`+name+`"`), []byte(`"/`+name+`?v=`+versions[name]+`"`), 1)
	}
	return page, versions
}

func (s *Server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"authed": s.authed(r), "auth_required": s.cfg.AuthRequired(), "version": version})
	})
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/pair", s.api(s.handlePairCode))
	mux.HandleFunc("GET /pair/{code}", s.handlePair)
	mux.HandleFunc("POST /pair", s.handlePairPost)
	mux.HandleFunc("GET /api/sessions", s.api(s.handleSessions))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.api(s.handleSessionDelete))
	mux.HandleFunc("POST /api/sessions/revoke-others", s.api(s.handleSessionsRevokeOthers))
	mux.HandleFunc("POST /api/token/rotate", s.api(s.handleTokenRotate))
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if x, ok := s.currentSession(r); ok {
			s.sessions.Delete(x.ID)
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		writeOK(w, 200)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Token string }
	if err := readJSON(r, &body); err != nil || !sameOrigin(r) {
		writeErr(w, 400, "geçersiz istek")
		return
	}
	if !s.tokenMatches(strings.TrimSpace(body.Token)) {
		time.Sleep(700 * time.Millisecond)
		writeErr(w, 401, "token hatalı")
		return
	}
	s.setSession(w, r, "token")
	writeOK(w, 200)
}

// engineRoutes: İndirgen's own updates and the engines.
func (s *Server) engineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/about", s.api(s.handleAbout))
	mux.HandleFunc("POST /api/about/check", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.upd.Check(r.Context()))
	}))
	mux.HandleFunc("POST /api/about/install", s.api(func(w http.ResponseWriter, r *http.Request) {
		// Not tied to the request: closing the tab must not abort a swap.
		ver, err := s.upd.Install(s.ctx)
		if err != nil {
			writeErr(w, 409, err.Error())
			return
		}
		writeJSON(w, 202, map[string]string{"version": ver})
		s.restartSoon()
	}))
	mux.HandleFunc("GET /api/engines", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.eng.States())
	}))
	mux.HandleFunc("POST /api/engines/update", s.api(func(w http.ResponseWriter, r *http.Request) {
		if until := s.eng.RateLimitUntil(); !until.IsZero() { // GitHub said wait: the check would only be refused
			writeJSON(w, 200, map[string]time.Time{"rate_limit_until": until})
			return
		}
		go s.eng.UpdateAll(s.ctx, r.URL.Query().Get("force") == "1")
		writeOK(w, 202)
	}))
	mux.HandleFunc("POST /api/engines/{name}/rollback", s.api(func(w http.ResponseWriter, r *http.Request) {
		if err := s.eng.Rollback(r.PathValue("name")); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.hub.Publish("engines", s.eng.States())
		writeOK(w, 200)
	}))
	mux.HandleFunc("POST /api/engines/{name}/unhold", s.api(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if _, ok := specByName(name); !ok {
			writeErr(w, 400, "bilinmeyen motor")
			return
		}
		go func() { _ = s.eng.Unhold(s.ctx, name) }()
		writeOK(w, 202)
	}))
	mux.HandleFunc("POST /api/engines/channel", s.api(s.handleChannel))
}

// jobRoutes: looking at links, adding, and the download queue.
func (s *Server) jobRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/probe", s.api(s.handleProbe))
	mux.HandleFunc("POST /api/browse", s.api(s.handleBrowse))
	mux.HandleFunc("POST /api/add", s.api(s.handleAdd))
	mux.HandleFunc("GET /api/jobs", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.queue.List())
	}))
	mux.HandleFunc("POST /api/jobs", s.api(s.handleAddJobs))
	mux.HandleFunc("POST /api/jobs/clear", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.queue.ClearFinished()
		writeOK(w, 200)
	}))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.queue.Cancel(r.PathValue("id")))
	}))
	mux.HandleFunc("POST /api/jobs/{id}/now", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.queue.StartNow(r.PathValue("id")))
	}))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.queue.Retry(r.PathValue("id")))
	}))
	mux.HandleFunc("DELETE /api/jobs/{id}", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.queue.Remove(r.PathValue("id"), r.URL.Query().Get("files") == "1"))
	}))
	mux.HandleFunc("GET /api/jobs/{id}/file/{n}", s.api(s.handleFile))
	mux.HandleFunc("GET /api/jobs/{id}/zip", s.api(s.handleZip))
	mux.HandleFunc("GET /api/queue", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.queue.State())
	}))
	mux.HandleFunc("POST /api/queue", s.api(func(w http.ResponseWriter, r *http.Request) {
		var body QueueSettings
		if !readBody(w, r, &body) {
			return
		}
		if err := s.cfg.SetQueueSettings(body); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.queue.schedule() // apply now: state and banner follow at once
		writeJSON(w, 200, s.queue.State())
	}))
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL, Section string
		Start, Count int
	}
	if !readBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.router.Browse(ctx, body.URL, body.Section, body.Start, body.Count, s.cfg.Pacing())
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// settingsRoutes: bot protection, storage, backup and network access.
func (s *Server) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings", s.api(func(w http.ResponseWriter, r *http.Request) {
		req, file := s.cfg.CustomPacing()
		writeJSON(w, 200, map[string]any{"pacing": s.cfg.Pacing(), "presets": pacePresets,
			"custom": map[string]string{"sleep_request": req, "sleep_file": file}})
	}))
	mux.HandleFunc("POST /api/settings", s.api(func(w http.ResponseWriter, r *http.Request) {
		var p Pacing
		if !readBody(w, r, &p) {
			return
		}
		if err := s.cfg.SetPacing(p); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, s.cfg.Pacing())
	}))
	mux.HandleFunc("GET /api/storage", s.api(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.storageInfo()) }))
	mux.HandleFunc("POST /api/storage", s.api(s.handleStorage))
	mux.HandleFunc("GET /api/dirs", s.api(s.handleDirs))
	mux.HandleFunc("POST /api/dirs", s.api(s.handleMkdir))
	mux.HandleFunc("GET /api/backup", s.api(s.handleBackup))
	mux.HandleFunc("POST /api/backup", s.api(s.handleRestore))
	mux.HandleFunc("GET /api/connect", s.api(func(w http.ResponseWriter, r *http.Request) {
		ci := s.connectInfo()
		if s.cfg.ListenOverridden() {
			ci.CanEdit = false // -listen on the command line wins over the toggle
		}
		writeJSON(w, 200, ci)
	}))
	mux.HandleFunc("POST /api/connect", s.api(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LAN bool `json:"lan"`
		}
		if !readBody(w, r, &body) {
			return
		}
		if s.cfg.ListenOverridden() {
			writeErr(w, 409, "İndirgen -listen ile başlatılmış; ayar komut satırından geliyor")
			return
		}
		if err := s.setLAN(body.LAN); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, 202)
		s.restartSoon()
	}))
}

func (s *Server) cookieRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cookies", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.cookies.List())
	}))
	mux.HandleFunc("GET /api/cookies/browser", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.browserCookieInfo())
	}))
	mux.HandleFunc("POST /api/cookies/browser", s.api(s.handleBrowserCookies))
	// analyze: which sites does this export contain? Nothing is stored.
	mux.HandleFunc("POST /api/cookies/analyze", s.api(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Content string }
		if err := readJSONLimit(r, &body, maxCookieInput+(1<<20)); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		a, err := s.cookies.Analyze([]byte(body.Content))
		if err != nil {
			writeErr(w, 422, err.Error())
			return
		}
		writeJSON(w, 200, a)
	}))
	// import: store only the selected sites (or the site of "host").
	mux.HandleFunc("POST /api/cookies", s.api(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Host    string   `json:"host"`
			Sites   []string `json:"sites"`
			Content string   `json:"content"`
		}
		if err := readJSONLimit(r, &body, maxCookieInput+(1<<20)); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if len(body.Sites) == 0 && body.Host != "" {
			body.Sites = []string{body.Host}
		}
		saved, err := s.cookies.Import([]byte(body.Content), body.Sites)
		if err != nil {
			writeErr(w, 422, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"saved": saved})
	}))
	mux.HandleFunc("DELETE /api/cookies/{host}", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.cookies.Delete(r.PathValue("host")))
	}))
}

func (s *Server) subRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/subs", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"subs": s.subs.List(), "intervals": subIntervals, "limits": subLimits})
	}))
	mux.HandleFunc("POST /api/subs", s.api(func(w http.ResponseWriter, r *http.Request) {
		var n NewSub
		if !readBody(w, r, &n) {
			return
		}
		v, err := s.subs.Add(n)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, v)
	}))
	mux.HandleFunc("PATCH /api/subs/{id}", s.api(func(w http.ResponseWriter, r *http.Request) {
		var p SubPatch
		if !readBody(w, r, &p) {
			return
		}
		s.result(w, s.subs.Patch(r.PathValue("id"), p))
	}))
	mux.HandleFunc("POST /api/subs/{id}/check", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.subs.CheckNow(r.PathValue("id")))
	}))
	mux.HandleFunc("DELETE /api/subs/{id}", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.subs.Delete(r.PathValue("id")))
	}))
}

func (s *Server) telegramRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/telegram", s.api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.tg.Status())
	}))
	mux.HandleFunc("POST /api/telegram", s.api(s.handleTelegram))
	mux.HandleFunc("POST /api/telegram/pair", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.tg.NewPairCode()
		writeJSON(w, 200, s.tg.Status())
	}))
	mux.HandleFunc("POST /api/telegram/test", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.result(w, s.tg.Test(r.Context()))
	}))
}

func (s *Server) libraryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/library", s.api(s.handleLibrary))
	mux.HandleFunc("GET /api/library/file", s.api(s.handleLibraryFile))
	mux.HandleFunc("GET /api/library/zip", s.api(s.handleLibraryZip))
	mux.HandleFunc("DELETE /api/library", s.api(s.handleLibraryDelete))
	mux.HandleFunc("POST /api/library/delete", s.api(s.handleLibraryDeleteMany))
}

// restartSoon restarts İndirgen once the response has gone out.
func (s *Server) restartSoon() {
	if s.restart != nil {
		go func() { time.Sleep(300 * time.Millisecond); s.restart() }()
	}
}

func (s *Server) result(w http.ResponseWriter, err error) {
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, 200)
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, AboutInfo{
		Project:   project,
		Build:     buildInfo(),
		Update:    s.upd.State(),
		Changelog: parseChangelog(changelogRaw, 2, 8),
		Data:      s.cfg.dataDir,
		Library:   s.cfg.Library,
		FreeBytes: diskFree(s.cfg.Library),
		Retention: s.cfg.TempRetentionHours,
		Engines:   s.eng.States(),
		UpdateChk: s.cfg.UpdateCheck,
		Install:   s.upd.Mode(),
	})
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	var body struct{ URL, Engine string }
	if !readBody(w, r, &body) {
		return
	}
	if body.Engine != "" && body.Engine != EngYTDLP && body.Engine != EngGalleryDL {
		writeErr(w, 400, "bilinmeyen motor")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	body.URL = resolveShortLink(ctx, body.URL) // share links → the video's own link, before "already downloaded?"
	res, err := s.router.Probe(ctx, body.URL, body.Engine)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	if ex, ok := FindExisting(s.queue, s.lib, res.URL); ok && res.Profile == nil {
		v := ex.View(s.lib)
		res.Existing = &v
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleAddJobs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Jobs []NewJob `json:"jobs"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if len(body.Jobs) == 0 || len(body.Jobs) > 200 {
		writeErr(w, 400, "1–200 arası iş gönderilmeli")
		return
	}
	var added []JobView
	var errs []string
	for _, n := range body.Jobs {
		v, err := s.queue.Add(n)
		if err != nil {
			errs = append(errs, n.URL+": "+err.Error())
			continue
		}
		added = append(added, v)
	}
	writeJSON(w, 200, map[string]any{"added": added, "errors": errs})
}

func (s *Server) jobFiles(w http.ResponseWriter, id string) (Job, []string, bool) {
	j, ok := s.queue.Get(id)
	if !ok {
		writeErr(w, 404, "iş bulunamadı")
		return j, nil, false
	}
	var files []string
	for _, f := range j.Files {
		if s.queue.allowedPath(f) {
			if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		writeErr(w, 404, "dosya yok (silinmiş ya da süresi dolmuş olabilir)")
		return j, nil, false
	}
	return j, files, true
}

func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

// handleFile serves file n. n indexes the job's full file list, the same
// list the UI numbers (file_names): indexing the filtered list instead
// served the wrong file once an earlier file had been deleted.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	j, ok := s.queue.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "iş bulunamadı")
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= len(j.Files) || !s.queue.allowedPath(j.Files[n]) {
		writeErr(w, 404, "dosya yok")
		return
	}
	path := j.Files[n]
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 404, "dosya yok (silinmiş ya da süresi dolmuş olabilir)")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		writeErr(w, 404, "dosya yok")
		return
	}
	name := filepath.Base(path)
	serveContent(w, r, f, st, name, r.URL.Query().Get("inline") != "1")
}

// serveContent sends an open file with its media type, as a download unless
// attach is false (the preview). Range requests work: seeking in videos.
func serveContent(w http.ResponseWriter, r *http.Request, f *os.File, st fs.FileInfo, name string, attach bool) {
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if attach {
		w.Header().Set("Content-Disposition", contentDisposition(name))
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// handleZip streams all files of a job as a ZIP (store, no recompression:
// media is already compressed). Nothing is written to disk.
func (s *Server) handleZip(w http.ResponseWriter, r *http.Request) {
	j, files, ok := s.jobFiles(w, r.PathValue("id"))
	if !ok {
		return
	}
	name := sanitizeFilename(j.Title)
	name = cmp.Or(name, "indirgen-"+j.ID)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(name+".zip"))
	zw := zip.NewWriter(w)
	used := map[string]int{}
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		st, _ := f.Stat()
		base := filepath.Base(p)
		if c := used[base]; c > 0 {
			ext := filepath.Ext(base)
			base = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(base, ext), c, ext)
		}
		used[filepath.Base(p)]++
		hdr, _ := zip.FileInfoHeader(st)
		hdr.Name, hdr.Method = base, zip.Store
		zf, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.Copy(zf, f)
		}
		f.Close()
		if err != nil {
			return // client went away
		}
	}
	_ = zw.Close()
}

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	off, _ := strconv.Atoi(v.Get("offset"))
	lim, _ := strconv.Atoi(v.Get("limit"))
	res, err := s.lib.List(LibQuery{Path: v.Get("path"), Q: v.Get("q"), Sort: v.Get("sort"),
		Flat: v.Get("flat") == "1", Offset: off, Limit: lim})
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleLibraryFile(w http.ResponseWriter, r *http.Request) {
	f, st, err := s.lib.OpenFile(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	defer f.Close()
	name := st.Name()
	serveContent(w, r, f, st, name, r.URL.Query().Get("inline") != "1")
}

// handleLibraryZip streams a folder as a ZIP (stored, not recompressed).
func (s *Server) handleLibraryZip(w http.ResponseWriter, r *http.Request) {
	var zw *zip.Writer
	_, err := s.lib.Walk(r.URL.Query().Get("path"), func(rel string, f *os.File, st fs.FileInfo) error {
		if zw == nil { // headers only once there is something to send
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", contentDisposition(s.lib.ZipName(r.URL.Query().Get("path"))))
			zw = zip.NewWriter(w)
		}
		hdr, err := zip.FileInfoHeader(st)
		if err != nil {
			return nil
		}
		hdr.Name, hdr.Method = rel, zip.Store
		zf, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.Copy(zf, f)
		}
		return err // client went away: stop walking
	})
	if zw == nil {
		if err != nil {
			writeErr(w, 404, err.Error())
		} else {
			writeErr(w, 404, "klasörde dosya yok")
		}
		return
	}
	_ = zw.Close()
}

func (s *Server) handleLibraryDelete(w http.ResponseWriter, r *http.Request) {
	res, abs, err := s.lib.Delete(r.URL.Query().Get("path"))
	if err != nil {
		code := 400
		if errors.Is(err, errLibNotFound) {
			code = 404
		}
		writeErr(w, code, err.Error())
		return
	}
	slog.Info("arşivden silindi", "yol", abs, "dosya", res.Files, "boyut", humanBytes(res.Bytes))
	s.queue.ForgetFiles(abs)
	s.hub.Publish("library", nil)
	writeJSON(w, 200, res)
}

// handleLibraryDeleteMany deletes a selection. Paths inside a folder that is
// also selected are skipped: the folder takes them along.
func (s *Server) handleLibraryDeleteMany(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := readJSON(r, &body); err != nil || len(body.Paths) == 0 || len(body.Paths) > 5000 {
		writeErr(w, 400, "silinecek bir şey seçilmedi")
		return
	}
	paths := make([]string, 0, len(body.Paths))
	for _, p := range body.Paths {
		if c, err := cleanRel(p); err == nil && c != "" {
			paths = append(paths, c)
		}
	}
	slices.Sort(paths) // parents before their children
	type failure struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	}
	var total LibDeleted
	failed := []failure{}
	var done []string
	for _, p := range slices.Compact(paths) {
		if slices.ContainsFunc(done, func(d string) bool { return strings.HasPrefix(p, d+"/") }) {
			continue
		}
		res, abs, err := s.lib.Delete(p)
		if err != nil {
			failed = append(failed, failure{p, err.Error()})
			continue
		}
		done = append(done, p)
		total.Files += res.Files
		total.Bytes += res.Bytes
		s.queue.ForgetFiles(abs)
	}
	slog.Info("arşivden toplu silindi", "öğe", len(done), "dosya", total.Files, "boyut", humanBytes(total.Bytes), "silinemeyen", len(failed))
	s.hub.Publish("library", nil)
	writeJSON(w, 200, map[string]any{"files": total.Files, "bytes": total.Bytes, "failed": failed})
}

// handleTelegram saves bot settings. A new token is checked with getMe
// before it is stored, so a typo never replaces a working bot.
func (s *Server) handleTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token      *string `json:"token"`
		Quiet      *bool   `json:"quiet"`
		NoFiles    *bool   `json:"no_files"`
		RemoveChat int64   `json:"remove_chat"`
		Disconnect bool    `json:"disconnect"`
	}
	if !readBody(w, r, &body) {
		return
	}
	restart, botName := false, ""
	if body.Token != nil {
		tok := strings.TrimSpace(*body.Token)
		if !tgTokenRe.MatchString(tok) {
			writeErr(w, 400, "bot token biçimi hatalı (BotFather'dan gelen 123456:ABC… değeri)")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		name, err := s.tg.getMe(ctx, tok)
		cancel()
		botName = name
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		body.Token, restart = &tok, true
	}
	err := s.cfg.SetTelegram(func(c *TelegramConfig) {
		if body.Token != nil {
			c.Token = *body.Token
		}
		if body.Quiet != nil {
			c.Quiet = *body.Quiet
		}
		if body.NoFiles != nil {
			c.NoFiles = *body.NoFiles
		}
		if body.RemoveChat != 0 {
			c.Chats = slices.DeleteFunc(c.Chats, func(id int64) bool { return id == body.RemoveChat })
		}
		if body.Disconnect {
			*c, restart = TelegramConfig{}, true
		}
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if restart {
		s.tg.Reconfigure()
		if body.Token != nil {
			s.tg.setConn(true, botName, nil) // getMe just worked; the pairing link can use the name
			s.tg.NewPairCode()               // next step: pair a chat
		}
	}
	writeJSON(w, 200, s.tg.Status())
}

var tgTokenRe = regexp.MustCompile(`^\d{5,15}:[A-Za-z0-9_-]{30,50}$`)

func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return -1
		}
		return r
	}, s)
	return clip(strings.TrimSpace(s), 120)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming desteklenmiyor")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)
	var ended <-chan struct{} // a revoked session's stream stops at once
	if x, ok := s.currentSession(r); ok {
		ended = x.done
	}

	snap, _ := json.Marshal(map[string]any{"jobs": s.queue.List(), "engines": s.eng.States(), "queue": s.queue.State()})
	fmt.Fprintf(w, "retry: 3000\nevent: snapshot\ndata: %s\n\n", snap)
	fl.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-ended:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			// A real event, not an SSE comment: the page uses it to notice a
			// connection that died silently (sleep, network change).
			if _, err := io.WriteString(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int)           { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter } // for http.ResponseController
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t := time.Now()
		h.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/events" {
			slog.Debug("istek", "yöntem", r.Method, "yol", r.URL.Path, "kod", sw.code, "süre", time.Since(t).Round(time.Millisecond))
		}
	})
}
