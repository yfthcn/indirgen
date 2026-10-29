package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path"
	"slices"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Router: decides whether yt-dlp or gallery-dl should handle a URL.
//
//   galleryFirst hosts → gallery-dl, yt-dlp as fallback
//   mixed hosts        → probe both in parallel; single video wins for
//                        yt-dlp (better format selection/merging), multi-item
//                        posts and image posts go to gallery-dl
//   everything else    → yt-dlp, gallery-dl as fallback
// ---------------------------------------------------------------------------

const (
	EngYTDLP     = "yt-dlp"
	EngGalleryDL = "gallery-dl"
	probeLimit   = 60 // items counted while probing a gallery (keeps request count low)
)

var galleryFirstHosts = []string{
	"pixiv.net", "deviantart.com", "imgur.com", "artstation.com", "flickr.com",
	"danbooru.donmai.us", "gelbooru.com", "safebooru.org", "e621.net", "behance.net",
	"wallhaven.cc", "imgbox.com", "furaffinity.net", "500px.com", "unsplash.com",
	"mangadex.org", "kemono.su", "kemono.cr", "zerochan.net", "catbox.moe",
	"imgchest.com", "postimg.cc", "imagebam.com", "bunkr.si", "cyberdrop.me",
}

var mixedHosts = []string{
	"x.com", "twitter.com", "instagram.com", "reddit.com", "redd.it", "tumblr.com",
	"bsky.app", "pinterest.com", "pin.it", "newgrounds.com", "weibo.com", "weibo.cn",
	"vk.com", "facebook.com", "tiktok.com", "threads.net", "threads.com",
	"mastodon.social", "misskey.io", "patreon.com", "fanbox.cc",
}

func hostMatch(host string, list []string) bool {
	for _, d := range list {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// ValidateURL accepts only absolute http(s) URLs. It also rejects anything
// that could be parsed as a command line flag (the CLI calls also put "--"
// before the URL, this is defence in depth).
func ValidateURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "-") || len(raw) > 4096 {
		return nil, errors.New("geçersiz URL")
	}
	if strings.ContainsAny(raw, "\r\n\x00 \t") {
		return nil, errors.New("URL boşluk/kontrol karakteri içeremez")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("sadece http/https bağlantıları kabul edilir")
	}
	return u, nil
}

func normHost(u *url.URL) string {
	h := strings.ToLower(u.Hostname())
	for _, p := range []string{"www.", "m.", "mobile."} {
		h = strings.TrimPrefix(h, p)
	}
	return h
}

type Format struct {
	Height int    `json:"height"`
	Label  string `json:"label"`
}

type ProbeResult struct {
	URL         string        `json:"url"`
	Host        string        `json:"host"`
	Engine      string        `json:"engine"`
	Title       string        `json:"title"`
	Uploader    string        `json:"uploader,omitempty"`
	Thumbnail   string        `json:"thumbnail,omitempty"`
	Duration    float64       `json:"duration,omitempty"`
	Extractor   string        `json:"extractor,omitempty"`
	Formats     []Format      `json:"formats,omitempty"`
	AudioOnly   bool          `json:"audio_only,omitempty"`
	IsPlaylist  bool          `json:"is_playlist,omitempty"`
	CanPlaylist bool          `json:"can_playlist,omitempty"`
	Count       int           `json:"count"`
	CountCapped bool          `json:"count_capped,omitempty"`
	Kind        string        `json:"kind"` // video | audio | playlist | gallery
	Alt         string        `json:"alt,omitempty"`
	Profile     *ProfileInfo  `json:"profile,omitempty"`
	Existing    *ExistingView `json:"existing,omitempty"` // downloaded before, files still on disk
}

type Router struct {
	eng     *EngineManager
	cookies *CookieStore
}

func (r *Router) Probe(ctx context.Context, raw, forced string) (*ProbeResult, error) {
	res, err := r.probe(ctx, raw, forced)
	if err != nil {
		if u, uerr := ValidateURL(raw); uerr == nil {
			return nil, withLoginHint(normHost(u), err)
		}
	}
	return res, err
}

func (r *Router) probe(ctx context.Context, raw, forced string) (*ProbeResult, error) {
	u, err := ValidateURL(raw)
	if err != nil {
		return nil, err
	}
	host := normHost(u)
	clean := u.String()

	// Instagram profiles are recognised locally: no request until the user
	// browses or downloads.
	if forced != EngYTDLP {
		if res, ok := r.instagramProfile(u); ok {
			return res, nil
		}
	}

	switch forced {
	case EngYTDLP:
		return r.probeYT(ctx, clean, host)
	case EngGalleryDL:
		return r.probeGD(ctx, clean, host)
	}

	switch {
	case hostMatch(host, galleryFirstHosts) || imageExts[urlExt(clean)]:
		res, err := r.probeGD(ctx, clean, host)
		if err == nil {
			res.Alt = EngYTDLP
			return res, nil
		}
		if res2, err2 := r.probeYT(ctx, clean, host); err2 == nil {
			res2.Alt = EngGalleryDL
			return res2, nil
		}
		return nil, err

	case hostMatch(host, mixedHosts):
		type out struct {
			r   *ProbeResult
			err error
		}
		ych, gch := make(chan out, 1), make(chan out, 1)
		go func() { r1, e := r.probeYT(ctx, clean, host); ych <- out{r1, e} }()
		go func() { r2, e := r.probeGD(ctx, clean, host); gch <- out{r2, e} }()
		y, g := <-ych, <-gch
		switch {
		case y.err == nil && (g.err != nil || g.r.Count <= max(1, y.r.Count)):
			if g.err == nil {
				y.r.Alt = EngGalleryDL
			}
			return y.r, nil
		case g.err == nil:
			if y.err == nil {
				g.r.Alt = EngYTDLP
			}
			return g.r, nil
		}
		return nil, pickError(y.err, g.err)

	default:
		res, err := r.probeYT(ctx, clean, host)
		if err == nil {
			return res, nil
		}
		// gallery-dl's "directlink" extractor accepts any file-looking URL
		// without fetching it; that is not a real match for a URL yt-dlp
		// already failed on (e.g. a 404 .mp4), so keep yt-dlp's error.
		res2, err2 := r.probeGD(ctx, clean, host)
		if err2 == nil && res2.Extractor != "directlink" {
			return res2, nil
		}
		if err2 != nil {
			return nil, pickError(err, err2)
		}
		return nil, err
	}
}

// pickError chooses the more useful message when both engines fail. If
// yt-dlp only says "there is no video here", the content is images and
// gallery-dl's reason (typically "login required") is the real one.
func pickError(ytErr, gdErr error) error {
	if gdErr == nil {
		return ytErr
	}
	if fallbackEngine(EngYTDLP, ytErr.Error()) == EngGalleryDL ||
		!strings.Contains(strings.ToLower(gdErr.Error()), "no suitable extractor") && isLoginError(gdErr) {
		return gdErr
	}
	return ytErr
}

func (r *Router) run(ctx context.Context, timeout time.Duration, bin string, args ...string) ([]byte, string, error) {
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx2, bin, args...)
	cmd.Env = r.eng.Env()
	setProcAttrs(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx2.Err(), context.DeadlineExceeded) {
		return nil, "", errors.New("sorgu zaman aşımına uğradı")
	}
	return stdout.Bytes(), stderr.String(), err
}

func (r *Router) probeYT(ctx context.Context, link, host string) (*ProbeResult, error) {
	bin, err := r.eng.Need(EngYTDLP)
	if err != nil {
		return nil, err
	}
	args := []string{"--ignore-config", "--no-warnings", "-J", "--flat-playlist", "--no-playlist"}
	ck, done := r.cookies.Use(host)
	defer done()
	args = append(args, ck.YTDLP()...)
	args = append(args, "--", link)
	stdout, stderr, err := r.run(ctx, 90*time.Second, bin, args...)
	if err != nil {
		return nil, engineError(stderr, err)
	}
	var info map[string]any
	if err := json.Unmarshal(firstJSON(stdout), &info); err != nil {
		return nil, fmt.Errorf("yt-dlp çıktısı okunamadı: %v", err)
	}
	res := &ProbeResult{URL: link, Host: host, Engine: EngYTDLP, Count: 1}
	res.Title = str(info, "title")
	res.Uploader = firstStr(info, "uploader", "channel", "creator", "uploader_id")
	res.Thumbnail = str(info, "thumbnail")
	res.Duration = num(info, "duration")
	res.Extractor = firstStr(info, "extractor_key", "extractor")
	u, _ := url.Parse(link)
	res.CanPlaylist = u != nil && u.Query().Get("list") != ""

	if t, _ := info["_type"].(string); t == "playlist" || t == "multi_video" {
		entries, _ := info["entries"].([]any)
		res.IsPlaylist, res.Kind, res.Count = true, "playlist", len(entries)
		if pc := num(info, "playlist_count"); int(pc) > res.Count {
			res.Count = int(pc)
		}
		if res.Thumbnail == "" {
			res.Thumbnail = thumbFrom(info)
			for _, e := range entries {
				if em, ok := e.(map[string]any); ok && res.Thumbnail == "" {
					res.Thumbnail = thumbFrom(em)
				}
			}
		}
		return res, nil
	}

	heights := map[int]bool{}
	hasVideo := false
	formats := anySlice(info["formats"])
	if len(formats) == 0 {
		formats = []any{info} // single-format result: top-level fields describe it
	}
	for _, f := range formats {
		fm, _ := f.(map[string]any)
		if fm == nil || !formatHasVideo(fm) {
			continue
		}
		hasVideo = true
		if h := int(num(fm, "height")); h > 0 {
			heights[h] = true
		}
	}
	for h := range heights {
		res.Formats = append(res.Formats, Format{Height: h, Label: fmt.Sprintf("%dp", h)})
	}
	slices.SortFunc(res.Formats, func(a, b Format) int { return b.Height - a.Height })
	res.Thumbnail = cmp.Or(res.Thumbnail, thumbFrom(info))
	res.Kind = "video"
	if !hasVideo {
		res.Kind, res.AudioOnly = "audio", true
	}
	res.Title = cmp.Or(res.Title, link)
	return res, nil
}

func (r *Router) probeGD(ctx context.Context, link, host string) (*ProbeResult, error) {
	bin, err := r.eng.Need(EngGalleryDL)
	if err != nil {
		return nil, err
	}
	args := []string{"--config-ignore", "-j", "--range", fmt.Sprintf("1-%d", probeLimit)}
	ck, done := r.cookies.Use(host)
	defer done()
	args = append(args, ck.GalleryDL()...)
	args = append(args, "--", link)
	stdout, stderr, err := r.run(ctx, 120*time.Second, bin, args...)
	if err != nil && len(bytes.TrimSpace(stdout)) == 0 {
		return nil, engineError(stderr, err)
	}
	msgs, err := parseGDMessages(stdout)
	if err != nil {
		return nil, engineError(stderr, fmt.Errorf("gallery-dl çıktısı okunamadı: %v", err))
	}
	res := &ProbeResult{URL: link, Host: host, Engine: EngGalleryDL, Kind: "gallery"}
	var meta map[string]any
	gdErr := ""
	for _, m := range msgs {
		switch m.Code {
		case gdDirectory:
			if meta == nil && len(m.Args) > 0 {
				_ = json.Unmarshal(m.Args[0], &meta)
			}
		case gdError: // DataJob reports extractor errors inside the JSON, exit code 0
			if e := m.errorText(); e != "" {
				gdErr = e
			}
		case gdURL, gdQueue:
			res.Count++
			var u string
			var kw map[string]any
			if len(m.Args) > 0 {
				_ = json.Unmarshal(m.Args[0], &u)
			}
			if len(m.Args) > 1 {
				_ = json.Unmarshal(m.Args[1], &kw)
			}
			if meta == nil {
				meta = kw
			}
			if m.Code == gdURL && res.Thumbnail == "" && isImageURL(u, kw) {
				res.Thumbnail = u
			}
		}
	}
	if res.Count == 0 {
		if gdErr != "" {
			return nil, errors.New("[gallery-dl] " + gdErr)
		}
		if msg := engineError(stderr, nil); msg != nil {
			return nil, msg
		}
		return nil, errors.New("gallery-dl indirilecek bir şey bulamadı")
	}
	res.CountCapped = res.Count >= probeLimit
	if meta != nil {
		res.Extractor = str(meta, "category")
		res.Title = firstStr(meta, "title", "description", "content", "caption", "name")
		res.Uploader = firstStr(meta, "author", "user", "username", "uploader", "artist", "owner")
	}
	res.Title = clip(strings.Join(strings.Fields(res.Title), " "), 160)
	res.Title = cmp.Or(res.Title, host+" · "+strings.Trim(urlPath(link), "/"))
	return res, nil
}

// ---- helpers --------------------------------------------------------------

// gallery-dl -j prints a JSON array of messages: [code, args...].
const (
	gdError     = -1 // ({"error", "message"})
	gdDirectory = 2  // (kwdict)
	gdURL       = 3  // (url, kwdict)
	gdQueue     = 6  // (url, kwdict)
)

type gdMessage struct {
	Code int
	Args []json.RawMessage
}

func parseGDMessages(stdout []byte) ([]gdMessage, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(firstJSONArray(stdout), &raw); err != nil {
		return nil, err
	}
	out := make([]gdMessage, 0, len(raw))
	for _, m := range raw {
		var parts []json.RawMessage
		if json.Unmarshal(m, &parts) != nil || len(parts) == 0 {
			continue
		}
		var code int
		_ = json.Unmarshal(parts[0], &code)
		out = append(out, gdMessage{Code: code, Args: parts[1:]})
	}
	return out, nil
}

// errorText formats the payload of a gdError message ("" if unreadable).
func (m gdMessage) errorText() string {
	var e map[string]any
	if len(m.Args) == 0 || json.Unmarshal(m.Args[0], &e) != nil {
		return ""
	}
	return strings.TrimSpace(str(e, "error") + ": " + strings.TrimSpace(str(e, "message")))
}

func engineError(stderr string, err error) error {
	var last string
	sc := bufio.NewScanner(strings.NewReader(stderr))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "ERROR:") || strings.Contains(l, "[error]") {
			last = l
		} else if last == "" || !strings.HasPrefix(last, "ERROR:") {
			if !strings.HasPrefix(l, "WARNING:") && !strings.Contains(l, "[warning]") {
				last = l
			}
		}
	}
	if last != "" {
		return errors.New(clip(last, 400))
	}
	return err
}

func firstJSON(b []byte) []byte {
	for l := range bytes.SplitSeq(b, []byte("\n")) {
		l = bytes.TrimSpace(l)
		if len(l) > 0 && l[0] == '{' {
			return l
		}
	}
	return b
}

func firstJSONArray(b []byte) []byte {
	if i := bytes.IndexByte(b, '['); i >= 0 {
		return b[i:]
	}
	return b
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case map[string]any: // e.g. gallery-dl "author": {"name": ...}
		return firstStr(v, "name", "nick", "username", "screen_name", "display_name")
	}
	return ""
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := strings.TrimSpace(str(m, k)); s != "" {
			return s
		}
	}
	return ""
}

func num(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func anySlice(v any) []any { s, _ := v.([]any); return s }

func thumbFrom(m map[string]any) string {
	if t := str(m, "thumbnail"); t != "" {
		return t
	}
	th := anySlice(m["thumbnails"])
	for _, t := range slices.Backward(th) {
		if tm, ok := t.(map[string]any); ok {
			if u := str(tm, "url"); u != "" {
				return u
			}
		}
	}
	return ""
}

var audioExts = map[string]bool{"mp3": true, "m4a": true, "opus": true, "ogg": true, "oga": true,
	"wav": true, "flac": true, "aac": true, "weba": true, "mka": true}

var imageExts = map[string]bool{"jpg": true, "jpeg": true, "png": true, "webp": true, "gif": true,
	"avif": true, "bmp": true, "tif": true, "tiff": true, "heic": true, "jxl": true}

// formatHasVideo: vcodec "none" means audio-only; a missing vcodec (direct
// links, some extractors) means "unknown", so we fall back to the extension.
func formatHasVideo(fm map[string]any) bool {
	vc, known := fm["vcodec"].(string)
	if known && vc != "" {
		return vc != "none"
	}
	if h := num(fm, "height"); h > 0 {
		return true
	}
	return !audioExts[strings.ToLower(str(fm, "ext"))]
}

func urlExt(raw string) string {
	return strings.TrimPrefix(strings.ToLower(path.Ext(urlPath(raw))), ".")
}

func isImageURL(u string, kw map[string]any) bool {
	ext := strings.ToLower(str(kw, "extension"))
	ext = cmp.Or(ext, urlExt(u))
	switch ext {
	case "jpg", "jpeg", "png", "webp", "gif", "avif":
		return strings.HasPrefix(u, "http")
	}
	return false
}

func urlPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
