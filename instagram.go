package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Instagram profiles: sections, browsing and selection (all via gallery-dl).
//
// A profile URL is recognised locally, without any request. Instagram is
// only contacted when the user browses a section or starts a download.
// ---------------------------------------------------------------------------

var igSections = []string{"posts", "reels", "tagged", "stories", "highlights", "avatar", "saved"}

// Sections that list individual posts and can therefore be browsed/selected.
var igBrowsable = map[string]bool{"posts": true, "reels": true, "tagged": true, "saved": true}

// igBrowsableList is igBrowsable in igSections order (for the UI).
var igBrowsableList = func() []string {
	var out []string
	for _, s := range igSections {
		if igBrowsable[s] {
			out = append(out, s)
		}
	}
	return out
}()

var igUserRe = regexp.MustCompile(`^[A-Za-z0-9._]{1,30}$`)

var igReserved = map[string]bool{
	"p": true, "tv": true, "reel": true, "reels": true, "explore": true, "stories": true,
	"accounts": true, "direct": true, "about": true, "developer": true, "legal": true,
	"web": true, "api": true, "graphql": true, "s": true, "challenge": true, "emails": true,
	"session": true, "privacy": true, "terms": true, "ar": true, "static": true, "topics": true,
	"locations": true, "lite": true, "oauth": true, "nametag": true, "directory": true,
}

type ProfileInfo struct {
	Site       string   `json:"site"`
	User       string   `json:"user"`
	Sections   []string `json:"sections"`
	Browsable  []string `json:"browsable"`
	Preselect  []string `json:"preselect"`
	HasCookies bool     `json:"has_cookies"`
}

func isInstagramHost(host string) bool {
	return host == "instagram.com" || strings.HasSuffix(host, ".instagram.com")
}

// parseInstagramProfile recognises profile and profile-section URLs:
// /USER/, /USER/{posts,reels,tagged,saved,highlights,avatar}/, /stories/USER/
func parseInstagramProfile(u *url.URL) (user, section string, ok bool) {
	if !isInstagramHost(normHost(u)) {
		return "", "", false
	}
	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	switch {
	case len(segs) == 2 && segs[0] == "stories" && segs[1] != "highlights" && segs[1] != "me":
		user, section = segs[1], "stories"
	case len(segs) == 1 && !igReserved[strings.ToLower(segs[0])]:
		user = segs[0]
	case len(segs) == 2 && !igReserved[strings.ToLower(segs[0])]:
		if sec := strings.ToLower(segs[1]); sec != "stories" && slices.Contains(igSections, sec) {
			user, section = segs[0], sec
		}
	}
	if user == "" || !igUserRe.MatchString(user) {
		return "", "", false
	}
	return user, section, true
}

func igSectionURL(user, section string) string {
	if section == "stories" {
		return "https://www.instagram.com/stories/" + user + "/"
	}
	return "https://www.instagram.com/" + user + "/" + section + "/"
}

func (r *Router) instagramProfile(u *url.URL) (*ProbeResult, bool) {
	user, section, ok := parseInstagramProfile(u)
	if !ok {
		return nil, false
	}
	pre := []string{"posts"}
	if section != "" {
		pre = []string{section}
	}
	host := normHost(u)
	return &ProbeResult{
		URL: "https://www.instagram.com/" + user + "/", Host: host, Engine: EngGalleryDL,
		Title: "@" + user, Uploader: "Instagram", Extractor: "instagram", Kind: "profile",
		Profile: &ProfileInfo{Site: "instagram", User: user, Sections: igSections,
			Browsable: igBrowsableList, Preselect: pre, HasCookies: r.cookies.Has(host)},
	}, true
}

// ---- browse ---------------------------------------------------------------

type BrowseItem struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Thumb   string `json:"thumb,omitempty"`
	Type    string `json:"type"` // image | video | carousel
	Count   int    `json:"count"`
	Date    string `json:"date,omitempty"`
	Caption string `json:"caption,omitempty"`
	Likes   int    `json:"likes,omitempty"`
}

type BrowseResult struct {
	User    string       `json:"user"`
	Section string       `json:"section"`
	Start   int          `json:"start"`
	Items   []BrowseItem `json:"items"`
	More    bool         `json:"more"`
}

type browseCache struct {
	mu   sync.Mutex
	data map[string]browseEntry
	run  sync.Mutex // one Instagram listing at a time
}

type browseEntry struct {
	res *BrowseResult
	at  time.Time
}

var igBrowse = &browseCache{data: map[string]browseEntry{}}

// Browse lists posts [start, start+count) of one profile section.
func (r *Router) Browse(ctx context.Context, rawURL, section string, start, count int, pace Pacing) (*BrowseResult, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	user, _, ok := parseInstagramProfile(u)
	if !ok {
		return nil, errors.New("Instagram profil bağlantısı değil")
	}
	if !igBrowsable[section] {
		return nil, errors.New("bu bölüm listelenemez, doğrudan indirilebilir")
	}
	start = max(start, 1)
	if count < 1 || count > 200 {
		count = 48
	}
	key := fmt.Sprintf("%s|%s|%d|%d", strings.ToLower(user), section, start, count)
	igBrowse.mu.Lock()
	if e, ok := igBrowse.data[key]; ok && time.Since(e.at) < 20*time.Minute {
		igBrowse.mu.Unlock()
		return e.res, nil
	}
	igBrowse.mu.Unlock()

	bin, err := r.eng.Need(EngGalleryDL)
	if err != nil {
		return nil, err
	}
	igBrowse.run.Lock()
	defer igBrowse.run.Unlock()

	host := normHost(u)
	args := []string{"--config-ignore", "-j", "--post-range", fmt.Sprintf("%d-%d", start, start+count-1)}
	if pace.Request != "" {
		args = append(args, "--sleep-request", pace.Request)
	}
	ck, done := r.cookies.Use(host)
	defer done()
	args = append(args, ck.GalleryDL()...)
	args = append(args, "--", igSectionURL(user, section))

	stdout, stderr, err := r.run(ctx, 10*time.Minute, bin, args...)
	if err != nil && len(stdout) == 0 {
		return nil, withLoginHint(host, engineError(stderr, err))
	}
	items, gdErr := parseBrowse(stdout)
	if len(items) == 0 && gdErr != "" {
		return nil, withLoginHint(host, errors.New("[gallery-dl] "+gdErr))
	}
	res := &BrowseResult{User: user, Section: section, Start: start, Items: items,
		More: len(items) >= count && gdErr == ""}
	igBrowse.mu.Lock()
	igBrowse.data[key] = browseEntry{res, time.Now()}
	for k, e := range igBrowse.data {
		if time.Since(e.at) > 20*time.Minute {
			delete(igBrowse.data, k)
		}
	}
	igBrowse.mu.Unlock()
	return res, nil
}

// parseBrowse groups gallery-dl -j output into posts: a gdDirectory message
// starts a post, gdURL messages are its media files.
func parseBrowse(stdout []byte) ([]BrowseItem, string) {
	msgs, err := parseGDMessages(stdout)
	if err != nil {
		return nil, "gallery-dl çıktısı okunamadı: " + err.Error()
	}
	var items []BrowseItem
	var gdErr string
	var cur *BrowseItem
	videos := 0
	files := 0
	flush := func() {
		if cur == nil {
			return
		}
		if cur.Count == 0 {
			cur.Count = files // no "count" in the metadata: count the media seen
		}
		switch {
		case cur.Count > 1:
			cur.Type = "carousel"
		case videos > 0:
			cur.Type = "video"
		default:
			cur.Type = "image"
		}
		if cur.URL != "" {
			items = append(items, *cur)
		}
		cur, videos, files = nil, 0, 0
	}
	for _, m := range msgs {
		if len(m.Args) == 0 {
			continue
		}
		switch m.Code {
		case gdError:
			if e := m.errorText(); e != "" {
				gdErr = e
			}
		case gdDirectory:
			flush()
			var kw map[string]any
			if json.Unmarshal(m.Args[len(m.Args)-1], &kw) != nil {
				continue
			}
			cur = &BrowseItem{
				ID:      firstStr(kw, "post_shortcode", "shortcode"),
				URL:     str(kw, "post_url"),
				Date:    firstStr(kw, "post_date", "date"),
				Caption: clip(strings.Join(strings.Fields(str(kw, "description")), " "), 200),
				Likes:   int(num(kw, "likes")),
				Count:   int(num(kw, "count")),
			}
			if cur.URL == "" && cur.ID != "" {
				cur.URL = "https://www.instagram.com/p/" + cur.ID + "/"
			}
		case gdURL:
			if cur == nil || len(m.Args) < 2 {
				continue
			}
			files++
			var kw map[string]any
			_ = json.Unmarshal(m.Args[1], &kw)
			if v := str(kw, "video_url"); v != "" {
				videos++
			}
			cur.Thumb = cmp.Or(cur.Thumb, str(kw, "display_url"))
		}
	}
	flush()
	return items, gdErr
}

// validIGItems validates a user selection: only Instagram post/reel URLs.
var igPostRe = regexp.MustCompile(`^https://www\.instagram\.com/(p|reel|tv)/[A-Za-z0-9_-]{5,40}/$`)

func validIGItems(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		s = strings.TrimSpace(s)
		if igPostRe.MatchString(s) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
		if len(out) >= 1000 {
			break
		}
	}
	return out
}

// validSections keeps the known sections of list, in igSections order.
func validSections(list []string) []string {
	var out []string
	for _, s := range igSections {
		if slices.Contains(list, s) {
			out = append(out, s)
		}
	}
	return out
}
