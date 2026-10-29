package main

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// About: identity, build info, changelog and the app's own update check.
//
// project.json is the single source for name, meaning, repo and links.
// version comes from main.go (build.sh overrides it from the git tag);
// commit and build date are injected by build.sh or, for a plain
// "go build" inside the git repo, read from Go's embedded VCS info.
// ---------------------------------------------------------------------------

//go:embed project.json
var projectRaw []byte

//go:embed CHANGELOG.md
var changelogRaw string

// Set with -ldflags "-X main.commit=… -X main.buildDate=…" (build.sh).
var (
	commit    string
	buildDate string
)

type Project struct {
	Name        string            `json:"name"`
	Display     string            `json:"display"`
	Meaning     map[string]string `json:"meaning"`
	Description map[string]string `json:"description"`
	Repo        string            `json:"repo"`
	Site        string            `json:"site"`
	License     string            `json:"license"`
	Author      string            `json:"author"`
}

var project = func() Project {
	var p Project
	if err := json.Unmarshal(projectRaw, &p); err != nil {
		panic("project.json: " + err.Error())
	}
	return p
}()

type BuildInfo struct {
	Version  string `json:"version"`
	Commit   string `json:"commit,omitempty"` // short hash
	Dirty    bool   `json:"dirty,omitempty"`  // built with uncommitted changes
	Date     string `json:"date,omitempty"`   // YYYY-MM-DD
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

var buildInfo = sync.OnceValue(func() BuildInfo {
	b := BuildInfo{Version: version, Commit: commit, Date: buildDate,
		Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Commit = cmp.Or(b.Commit, s.Value)
			case "vcs.time":
				if b.Date == "" && len(s.Value) >= 10 {
					b.Date = s.Value[:10]
				}
			case "vcs.modified":
				b.Dirty = s.Value == "true"
			}
		}
	}
	if len(b.Commit) > 7 {
		b.Commit = b.Commit[:7]
	}
	return b
})

// versionLine is the CLI "version" output.
func versionLine() string {
	b := buildInfo()
	s := project.Name + " " + b.Version
	var extra []string
	if b.Commit != "" {
		c := b.Commit
		if b.Dirty {
			c += "-dirty"
		}
		extra = append(extra, c)
	}
	if b.Date != "" {
		extra = append(extra, b.Date)
	}
	extra = append(extra, b.Platform)
	return s + " (" + strings.Join(extra, ", ") + ")"
}

// ---- changelog ------------------------------------------------------------

type ChangelogEntry struct {
	Version string   `json:"version"`
	Date    string   `json:"date,omitempty"`
	Items   []string `json:"items"`
}

// parseChangelog reads "## 1.2.3 · 2026-10-26" sections and their top-level
// "- " bullets (section headings like "### Fixed" are skipped).
func parseChangelog(md string, limit, maxItems int) []ChangelogEntry {
	var out []ChangelogEntry
	var cur *ChangelogEntry
	for line := range strings.Lines(md) {
		line = strings.TrimRight(line, "\r\n")
		if h, ok := strings.CutPrefix(line, "## "); ok {
			if len(out) == limit {
				break
			}
			v, d, _ := strings.Cut(h, "·")
			out = append(out, ChangelogEntry{Version: strings.TrimSpace(v), Date: strings.TrimSpace(d)})
			cur = &out[len(out)-1]
			continue
		}
		if item, ok := strings.CutPrefix(line, "- "); ok && cur != nil && len(cur.Items) < maxItems {
			cur.Items = append(cur.Items, strings.TrimSpace(item))
		}
	}
	return out
}

// ---- update check ---------------------------------------------------------

const (
	UpdUnknown     = ""            // not checked yet
	UpdCurrent     = "current"     // running the latest release (or newer)
	UpdAvailable   = "available"   // a newer release exists
	UpdUnpublished = "unpublished" // the repo has no release yet
	UpdError       = "error"
)

type UpdateState struct {
	Status         string    `json:"status"`
	Latest         string    `json:"latest,omitempty"`
	URL            string    `json:"url,omitempty"` // release page
	Checked        time.Time `json:"checked,omitzero"`
	Error          string    `json:"error,omitempty"`
	RateLimitUntil time.Time `json:"rate_limit_until,omitzero"` // GitHub said wait: the next try
	Checking       bool      `json:"checking,omitempty"`
	Installing     bool      `json:"installing,omitempty"`
}

// Updater checks the app's own GitHub releases and installs them
// (selfupdate.go).
type Updater struct {
	api     string
	repo    string
	gh      *apiClient // release check: rate limit, ETag cache (the engines' one in the server)
	client  *http.Client
	mu      sync.Mutex
	state   UpdateState
	run     sync.Mutex // one check or install at a time
	notify  func(UpdateState)
	exePath func() (string, error) // the running binary; replaced in tests
}

func NewUpdater(api string) *Updater {
	return &Updater{api: api, repo: project.Repo, gh: newAPIClient(""), client: &http.Client{}, exePath: runningExe}
}

func (u *Updater) State() UpdateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func (u *Updater) set(fn func(*UpdateState)) {
	u.mu.Lock()
	fn(&u.state)
	s := u.state
	u.mu.Unlock()
	if u.notify != nil {
		u.notify(s)
	}
}

// Check asks GitHub for the latest stable release and compares versions.
func (u *Updater) Check(ctx context.Context) UpdateState {
	if !u.run.TryLock() {
		return u.State() // a check is already running
	}
	defer u.run.Unlock()
	u.set(func(s *UpdateState) { s.Checking = true })

	st := UpdateState{Checked: time.Now()}
	rel, err := u.release(ctx)
	var rl *RateLimitError
	switch {
	case errors.As(err, &rl): // not checked: the last result stands
		st = u.State()
		st.Checking, st.RateLimitUntil = false, rl.Until
	case errors.Is(err, errNoRelease):
		st.Status = UpdUnpublished
	case err != nil:
		st.Status, st.Error = UpdError, firstLine(err.Error())
	default:
		st.Latest, st.URL = strings.TrimPrefix(rel.Tag, "v"), rel.URL
		st.Status = UpdCurrent
		if newerVersion(st.Latest, version) {
			st.Status = UpdAvailable
		}
	}
	u.set(func(s *UpdateState) { *s = st })
	return st
}

var errNoRelease = errors.New("henüz yayınlanmış sürüm yok")

type appRelease struct {
	Tag        string    `json:"tag_name"`
	URL        string    `json:"html_url"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

// release fetches the latest stable release of the app.
func (u *Updater) release(ctx context.Context) (*appRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	hdr := http.Header{"Accept": {"application/vnd.github+json"}}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}
	b, err := u.gh.get(ctx, "GitHub", u.api+"/repos/"+u.repo+"/releases/latest", hdr)
	var se *APIStatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, errNoRelease // no release, or the repo is not public yet
	}
	if err != nil {
		return nil, err
	}
	var rel appRelease
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, err
	}
	if rel.Draft || rel.Prerelease || rel.Tag == "" {
		return nil, errNoRelease
	}
	return &rel, nil
}

// Loop checks on start and once a day.
func (u *Updater) Loop(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if st := u.Check(ctx); st.Status == UpdAvailable {
			slog.Info("İndirgen için yeni sürüm var", "kurulu", version, "yeni", st.Latest, "adres", st.URL)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// newerVersion reports whether a > b for "x.y.z" versions. A suffix such as
// "-dev" marks a pre-release, which sorts before the plain release.
func newerVersion(a, b string) bool {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return false // never nag about versions we cannot compare
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return pa[3] > pb[3] // release (1) beats pre-release (0)
}

func parseVersion(s string) ([4]int, bool) {
	var v [4]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	core, _, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	if !hasPre {
		v[3] = 1
	}
	return v, true
}

// ---- API payload ----------------------------------------------------------

type AboutInfo struct {
	Project   Project          `json:"project"`
	Build     BuildInfo        `json:"build"`
	Update    UpdateState      `json:"update"`
	Changelog []ChangelogEntry `json:"changelog"`
	Data      string           `json:"data"`
	Library   string           `json:"library"`
	FreeBytes int64            `json:"free_bytes"`
	Retention int              `json:"temp_retention"`
	Engines   []EngineState    `json:"engines"`
	UpdateChk string           `json:"update_check"` // on | off
	Install   InstallMode      `json:"install"`
}
