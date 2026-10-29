package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// "Already downloaded?" for links sent to the Telegram bot.
//
// Only links to one video or post are checked; a profile, channel or
// playlist always runs, because it may have new items. The same item has
// many URLs (youtu.be/X, youtube.com/watch?v=X&si=…, /shorts/X), so links
// are reduced to site + item ID first.
// ---------------------------------------------------------------------------

var (
	ytIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	igPathRe  = regexp.MustCompile(`^/(?:[A-Za-z0-9._]+/)?(?:p|reel|reels|tv)/([A-Za-z0-9_-]{5,40})/?$`)
	ttPathRe  = regexp.MustCompile(`^/(?:(?:@[^/]+/)?(?:video|photo)|v)/(\d{8,25})(?:\.html)?/?$`)
	xPathRe   = regexp.MustCompile(`^/(?:[A-Za-z0-9_]{1,15}|i/web|i)/status/(\d{5,25})(?:/(?:photo|video)/\d)?/?$`)
	rdPathRe  = regexp.MustCompile(`^/r/[A-Za-z0-9_]+/comments/([a-z0-9]{4,12})(?:/|$)`)
	vmPathRe  = regexp.MustCompile(`^/(\d{6,12})/?$`)
	dmPathRe  = regexp.MustCompile(`^/video/([A-Za-z0-9]{5,12})/?$`)
	ytPathRes = []*regexp.Regexp{
		regexp.MustCompile(`^/(?:shorts|live|embed|v)/([A-Za-z0-9_-]{11})/?$`),
	}
)

// mediaKey returns a site-wide identity for a single video/post link
// ("youtube:dQw4w9WgXcQ") and the item ID as yt-dlp names files with it.
// ok is false for anything else (profiles, channels, playlists, unknown).
func mediaKey(raw string) (key, id string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	host := normHost(u)
	p := u.Path
	match := func(re *regexp.Regexp) string {
		if m := re.FindStringSubmatch(p); m != nil {
			return m[1]
		}
		return ""
	}
	site := ""
	switch {
	case host == "youtu.be":
		site, id = "youtube", strings.Trim(p, "/")
	case hostMatch(host, []string{"youtube.com", "youtube-nocookie.com"}):
		site = "youtube"
		if p == "/watch" {
			id = u.Query().Get("v")
		}
		for _, re := range ytPathRes {
			if id == "" {
				id = match(re)
			}
		}
		if id != "" && !ytIDRe.MatchString(id) {
			id = ""
		}
	case hostMatch(host, []string{"instagram.com"}):
		site, id = "instagram", match(igPathRe)
	case hostMatch(host, []string{"tiktok.com"}):
		site, id = "tiktok", match(ttPathRe)
	case hostMatch(host, []string{"x.com", "twitter.com"}):
		site, id = "x", match(xPathRe)
	case hostMatch(host, []string{"reddit.com"}):
		site, id = "reddit", match(rdPathRe)
	case hostMatch(host, []string{"vimeo.com"}):
		site, id = "vimeo", match(vmPathRe)
	case hostMatch(host, []string{"dailymotion.com"}):
		site, id = "dailymotion", match(dmPathRe)
	}
	if id == "" {
		return "", "", false
	}
	return site + ":" + id, id, true
}

// TikTok share links (vm.tiktok.com/ZM…, tiktok.com/t/…) carry no video
// ID; it is in the redirect. resolveShortLink reads that Location header
// (without following it) so "already downloaded" works for them too. Only
// these hosts are contacted; any other link is returned untouched.
const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

var (
	shortLinkClient = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	shortLinkTimeout = 6 * time.Second // all redirects together
)

func isShortLink(u *url.URL) bool {
	switch h := strings.ToLower(u.Hostname()); h {
	case "vm.tiktok.com", "vt.tiktok.com":
		return true
	case "tiktok.com", "www.tiktok.com", "m.tiktok.com":
		return strings.HasPrefix(u.Path, "/t/")
	}
	return false
}

// resolveShortLink returns the full link behind a known share link: at most
// 3 redirects, 6 seconds in all. On any failure, or when the target is not a
// single video/post, the original link is returned.
func resolveShortLink(ctx context.Context, link string) string {
	cur, err := url.Parse(link)
	if err != nil || !isShortLink(cur) {
		return link
	}
	ctx, cancel := context.WithTimeout(ctx, shortLinkTimeout)
	defer cancel()
	for range 3 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.String(), nil)
		if err != nil {
			return link
		}
		req.Header.Set("User-Agent", browserUA)
		resp, err := shortLinkClient.Do(req)
		if err != nil {
			return link
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
			return link
		}
		if cur, err = cur.Parse(loc); err != nil {
			return link
		}
		if !isShortLink(cur) {
			if v, err := ValidateURL(cur.String()); err == nil {
				if _, _, ok := mediaKey(v.String()); ok {
					return v.String()
				}
			}
			return link
		}
	}
	return link
}

// Existing describes an earlier download of the same item.
type Existing struct {
	Active  bool // still queued or downloading
	Title   string
	Files   []string  // absolute paths, all present on disk
	When    time.Time // when it finished (file time for archive matches)
	JobID   string    // the job it came from ("" = found in the archive only)
	FileIdx []int     // index of each file in that job's Files (for /api/jobs/{id}/file/{n})
}

// FindExisting looks for the item behind link: first in the download
// history, then (for downloads whose record was cleared) in the archive by
// the "[ID]" yt-dlp puts in file names. Only files still on disk count.
func FindExisting(q *Queue, lib *Library, link string) (Existing, bool) {
	key, id, ok := mediaKey(link)
	if !ok {
		return Existing{}, false
	}
	for _, j := range q.List() { // newest first
		if k, _, ok := mediaKey(j.URL); !ok || k != key {
			continue
		}
		switch {
		case j.Status == StQueued || j.Status == StRunning:
			return Existing{Active: true, Title: j.Title, JobID: j.ID}, true
		case j.Status == StDone && !j.Expired:
			full, _ := q.Get(j.ID) // JobView hides full paths
			ex := Existing{Title: j.Title, When: j.Finished, JobID: j.ID}
			for i, f := range full.Files {
				if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
					ex.Files, ex.FileIdx = append(ex.Files, f), append(ex.FileIdx, i)
				}
			}
			if len(ex.Files) > 0 {
				return ex, true
			}
		}
	}
	if lib == nil {
		return Existing{}, false
	}
	all, _ := lib.index()
	var ex Existing
	tag := "[" + id + "]"
	for _, f := range all {
		if strings.Contains(f.Name, tag) {
			ex.Files = append(ex.Files, filepath.Join(lib.dir, filepath.FromSlash(f.Path)))
			if f.Mod.After(ex.When) {
				ex.When = f.Mod
			}
			ex.Title = cmp.Or(ex.Title, strings.TrimSpace(strings.TrimSuffix(f.Name, filepath.Ext(f.Name))))
		}
	}
	return ex, len(ex.Files) > 0
}

// ExistingView is what the web UI and /api/add show about an earlier
// download: where to get the files, without exposing full paths.
type ExistingView struct {
	Active bool           `json:"active,omitempty"`
	Title  string         `json:"title"`
	When   time.Time      `json:"when,omitzero"`
	Size   int64          `json:"size"`
	Files  []ExistingFile `json:"files"`
}

type ExistingFile struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	URL  string `json:"url"` // download link; add &inline=1 / ?inline=1 to view
}

func (ex Existing) View(lib *Library) ExistingView {
	v := ExistingView{Active: ex.Active, Title: ex.Title, When: ex.When, Files: []ExistingFile{}}
	for i, f := range ex.Files {
		if st, err := os.Stat(f); err == nil {
			v.Size += st.Size()
		}
		ef := ExistingFile{Name: filepath.Base(f), Kind: kindOf(f)}
		switch {
		case ex.JobID != "" && i < len(ex.FileIdx):
			ef.URL = fmt.Sprintf("/api/jobs/%s/file/%d", ex.JobID, ex.FileIdx[i])
		case lib != nil:
			if rel, err := filepath.Rel(lib.dir, f); err == nil {
				ef.URL = "/api/library/file?path=" + url.QueryEscape(filepath.ToSlash(rel))
			}
		}
		v.Files = append(v.Files, ef)
	}
	return v
}

// phoneReady: Photos (or Files) can take this file as it is.
func phoneReady(path string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "mp4", "m4v", "mov", "jpg", "jpeg", "png", "heic", "gif", "mp3", "m4a":
		return true
	}
	return false
}
