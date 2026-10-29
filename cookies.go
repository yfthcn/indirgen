package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Cookie store
//
// One Netscape cookies.txt per site (data/cookies/<site>.txt). A browser
// export may contain hundreds of sites (bank, mail…); it is analysed first
// and only the sites the user ticks are written, each into its own file.
// Cookie values never leave the server: the UI only sees counts and dates.
// ---------------------------------------------------------------------------

type CookieStore struct {
	dir     string
	browser func() string // --cookies-from-browser value ("firefox:<profile>"), "" = off
	ytdlp   func() string // yt-dlp binary, exports the browser's cookies

	mu         sync.Mutex // browser export cache, write-backs
	export     []byte
	exportSpec string
	exportAt   time.Time
}

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

const maxCookieInput = 8 << 20

func NewCookieStore(dir string) *CookieStore {
	_ = os.MkdirAll(dir, 0o700)
	return &CookieStore{dir: dir}
}

// ---- parsing --------------------------------------------------------------

type cookie struct {
	Domain     string // as exported, e.g. ".instagram.com" or "www.x.com"
	IncludeSub bool
	Path       string
	Secure     bool
	HTTPOnly   bool
	Expires    int64 // unix seconds, 0 = session cookie
	Name       string
	Value      string
}

// parseCookies accepts Netscape cookies.txt (also with #HttpOnly_ lines) and
// the JSON exports of Cookie-Editor / EditThisCookie style extensions.
func parseCookies(data []byte) ([]cookie, string, error) {
	if len(data) > maxCookieInput {
		return nil, "", errors.New("çerez dosyası çok büyük")
	}
	s := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if s == "" {
		return nil, "", errors.New("çerez içeriği boş")
	}
	if s[0] == '[' || s[0] == '{' {
		cs, err := parseCookieJSON([]byte(s))
		return cs, "json", err
	}
	cs, err := parseNetscape(s)
	return cs, "netscape", err
}

func parseNetscape(s string) ([]cookie, error) {
	var out []cookie
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		httpOnly := false
		if rest, ok := strings.CutPrefix(line, "#HttpOnly_"); ok {
			line, httpOnly = rest, true
		} else if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		exp, _ := strconv.ParseFloat(strings.TrimSpace(f[4]), 64)
		c := cookie{
			Domain: strings.TrimSpace(f[0]), IncludeSub: strings.EqualFold(f[1], "TRUE"),
			Path: f[2], Secure: strings.EqualFold(f[3], "TRUE"), HTTPOnly: httpOnly,
			Expires: int64(exp), Name: f[5], Value: strings.Join(f[6:], "\t"),
		}
		if validCookie(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("geçerli çerez bulunamadı (Netscape cookies.txt ya da Cookie-Editor JSON bekleniyor)")
	}
	return out, nil
}

type jsonCookie struct {
	Domain         string   `json:"domain"`
	Name           string   `json:"name"`
	Value          string   `json:"value"`
	Path           string   `json:"path"`
	Secure         bool     `json:"secure"`
	HTTPOnly       bool     `json:"httpOnly"`
	HostOnly       *bool    `json:"hostOnly"`
	Session        bool     `json:"session"`
	ExpirationDate *float64 `json:"expirationDate"`
	Expires        *float64 `json:"expires"` // Playwright/Puppeteer style
}

func parseCookieJSON(b []byte) ([]cookie, error) {
	var list []jsonCookie
	if err := json.Unmarshal(b, &list); err != nil {
		var wrap struct {
			Cookies []jsonCookie `json:"cookies"`
		}
		if err2 := json.Unmarshal(b, &wrap); err2 != nil || len(wrap.Cookies) == 0 {
			return nil, errors.New("JSON çerez listesi okunamadı: " + err.Error())
		}
		list = wrap.Cookies
	}
	var out []cookie
	for _, j := range list {
		c := cookie{Domain: strings.TrimSpace(j.Domain), Path: j.Path, Secure: j.Secure,
			HTTPOnly: j.HTTPOnly, Name: j.Name, Value: j.Value}
		c.Path = cmp.Or(c.Path, "/")
		switch {
		case j.HostOnly != nil:
			c.IncludeSub = !*j.HostOnly
		default:
			c.IncludeSub = strings.HasPrefix(c.Domain, ".")
		}
		if c.IncludeSub && !strings.HasPrefix(c.Domain, ".") {
			c.Domain = "." + c.Domain
		}
		if !c.IncludeSub {
			c.Domain = strings.TrimPrefix(c.Domain, ".")
		}
		if !j.Session {
			if j.ExpirationDate != nil && *j.ExpirationDate > 0 {
				c.Expires = int64(*j.ExpirationDate)
			} else if j.Expires != nil && *j.Expires > 0 {
				c.Expires = int64(*j.Expires)
			}
		}
		if validCookie(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("JSON içinde geçerli çerez yok")
	}
	return out, nil
}

func validCookie(c cookie) bool {
	if c.Name == "" || strings.ContainsAny(c.Name+c.Value+c.Path, "\t\r\n") {
		return false
	}
	_, err := normCookieHost(strings.TrimPrefix(c.Domain, "."))
	return err == nil
}

func (c cookie) netscapeLine() string {
	dom := c.Domain
	if c.HTTPOnly {
		dom = "#HttpOnly_" + dom
	}
	return strings.Join([]string{dom, tf(c.IncludeSub), c.Path, tf(c.Secure),
		strconv.FormatInt(c.Expires, 10), c.Name, c.Value}, "\t")
}

func tf(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

// ---- sites ----------------------------------------------------------------

// Two-label public suffixes that matter in practice (co.uk, com.tr, …).
var multiSuffix = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true, "com.tr": true, "org.tr": true,
	"gov.tr": true, "edu.tr": true, "net.tr": true, "gen.tr": true, "co.jp": true, "ne.jp": true,
	"or.jp": true, "com.br": true, "com.au": true, "net.au": true, "co.nz": true, "co.kr": true,
	"or.kr": true, "com.cn": true, "com.tw": true, "com.hk": true, "co.in": true, "co.za": true,
	"com.mx": true, "com.ar": true, "co.id": true, "com.sg": true, "co.il": true, "com.ua": true,
}

// siteOf reduces a cookie domain to its registrable site: ".www.instagram.com" → "instagram.com".
func siteOf(domain string) string {
	d := strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	parts := strings.Split(d, ".")
	if len(parts) <= 2 {
		return d
	}
	if multiSuffix[strings.Join(parts[len(parts)-2:], ".")] {
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func normCookieHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(strings.SplitN(h, "/", 2)[0], ".")
	h = strings.TrimPrefix(h, "www.")
	if !hostRe.MatchString(h) {
		return "", errors.New("geçersiz alan adı")
	}
	return h, nil
}

type knownSite struct {
	Label string
	Auth  []string // cookies that carry the login session
}

// Sites İndirgen actually downloads from; shown first and labelled in the UI.
var knownSites = map[string]knownSite{
	"instagram.com":   {"Instagram", []string{"sessionid"}},
	"x.com":           {"X", []string{"auth_token"}},
	"twitter.com":     {"Twitter (eski)", []string{"auth_token"}},
	"youtube.com":     {"YouTube", []string{"__Secure-3PSID", "__Secure-1PSID", "SID", "LOGIN_INFO"}},
	"tiktok.com":      {"TikTok", []string{"sessionid", "sessionid_ss"}},
	"reddit.com":      {"Reddit", []string{"reddit_session", "token_v2"}},
	"facebook.com":    {"Facebook", []string{"c_user", "xs"}},
	"threads.net":     {"Threads", []string{"sessionid"}},
	"threads.com":     {"Threads", []string{"sessionid"}},
	"pixiv.net":       {"Pixiv", []string{"PHPSESSID"}},
	"deviantart.com":  {"DeviantArt", []string{"auth", "auth_secure"}},
	"vimeo.com":       {"Vimeo", []string{"vimeo"}},
	"twitch.tv":       {"Twitch", []string{"auth-token"}},
	"patreon.com":     {"Patreon", []string{"session_id"}},
	"soundcloud.com":  {"SoundCloud", []string{"oauth_token"}},
	"tumblr.com":      {"Tumblr", []string{"logged_in", "sid"}},
	"pinterest.com":   {"Pinterest", []string{"_pinterest_sess"}},
	"bilibili.com":    {"Bilibili", []string{"SESSDATA"}},
	"nicovideo.jp":    {"Niconico", []string{"user_session"}},
	"vk.com":          {"VK", []string{"remixsid"}},
	"fanbox.cc":       {"pixivFANBOX", []string{"FANBOXSESSID"}},
	"kemono.su":       {"Kemono", []string{"session"}},
	"newgrounds.com":  {"Newgrounds", []string{"vmk1du5I8m"}},
	"imgur.com":       {"Imgur", nil},
	"dailymotion.com": {"Dailymotion", []string{"sdx"}},
}

// SiteSummary describes the cookies of one site without exposing values.
type SiteSummary struct {
	Site     string    `json:"site"`
	Label    string    `json:"label,omitempty"`
	Known    bool      `json:"known"`
	Count    int       `json:"count"`
	HasLogin bool      `json:"has_login"`         // a known session cookie is present
	Expires  int64     `json:"expires"`           // login (or longest) cookie expiry, 0 = session
	Expired  bool      `json:"expired"`           // login cookie already expired
	Stored   bool      `json:"stored"`            // a file for this site already exists
	Updated  time.Time `json:"updated,omitzero"`  // stored file mtime
	Domains  []string  `json:"domains,omitempty"` // distinct cookie domains in the group
}

func summarize(site string, cs []cookie, now time.Time) SiteSummary {
	ks, known := knownSites[site]
	s := SiteSummary{Site: site, Known: known, Label: ks.Label, Count: len(cs)}
	doms := map[string]bool{}
	auth := map[string]bool{}
	for _, a := range ks.Auth {
		auth[a] = true
	}
	// The login lasts as long as its shortest-lived session cookie; a
	// browser-session cookie (expiry 0) ends when the browser closes.
	var longest, loginExp int64
	loginFound, loginSession := false, false
	for _, c := range cs {
		doms[strings.TrimPrefix(c.Domain, ".")] = true
		if c.Expires > longest {
			longest = c.Expires
		}
		if !auth[c.Name] {
			continue
		}
		loginFound = true
		if c.Expires == 0 {
			loginSession = true
		} else if loginExp == 0 || c.Expires < loginExp {
			loginExp = c.Expires
		}
	}
	s.Domains = slices.Sorted(maps.Keys(doms))
	s.HasLogin = loginFound
	switch {
	case loginFound && loginSession:
		s.Expires = 0
	case loginFound:
		s.Expires = loginExp
		s.Expired = loginExp < now.Unix()
	default:
		s.Expires = longest
		s.Expired = longest > 0 && longest < now.Unix()
	}
	return s
}

func groupBySite(cs []cookie) map[string][]cookie {
	g := map[string][]cookie{}
	for _, c := range cs {
		site := siteOf(c.Domain)
		g[site] = append(g[site], c)
	}
	return g
}

func sortSummaries(list []SiteSummary) {
	slices.SortFunc(list, func(a, b SiteSummary) int {
		if a.Known != b.Known {
			if a.Known {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Site, b.Site)
	})
}

// ---- store operations -----------------------------------------------------

type CookieAnalysis struct {
	Format string        `json:"format"`
	Total  int           `json:"total"`
	Sites  []SiteSummary `json:"sites"`
}

// Analyze parses an export and reports its sites; nothing is written.
func (c *CookieStore) Analyze(data []byte) (*CookieAnalysis, error) {
	cs, format, err := parseCookies(data)
	if err != nil {
		return nil, err
	}
	stored := map[string]bool{}
	for _, s := range c.sites() {
		stored[s] = true
	}
	now := time.Now()
	a := &CookieAnalysis{Format: format, Total: len(cs)}
	for site, group := range groupBySite(cs) {
		s := summarize(site, group, now)
		s.Stored = stored[site]
		a.Sites = append(a.Sites, s)
	}
	sortSummaries(a.Sites)
	return a, nil
}

// Import writes the cookies of the selected sites, replacing what was stored
// for those sites. Sites not selected are ignored and never touch the disk.
func (c *CookieStore) Import(data []byte, sites []string) ([]SiteSummary, error) {
	if len(sites) == 0 {
		return nil, errors.New("en az bir site seçmelisin")
	}
	cs, _, err := parseCookies(data)
	if err != nil {
		return nil, err
	}
	groups := groupBySite(cs)
	want := map[string]bool{}
	for _, s := range sites {
		h, err := normCookieHost(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s, err)
		}
		site := siteOf(h)
		if _, ok := groups[site]; !ok {
			return nil, fmt.Errorf("dosyada %s çerezi yok", site)
		}
		want[site] = true
	}
	now := time.Now()
	var done []SiteSummary
	for site := range want {
		var b strings.Builder
		b.WriteString("# Netscape HTTP Cookie File\n# İndirgen: " + site + " · " + now.Format(time.RFC3339) + "\n\n")
		for _, ck := range groups[site] {
			b.WriteString(ck.netscapeLine())
			b.WriteByte('\n')
		}
		if err := writeFileAtomic(c.path(site), []byte(b.String()), 0o600); err != nil {
			return done, err
		}
		s := summarize(site, groups[site], now)
		s.Stored, s.Updated = true, now
		done = append(done, s)
	}
	sortSummaries(done)
	return done, nil
}

func (c *CookieStore) path(site string) string { return filepath.Join(c.dir, site+".txt") }

func (c *CookieStore) sites() []string {
	entries, _ := os.ReadDir(c.dir)
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".txt"); ok && !e.IsDir() {
			if _, err := normCookieHost(n); err == nil {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}

// List reports every stored site with its login status.
func (c *CookieStore) List() []SiteSummary {
	now := time.Now()
	out := []SiteSummary{}
	for _, site := range c.sites() {
		p := c.path(site)
		raw, err := os.ReadFile(p)
		s := SiteSummary{Site: site, Stored: true}
		if err == nil {
			if cs, _, perr := parseCookies(raw); perr == nil {
				s = summarize(site, cs, now)
				s.Count, s.Stored = len(cs), true
			}
		}
		if st, err := os.Stat(p); err == nil {
			s.Updated = st.ModTime()
		}
		out = append(out, s)
	}
	sortSummaries(out)
	return out
}

func (c *CookieStore) Delete(host string) error {
	h, err := normCookieHost(host)
	if err != nil {
		return err
	}
	return os.Remove(c.path(h))
}

// For returns the cookie file applying to host, the most specific one first.
func (c *CookieStore) For(host string) string {
	if c == nil {
		return ""
	}
	best := ""
	for _, h := range c.sites() {
		if (host == h || strings.HasSuffix(host, "."+h)) && len(h) > len(best) {
			best = h
		}
	}
	if best == "" {
		return ""
	}
	return c.path(best)
}
