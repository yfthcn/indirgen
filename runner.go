package main

import (
	"cmp"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// JobOptions are the user-facing choices. Nothing here is passed through as a
// raw flag: every option maps to a fixed, whitelisted argument set.
type JobOptions struct {
	Preset   string `json:"preset"`   // best | compat | mp3 | audio
	Height   int    `json:"height"`   // 0 = best available
	Playlist bool   `json:"playlist"` // download the whole playlist
	Subs     bool   `json:"subs"`     // write + embed subtitles
	Sponsor  bool   `json:"sponsor"`  // SponsorBlock: cut sponsor segments
	Embed    bool   `json:"embed"`    // metadata, chapters, thumbnail
	Mode     string `json:"mode"`     // library | temp

	// gallery-dl profile jobs
	Sections []string `json:"sections,omitempty"` // profile sections to download (posts, reels, …)
	Items    []string `json:"items,omitempty"`    // hand-picked post URLs from "browse"
	NewOnly  bool     `json:"new_only,omitempty"` // skip anything downloaded before (archive)
	Night    bool     `json:"night,omitempty"`    // start only inside the night window
	Limit    int      `json:"limit,omitempty"`    // only the newest N items/posts (subscriptions)
	Mark     bool     `json:"mark,omitempty"`     // record the newest Limit in the archive, download nothing
}

func (o *JobOptions) normalize() {
	switch o.Preset {
	case "best", "compat", "mp3", "audio":
	default:
		o.Preset = "best"
	}
	if o.Height < 0 || o.Height > 8640 {
		o.Height = 0
	}
	if o.Mode != "temp" {
		o.Mode = "library"
	}
	o.Sections = validSections(o.Sections)
	o.Items = validIGItems(o.Items)
	o.Limit = min(max(o.Limit, 0), 500)
	o.Mark = o.Mark && o.NewOnly && o.Limit > 0 // marking writes the archive of the newest Limit
}

const (
	progPrefix = "SGR:P|"
	filePrefix = "SGR:F|"
	markPrefix = "SGR:M|" // an entry a marking run recorded in the archive
	dirPrefix  = "SGR:D|" // planned path, printed before yt-dlp creates its folder
	progTmpl   = "download:" + progPrefix +
		"%(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|" +
		"%(progress.total_bytes_estimate)s|%(progress.speed)s|%(progress.eta)s|" +
		"%(info.playlist_index)s|%(info.n_entries)s|%(info.title)s"
)

type ytdlpPaths struct {
	Home, Temp string
	FFmpegDir  string
	Cookies    CookieArg
	SubLangs   string
	Pace       Pacing
	Sensitive  bool   // bot-sensitive site (Instagram, X, TikTok…)
	Archive    string // download-archive path (used when o.NewOnly)
	RateLimit  string // yt-dlp --limit-rate ("2560K"), "" = none
	Flat       bool   // archive layout "flat": no Site/Channel folders
}

func ytdlpArgs(o JobOptions, p ytdlpPaths, link string) []string {
	if o.Mark {
		return ytdlpMarkArgs(o, p, link)
	}
	a := []string{
		"--ignore-config", "--color", "never", "--newline", "--progress",
		"--progress-template", progTmpl,
		"--print", "after_move:" + filePrefix + "%(filepath)s",
		"--print", "pre_process:" + dirPrefix + "%(filename)s",
		"--retries", "10", "--fragment-retries", "10", "--concurrent-fragments", "4",
		"--continue", "--no-mtime", "--windows-filenames",
		"-P", "home:" + p.Home, "-P", "temp:" + p.Temp,
	}
	if p.FFmpegDir != "" {
		a = append(a, "--ffmpeg-location", p.FFmpegDir)
	}
	a = append(a, p.Cookies.YTDLP()...)
	if p.RateLimit != "" {
		a = append(a, "--limit-rate", p.RateLimit)
	}

	a = append(a, "-o", outputTemplate(o, p, link))

	if o.Playlist {
		a = append(a, "--yes-playlist")
	} else {
		a = append(a, "--no-playlist")
	}
	// "Only new": yt-dlp checks the archive before extracting an entry, so
	// already downloaded videos cost no requests. --break-on-existing is not
	// used: it exits 101 and would stop a channel's other tabs (Shorts, Live).
	if o.NewOnly && p.Archive != "" {
		a = append(a, "--download-archive", p.Archive)
	}
	if o.Limit > 0 {
		a = append(a, "--playlist-items", fmt.Sprintf("1:%d", o.Limit))
	}

	audio := o.Preset == "mp3" || o.Preset == "audio"
	a = append(a, formatArgs(o)...)

	if o.Embed {
		a = append(a, "--embed-metadata", "--embed-thumbnail", "--convert-thumbnails", "jpg")
		if !audio {
			a = append(a, "--embed-chapters")
		}
	}
	if o.Subs && !audio {
		langs := cmp.Or(p.SubLangs, "tr.*,en.*")
		a = append(a, "--write-subs", "--sub-langs", langs+",-live_chat", "--embed-subs")
	}
	if o.Sponsor {
		a = append(a, "--sponsorblock-remove", "sponsor,selfpromo,interaction")
	}
	a = append(a, ytdlpPacing(o, p, true)...)
	return append(a, "--", link)
}

// ytdlpMarkArgs lists the newest o.Limit entries and records them in the
// download archive without downloading anything: --force-write-archive
// writes entries even under --skip-download. Each recorded entry is printed
// once; entries already in the archive are skipped before extraction.
func ytdlpMarkArgs(o JobOptions, p ytdlpPaths, link string) []string {
	a := []string{
		"--ignore-config", "--color", "never", "--newline", "--retries", "10",
		"--skip-download", "--force-write-archive", "--download-archive", p.Archive,
		"--print", "video:" + markPrefix + "%(id)s",
	}
	a = append(a, p.Cookies.YTDLP()...)
	if o.Playlist {
		a = append(a, "--yes-playlist")
	} else {
		a = append(a, "--no-playlist")
	}
	a = append(a, "--playlist-items", fmt.Sprintf("1:%d", o.Limit))
	a = append(a, ytdlpPacing(o, p, false)...)
	return append(a, "--", link)
}

// ytdlpPacing: only where it matters, on bot-sensitive sites and playlists.
// files adds the pause between downloads.
func ytdlpPacing(o JobOptions, p ytdlpPaths, files bool) []string {
	if !(p.Sensitive || o.Playlist) || p.Pace.Request == "" {
		return nil
	}
	lo, _ := durationBounds(p.Pace.Request)
	a := []string{"--sleep-requests", lo}
	if flo, fhi := durationBounds(p.Pace.File); files && fhi != "0" {
		a = append(a, "--sleep-interval", flo, "--max-sleep-interval", fhi)
	}
	return a
}

// outputTemplate is the yt-dlp -o template: the archive layout (Site/Channel,
// or one folder), playlists numbered, browser downloads in one flat folder.
func outputTemplate(o JobOptions, p ytdlpPaths, link string) string {
	var tmpl string
	switch {
	case o.Mode == "temp" && o.Playlist:
		tmpl = "%(playlist_index|0)03d - %(title).150B [%(id)s].%(ext)s"
	case o.Mode == "temp":
		tmpl = "%(title).150B [%(id)s].%(ext)s"
	case p.Flat && o.Playlist:
		tmpl = "%(playlist_title,playlist|Playlist).80B - %(playlist_index|0)03d - %(title).150B [%(id)s].%(ext)s"
	case p.Flat:
		tmpl = "%(upload_date>%Y-%m-%d|0000-00-00)s - %(title).150B [%(id)s].%(ext)s"
	case o.Playlist:
		tmpl = "%(extractor_key)s/%(playlist_title,playlist|Playlist).120B/%(playlist_index|0)03d - %(title).150B [%(id)s].%(ext)s"
	default:
		tmpl = "%(extractor_key)s/%(uploader,channel,creator,uploader_id|Unknown).80B/%(upload_date>%Y-%m-%d|0000-00-00)s - %(title).150B [%(id)s].%(ext)s"
	}
	// On X, yt-dlp's id is the media ID; "already downloaded" looks for the
	// tweet ID (display_id). The media ID stays: a tweet may hold several videos.
	if u, err := url.Parse(link); err == nil && hostMatch(normHost(u), []string{"x.com", "twitter.com"}) {
		tmpl = strings.Replace(tmpl, "[%(id)s]", "[%(display_id)s] [%(id)s]", 1)
	}
	return tmpl
}

// formatArgs picks the formats for a preset.
func formatArgs(o JobOptions) []string {
	var a []string
	switch o.Preset {
	case "best":
		if o.Height > 0 {
			h := strconv.Itoa(o.Height)
			a = append(a, "-f", "bv*[height<=?"+h+"]+ba/b[height<=?"+h+"]/bv*+ba/b")
		} else {
			a = append(a, "-f", "bv*+ba/b")
		}
		a = append(a, "--merge-output-format", "mp4/mkv")
	case "compat":
		sort := "vcodec:h264,res,acodec:aac,ext:mp4:m4a"
		if o.Height > 0 {
			sort = fmt.Sprintf("vcodec:h264,res:%d,acodec:aac,ext:mp4:m4a", o.Height)
		}
		a = append(a, "-f", "bv*+ba/b", "-S", sort, "--merge-output-format", "mp4", "--remux-video", "mp4")
	case "mp3":
		a = append(a, "-f", "ba/b", "-x", "--audio-format", "mp3", "--audio-quality", "0")
	case "audio":
		a = append(a, "-f", "ba/b", "-x", "--audio-format", "best")
	}
	return a
}

type gallerydlRun struct {
	Dir       string
	Flat      bool // temp mode: everything in one folder
	Cookies   CookieArg
	Pace      Pacing
	Archive   string // download-archive path (used when o.NewOnly)
	URLs      []string
	RateLimit string // gallery-dl -r ("2560K"), "" = none
}

func gallerydlArgs(o JobOptions, r gallerydlRun) []string {
	a := []string{"--config-ignore", "-R", "5", "--http-timeout", "60"}
	if r.Flat {
		a = append(a, "-D", r.Dir)
	} else {
		a = append(a, "-d", r.Dir)
	}
	a = append(a, r.Cookies.GalleryDL()...)
	if r.RateLimit != "" {
		a = append(a, "-r", r.RateLimit)
	}
	if r.Pace.Request != "" {
		a = append(a, "--sleep-request", r.Pace.Request)
	}
	if r.Pace.File != "" && r.Pace.File != "0" {
		a = append(a, "--sleep", r.Pace.File)
	}
	if o.NewOnly && r.Archive != "" {
		a = append(a, "--download-archive", r.Archive)
		if o.Mark {
			// -s lists without downloading; with archive-event=skip that
			// listing writes the archive (gallery-dl's SimulationJob, job.py).
			a = append(a, "-s", "-o", "archive-event=skip")
		}
	}
	switch {
	case o.Limit > 0 && len(o.Items) == 0:
		// Subscriptions: the newest N posts (profiles) or files (galleries).
		flag := "--range"
		if len(o.Sections) > 0 {
			flag = "--post-range"
		}
		a = append(a, flag, fmt.Sprintf("1-%d", o.Limit))
	case len(o.Sections) > 0 && len(o.Items) == 0 && r.Pace.MaxItems > 0:
		a = append(a, "--post-range", fmt.Sprintf("1-%d", r.Pace.MaxItems))
	}
	return append(append(a, "--"), r.URLs...)
}

// progressLine is a parsed yt-dlp progress template line.
type progressLine struct {
	Status     string
	Downloaded int64
	Total      int64
	Speed      float64
	ETA        int
	Index, N   int
	Title      string
}

func parseProgress(line string) (progressLine, bool) {
	rest, ok := strings.CutPrefix(line, progPrefix)
	if !ok {
		return progressLine{}, false
	}
	f := strings.SplitN(rest, "|", 9)
	if len(f) < 9 {
		return progressLine{}, false
	}
	p := progressLine{Status: f[0], Title: f[8]}
	p.Downloaded = int64(pnum(f[1]))
	p.Total = int64(pnum(f[2]))
	if p.Total <= 0 {
		p.Total = int64(pnum(f[3]))
	}
	p.Speed = pnum(f[4])
	p.ETA = int(pnum(f[5]))
	p.Index = int(pnum(f[6]))
	p.N = int(pnum(f[7]))
	return p, true
}

func pnum(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NA" || s == "None" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// parseGalleryLine interprets gallery-dl's default stdout: one path per
// downloaded file, "# path" for files that already existed.
func parseGalleryLine(line string) (path string, skipped bool) {
	line = strings.TrimRight(line, "\r")
	if after, ok := strings.CutPrefix(line, "# "); ok {
		return after, true
	}
	if after, ok := strings.CutPrefix(line, "✔ "); ok { // terminal mode marker
		return after, false
	}
	return line, false
}
