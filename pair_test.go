package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type pairResp struct {
	Expires time.Time `json:"expires"`
	Links   []struct {
		URL string `json:"url"`
		SVG string `json:"svg"`
	} `json:"links"`
}

// pairReq sends a request from remote (host:port) with optional cookies.
func pairReq(h http.Handler, method, path, remote string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newPairCode(t *testing.T, h http.Handler) (code string, res pairResp) {
	t.Helper()
	r := pairReq(h, "POST", "/api/pair", "127.0.0.1:5000")
	if r.Code != 200 {
		t.Fatalf("pair from this computer: %d %s", r.Code, r.Body)
	}
	json.Unmarshal(r.Body.Bytes(), &res)
	if len(res.Links) == 0 {
		t.Fatalf("no links: %s", r.Body)
	}
	_, code, _ = strings.Cut(res.Links[0].URL, "/pair/")
	return code, res
}

func sessionOf(r *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range r.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

// Only this computer or a browser that is already signed in makes codes.
func TestPairCodeOnlyFromThisComputerOrSession(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	if r := pairReq(h, "POST", "/api/pair", "192.168.1.50:40000"); r.Code != 401 {
		t.Fatalf("stranger: %d", r.Code)
	}
	// The token alone (a Shortcut, the extension) is not a signed-in browser.
	req := httptest.NewRequest("POST", "/api/pair", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.RemoteAddr = "192.168.1.50:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("bearer: %d", rec.Code)
	}
	signed := login(t, s, h, "192.168.1.50:40000", "")
	if r := pairReq(h, "POST", "/api/pair", "192.168.1.50:40000", signed); r.Code != 200 {
		t.Fatalf("signed-in phone: %d %s", r.Code, r.Body)
	}
	// "never" lets everyone in, but only a session or this computer pairs.
	s.cfg.Auth = "never"
	if r := pairReq(h, "POST", "/api/pair", "192.168.1.50:40000"); r.Code != 403 {
		t.Fatalf("auth never, stranger: %d", r.Code)
	}
	// A web page cannot make one (CSRF) nor open one (Host check).
	req = httptest.NewRequest("POST", "/api/pair", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.RemoteAddr = "127.0.0.1:5000"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("cross-site: %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/pair/abc", nil)
	req.Host = "evil.example"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host: %d", rec.Code)
	}
}

func TestPairNeedsNetworkAccess(t *testing.T) {
	_, h := testServer(t, "auto", "127.0.0.1:8899")
	r := pairReq(h, "POST", "/api/pair", "127.0.0.1:5000")
	if r.Code != 409 || !strings.Contains(r.Body.String(), "ağ erişimi") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

// pairPost submits the pairing form like a browser on this origin; origin
// "" sends no Origin header.
func pairPost(h http.Handler, code, remote, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/pair", strings.NewReader(url.Values{"code": {code}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const pairOrigin = "http://example.com" // httptest's Host

// Opening the link (a QR scanner's in-app browser, a link preview) spends
// nothing; the button on the page (a POST) does, once.
func TestPairGetShowsButtonPostSpends(t *testing.T) {
	_, h := testServer(t, "auto", "192.168.1.10:8899")
	code, res := newPairCode(t, h)
	if len(code) < 32 || strings.Trim(code, "0123456789abcdef") != "" {
		t.Fatalf("code %q: want 128 bits or more, hex", code)
	}
	if d := time.Until(res.Expires); d < 4*time.Minute || d > 5*time.Minute+time.Second {
		t.Fatalf("expires in %v, want 5 minutes", d)
	}
	for range 2 { // the scanner's browser, then Safari
		r := pairReq(h, "GET", "/pair/"+code, "192.168.1.50:40000")
		body := r.Body.String()
		if r.Code != 200 || sessionOf(r) != nil || !strings.Contains(r.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("GET: %d %v", r.Code, r.Header())
		}
		for _, want := range []string{`method="post"`, `action="/pair"`, `name="code" value="` + code + `"`,
			"Bu cihazı İndirgen'e bağla", "Safari"} {
			if !strings.Contains(body, want) {
				t.Fatalf("page lacks %q:\n%s", want, body)
			}
		}
		// no-referrer would make the browser send "Origin: null" with the form
		if rp := r.Header().Get("Referrer-Policy"); rp != "same-origin" {
			t.Fatalf("Referrer-Policy %q", rp)
		}
	}

	r := pairPost(h, code, "192.168.1.50:40000", pairOrigin)
	c := sessionOf(r)
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/" || c == nil {
		t.Fatalf("POST: %d %v %v", r.Code, r.Header(), c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 90*86400 {
		t.Fatalf("cookie %+v", c)
	}
	if r := pairReq(h, "GET", "/api/jobs", "192.168.1.50:40000", c); r.Code != 200 {
		t.Fatalf("paired phone refused: %d", r.Code)
	}
	// Spent: the button again, or the link again, gets nothing.
	if r := pairPost(h, code, "192.168.1.51:40000", pairOrigin); r.Code != 404 || sessionOf(r) != nil || !strings.Contains(r.Body.String(), "geçersiz") {
		t.Fatalf("second POST: %d %q", r.Code, r.Body)
	}
	if r := pairReq(h, "GET", "/pair/"+code, "192.168.1.51:40000"); r.Code != 404 {
		t.Fatalf("GET after use: %d", r.Code)
	}
	// A new code replaces the old one.
	c1, _ := newPairCode(t, h)
	c2, _ := newPairCode(t, h)
	if r := pairPost(h, c1, "192.168.1.52:40000", pairOrigin); r.Code != 404 {
		t.Fatalf("replaced code still works: %d", r.Code)
	}
	if r := pairPost(h, c2, "192.168.1.52:40000", pairOrigin); r.Code != 303 {
		t.Fatalf("new code: %d", r.Code)
	}
}

func TestPairCodeExpires(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	now := time.Now()
	s.pair.now = func() time.Time { return now }
	code, _ := newPairCode(t, h)
	now = now.Add(5*time.Minute + time.Second)
	if r := pairReq(h, "GET", "/pair/"+code, "192.168.1.50:40000"); r.Code != 404 {
		t.Fatalf("GET of an expired code: %d", r.Code)
	}
	if r := pairPost(h, code, "192.168.1.50:40000", pairOrigin); r.Code != 404 || sessionOf(r) != nil {
		t.Fatalf("POST of an expired code: %d", r.Code)
	}
}

// The form must come from İndirgen's own page: no Origin, "null" or another
// site's Origin is refused, and the code stays usable.
func TestPairPostNeedsOwnOrigin(t *testing.T) {
	_, h := testServer(t, "auto", "192.168.1.10:8899")
	code, _ := newPairCode(t, h)
	for _, o := range []string{"", "null", "https://evil.example", "http://example.com.evil.example", "chrome-extension://abc"} {
		if r := pairPost(h, code, "192.168.1.50:1", o); r.Code != 403 || sessionOf(r) != nil {
			t.Fatalf("Origin %q: %d", o, r.Code)
		}
	}
	if r := pairPost(h, code, "192.168.1.50:1", pairOrigin); r.Code != 303 {
		t.Fatalf("refused POSTs spent the code: %d", r.Code)
	}
}

func TestPairAttemptLimit(t *testing.T) {
	_, h := testServer(t, "auto", "192.168.1.10:8899")
	code, _ := newPairCode(t, h)
	for i := range 3 {
		if r := pairReq(h, "GET", "/pair/"+strings.Repeat("0", 32), "192.168.1.66:1"); r.Code != 404 {
			t.Fatalf("GET %d: %d", i+1, r.Code)
		}
	}
	for i := range 2 {
		if r := pairPost(h, strings.Repeat("0", 32), "192.168.1.66:1", pairOrigin); r.Code != 404 {
			t.Fatalf("POST %d: %d", i+1, r.Code)
		}
	}
	// Over the limit (GETs and POSTs count together): refused, even with the
	// right code, from that address.
	if r := pairReq(h, "GET", "/pair/"+code, "192.168.1.66:2"); r.Code != http.StatusTooManyRequests {
		t.Fatalf("GET over the limit: %d", r.Code)
	}
	if r := pairPost(h, code, "192.168.1.66:2", pairOrigin); r.Code != http.StatusTooManyRequests || sessionOf(r) != nil {
		t.Fatalf("POST over the limit: %d", r.Code)
	}
	// Another address is not affected, and the code still works there.
	if r := pairReq(h, "GET", "/pair/"+code, "192.168.1.20:1"); r.Code != 200 {
		t.Fatalf("other address, GET: %d", r.Code)
	}
	if r := pairPost(h, code, "192.168.1.20:1", pairOrigin); r.Code != 303 {
		t.Fatalf("other address, POST: %d", r.Code)
	}
}

// Like the token, pairing codes never reach the log.
func TestPairCodeNotLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	_, h := testServer(t, "auto", "192.168.1.10:8899")
	code, _ := newPairCode(t, h)
	pairReq(h, "GET", "/pair/"+strings.Repeat("1", 32), "192.168.1.50:1")
	pairReq(h, "GET", "/pair/"+code, "192.168.1.50:1")
	pairPost(h, code, "192.168.1.50:1", pairOrigin)
	if strings.Contains(buf.String(), code) || strings.Contains(buf.String(), strings.Repeat("1", 32)) {
		t.Fatalf("code in the log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "telefon bağlandı") {
		t.Fatalf("pairing not logged at all:\n%s", buf.String())
	}
}

// The short code is for a home-screen app (its own cookie jar, no camera
// link): typed by hand, so 8 characters without look-alikes, shown 4-4.
func TestPairShortCode(t *testing.T) {
	_, h := testServer(t, "auto", "192.168.1.10:8899")
	shortRe := regexp.MustCompile(`^[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}$`)
	seen := map[rune]bool{}
	for range 200 {
		r := pairReq(h, "POST", "/api/pair", "127.0.0.1:5000")
		var res struct{ Short string }
		json.Unmarshal(r.Body.Bytes(), &res)
		if !shortRe.MatchString(res.Short) {
			t.Fatalf("short code %q", res.Short)
		}
		for _, c := range res.Short {
			seen[c] = true
		}
	}
	for _, bad := range "01OIl" {
		if seen[bad] {
			t.Fatalf("look-alike %q used", bad)
		}
	}

	var full struct{ Short string }
	r := pairReq(h, "POST", "/api/pair", "127.0.0.1:5000")
	json.Unmarshal(r.Body.Bytes(), &full)
	long := strings.SplitN(lastPairLink(t, r), "/pair/", 2)[1]
	// Typed loosely: lower case, no dash, spaces.
	typed := " " + strings.ToLower(strings.Replace(full.Short, "-", " ", 1)) + " "
	if r := pairPost(h, typed, "192.168.1.50:1", pairOrigin); r.Code != 303 || sessionOf(r) == nil {
		t.Fatalf("short code: %d", r.Code)
	}
	// One pairing: the short code and the link share it.
	if r := pairPost(h, full.Short, "192.168.1.51:1", pairOrigin); r.Code != 404 {
		t.Fatalf("short code twice: %d", r.Code)
	}
	if r := pairPost(h, long, "192.168.1.51:1", pairOrigin); r.Code != 404 {
		t.Fatalf("link after the short code: %d", r.Code)
	}
}

func lastPairLink(t *testing.T, r *httptest.ResponseRecorder) string {
	t.Helper()
	var res pairResp
	json.Unmarshal(r.Body.Bytes(), &res)
	if len(res.Links) == 0 {
		t.Fatalf("no links: %s", r.Body)
	}
	return res.Links[0].URL
}

func TestPairShortCodeExpiresAndSharesLimit(t *testing.T) {
	s, h := testServer(t, "auto", "192.168.1.10:8899")
	now := time.Now()
	s.pair.now = func() time.Time { return now }
	var res struct{ Short string }
	r := pairReq(h, "POST", "/api/pair", "127.0.0.1:5000")
	json.Unmarshal(r.Body.Bytes(), &res)
	long := strings.SplitN(lastPairLink(t, r), "/pair/", 2)[1]
	// Wrong short codes count against the same limit as wrong links.
	for range 3 {
		pairPost(h, "ZZZZ-ZZZZ", "192.168.1.66:1", pairOrigin)
	}
	for range 2 {
		pairReq(h, "GET", "/pair/"+strings.Repeat("0", 32), "192.168.1.66:1")
	}
	if r := pairPost(h, res.Short, "192.168.1.66:1", pairOrigin); r.Code != http.StatusTooManyRequests {
		t.Fatalf("short code over the limit: %d", r.Code)
	}
	now = now.Add(5*time.Minute + time.Second)
	if r := pairPost(h, res.Short, "192.168.1.20:1", pairOrigin); r.Code != 404 {
		t.Fatalf("expired short code: %d", r.Code)
	}
	if r := pairPost(h, long, "192.168.1.20:1", pairOrigin); r.Code != 404 {
		t.Fatalf("expired link: %d", r.Code)
	}
}

func TestPairShortCodeNotLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	_, h := testServer(t, "auto", "192.168.1.10:8899")
	var res struct{ Short string }
	r := pairReq(h, "POST", "/api/pair", "127.0.0.1:5000")
	json.Unmarshal(r.Body.Bytes(), &res)
	pairPost(h, "ABCD-EFGH", "192.168.1.50:1", pairOrigin)
	pairPost(h, res.Short, "192.168.1.50:1", pairOrigin)
	for _, c := range []string{res.Short, strings.ReplaceAll(res.Short, "-", ""), "ABCD"} {
		if strings.Contains(buf.String(), c) {
			t.Fatalf("code %q in the log:\n%s", c, buf.String())
		}
	}
}

// The pairing pages use the app's own stylesheet: no inline style, and a
// CSP without 'unsafe-inline'.
func TestPairPagesUseAppCSS(t *testing.T) {
	_, h := testServer(t, "auto", "192.168.1.10:8899")
	code, _ := newPairCode(t, h)
	pages := map[string]*httptest.ResponseRecorder{
		"form":    pairReq(h, "GET", "/pair/"+code, "192.168.1.50:1"),
		"invalid": pairReq(h, "GET", "/pair/"+strings.Repeat("0", 32), "192.168.1.51:1"),
		"origin":  pairPost(h, code, "192.168.1.52:1", "https://evil.example"),
	}
	for range 5 {
		pairReq(h, "GET", "/pair/"+strings.Repeat("1", 32), "192.168.1.53:1")
	}
	pages["limited"] = pairReq(h, "GET", "/pair/"+code, "192.168.1.53:1")
	css := `<link rel="stylesheet" href="/app.css?v=` + assetHash(t, "app.css") + `">`
	for name, r := range pages {
		body, csp := r.Body.String(), r.Header().Get("Content-Security-Policy")
		if strings.Contains(body, ` style="`) || strings.Contains(body, "<style") {
			t.Errorf("%s: inline style", name)
		}
		if !strings.Contains(body, css) {
			t.Errorf("%s: does not load %s", name, css)
		}
		if strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "style-src 'self'") || !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP %q", name, csp)
		}
	}
}
