package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Phone pairing: Settings → Phone and extension → "Connect a phone" shows a
// QR code of http://<LAN address>:<port>/pair/<code>. Opening it shows a
// button; pressing it (POST /pair) signs the phone in like the token does
// (the 90-day session cookie), so nothing has to be typed on the phone.
// Opening the link spends nothing: the iPhone code scanner opens it in an
// in-app browser with its own cookies, and link previews fetch it too.
//
// A code is 128 random bits, valid for 5 minutes and for one use; a new code
// replaces the old one. With it comes a short code (8 characters, no 0/O or
// 1/I, shown "ABCD-EFGH") for a home-screen app, which has its own cookies
// and no link to open: typed on the login screen, it is the same pairing
// and uses it up just the same. Only this computer or a browser that is already
// signed in can make one. Like the token, codes are never logged (the
// request log only writes /api/ paths). Wrong codes count against the
// client's address: 5 a minute, then it waits.
// ---------------------------------------------------------------------------

const (
	pairValid    = 5 * time.Minute
	pairMaxFails = 5 // per address and minute
)

type pairing struct {
	mu    sync.Mutex
	code  string
	short string // without the dash
	until time.Time
	fails map[string][]time.Time // client address → recent wrong attempts
	now   func() time.Time       // tests set the clock
}

func (p *pairing) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// shortAlphabet: 32 characters, none of them easy to misread.
const shortAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// newCode makes a code and its short form, and forgets the previous ones.
// short is shown as "ABCD-EFGH".
func (p *pairing) newCode() (code, short string, until time.Time) {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	sb := make([]byte, 8)
	_, _ = rand.Read(sb)
	for i := range sb {
		sb[i] = shortAlphabet[sb[i]%32] // 256 is a multiple of 32: no bias
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code, p.short, p.until = hex.EncodeToString(b), string(sb), p.clock().Add(pairValid)
	return p.code, p.short[:4] + "-" + p.short[4:], p.until
}

// normShort turns what was typed ("abcd efgh", "ABCD-EFGH") into the stored
// form; "" when it cannot be a short code.
func normShort(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) != 8 {
		return ""
	}
	return s
}

// try checks the code and, with spend, uses it up. Wrong codes count
// against addr; limited: addr tried too often.
func (p *pairing) try(addr, code string, spend bool) (ok, limited bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	for a, ts := range p.fails { // forget attempts older than a minute
		recent := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < time.Minute {
				recent = append(recent, t)
			}
		}
		if len(recent) == 0 {
			delete(p.fails, a)
		} else {
			p.fails[a] = recent
		}
	}
	if len(p.fails[addr]) >= pairMaxFails {
		return false, true
	}
	long := p.code != "" && subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) == 1
	short := p.short != "" && subtle.ConstantTimeCompare([]byte(normShort(code)), []byte(p.short)) == 1
	if (long || short) && now.Before(p.until) {
		if spend {
			p.code, p.short = "", ""
		}
		return true, false
	}
	if p.fails == nil {
		p.fails = map[string][]time.Time{}
	}
	p.fails[addr] = append(p.fails[addr], now)
	return false, false
}

type pairLink struct {
	URL string `json:"url"`
	SVG string `json:"svg"` // the QR code
}

// handlePairCode (POST /api/pair) makes a code and a link with its QR code
// for every address the phone may reach this computer on.
func (s *Server) handlePairCode(w http.ResponseWriter, r *http.Request) {
	if !s.browserOrLocal(r) {
		writeErr(w, 403, "telefonu bu bilgisayardan ya da oturum açmış bir tarayıcıdan bağla")
		return
	}
	ci := s.connectInfo()
	if !ci.LAN {
		writeErr(w, 409, "önce ağ erişimini aç: Ayarlar → Telefon ve eklenti → Ağdan erişim")
		return
	}
	code, short, until := s.pair.newCode()
	res := struct {
		Expires time.Time  `json:"expires"`
		Short   string     `json:"short"` // typed in a home-screen app
		Links   []pairLink `json:"links"`
	}{Expires: until, Short: short, Links: []pairLink{}}
	for _, u := range ci.URLs {
		link := u + "/pair/" + code
		if q, err := qrEncode(link); err == nil {
			res.Links = append(res.Links, pairLink{URL: link, SVG: q.svg()})
		}
	}
	writeJSON(w, 200, res)
}

// handlePair (GET /pair/{code}) shows the button that pairs this browser.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	ok, limited := s.pair.try(clientAddr(r), code, false)
	if !pairRefused(w, ok, limited) {
		pairForm(w, code)
	}
}

// handlePairPost (POST /pair, the button) spends the code, signs the
// browser in and opens the app.
func (s *Server) handlePairPost(w http.ResponseWriter, r *http.Request) {
	if !ownOrigin(r) {
		pairHeaders(w, "no-referrer")
		pairPage(w, http.StatusForbidden, "Bağlantı reddedildi",
			"Bu istek İndirgen'in kendi sayfasından gelmedi. Eşleştirme bağlantısını yeniden aç.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	addr := clientAddr(r)
	ok, limited := s.pair.try(addr, r.PostFormValue("code"), true)
	if pairRefused(w, ok, limited) {
		return
	}
	s.setSession(w, r, "qr")
	slog.Info("telefon bağlandı", "adres", addr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ownOrigin: a form posted from a page of this same address. Unlike
// sameOrigin, a missing or "null" Origin is refused: browsers always send
// Origin with a POST form.
func ownOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func clientAddr(r *http.Request) string {
	if addr, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return addr
	}
	return r.RemoteAddr
}

// pairRefused answers a wrong, used, expired or too often tried code.
func pairRefused(w http.ResponseWriter, ok, limited bool) bool {
	switch {
	case limited:
		pairHeaders(w, "no-referrer")
		pairPage(w, http.StatusTooManyRequests, "Çok fazla deneme",
			"Bu cihazdan çok fazla geçersiz bağlantı açıldı. Bir dakika sonra yeniden dene.")
	case !ok:
		pairHeaders(w, "no-referrer")
		pairPage(w, http.StatusNotFound, "Bağlantı geçersiz",
			"Bu eşleştirme bağlantısı geçersiz ya da süresi dolmuş: her bağlantı 5 dakika geçerli ve tek kullanımlık. "+
				"Bilgisayardaki İndirgen'de Ayarlar → Telefon ve eklenti → Telefonu bağla ile yenisini oluştur.")
	default:
		return false
	}
	return true
}

// pairHeaders: never cached, no third-party content. The form page uses
// "same-origin": with "no-referrer" browsers send "Origin: null" with the
// form, and the address (with the code) still never leaves this origin.
func pairHeaders(w http.ResponseWriter, referrer string) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", referrer)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
}

// pairHead starts a pairing page; it is styled by the app's own stylesheet.
func pairHead(w io.Writer) {
	fmt.Fprintf(w, `<!doctype html><html lang="tr"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<title>İndirgen</title><link rel="stylesheet" href="%s"><body class="pairpage">`, assetURL("app.css"))
}

func pairForm(w http.ResponseWriter, code string) {
	pairHeaders(w, "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pairHead(w)
	fmt.Fprintf(w, `<h1>İndirgen'e bağlan</h1>`+
		`<form method="post" action="/pair"><input type="hidden" name="code" value="%s">`+
		`<button class="btn primary">Bu cihazı İndirgen'e bağla</button></form>`+
		`<p>Uygulama içi bir tarayıcıdaysan (kod tarayıcı, Telegram, Instagram) önce Safari'de ya da asıl tarayıcında aç, sonra düğmeye bas. `+
		`Oturum yalnızca düğmeye bastığın tarayıcıda açılır.</p></body></html>`, html.EscapeString(code))
}

func pairPage(w http.ResponseWriter, code int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	pairHead(w)
	fmt.Fprintf(w, `<h1>%s</h1><p>%s</p></body></html>`, title, msg)
}
