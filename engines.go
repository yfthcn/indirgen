package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Engine manager
//
// Every engine (yt-dlp, gallery-dl, ffmpeg, deno) is pulled from its official
// GitHub release as a standalone binary. "Latest stable" means GitHub's
// /releases/latest endpoint, which never returns drafts or prereleases.
// New versions are installed side by side, verified (SHA-256 when the
// project publishes sums), smoke-tested with --version and only then
// switched to. A failing update never replaces a working engine.
// ---------------------------------------------------------------------------

const (
	assetRaw   = "raw"
	assetZip   = "zip"
	assetTarXZ = "tar.xz"
	maxAsset   = 400 << 20
)

type ghAsset struct {
	Name      string    `json:"name"`
	URL       string    `json:"browser_download_url"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

func (r *ghRelease) asset(name string) (ghAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ghAsset{}, false
}

// platformAsset picks the asset that names[goos/goarch] points to.
func (r *ghRelease) platformAsset(names map[string]string, goos, goarch string) (ghAsset, error) {
	n, ok := names[goos+"/"+goarch]
	if !ok {
		return ghAsset{}, errUnsupportedPlatform
	}
	a, ok := r.asset(n)
	if !ok {
		return ghAsset{}, fmt.Errorf("release %s içinde %s yok", r.Tag, n)
	}
	return a, nil
}

// pick describes which asset of a release to install for this platform.
type pick struct {
	Asset   ghAsset
	Kind    string   // raw | zip | tar.xz
	Members []string // binaries to extract from archives (base names)
	Version string   // human readable version
	Key     string   // change-detection key
}

type EngineSpec struct {
	Name        string
	Repo        string
	Forge       string // "" = GitHub, "codeberg" = Codeberg (Forgejo API, same release schema)
	Tag         string // "" = /releases/latest
	Binary      string // main executable name
	VersionArgs []string
	Required    bool
	Choose      func(rel *ghRelease, goos, goarch string) (pick, error)
	Checksum    func(rel *ghRelease, p pick) (sumAsset string)
	// Fresh: the installed build (key cur) is good enough although the
	// latest key differs. nil = any new key is an update.
	Fresh func(cur, latest string) bool
}

// ffmpegRefresh: BtbN rebuilds each release branch every day. A daily
// rebuild of the same version is not worth ~100 MB; a new version (n9.0 →
// n9.1) is installed at once, the same version at most once a month.
const ffmpegRefresh = 30 * 24 * time.Hour

func ffmpegFresh(cur, latest string) bool {
	cn, ct, ok1 := strings.Cut(cur, "@")
	ln, lt, ok2 := strings.Cut(latest, "@")
	if !ok1 || !ok2 || cn != ln {
		return false
	}
	c, err1 := time.Parse(time.RFC3339, ct)
	l, err2 := time.Parse(time.RFC3339, lt)
	return err1 == nil && err2 == nil && l.Sub(c) < ffmpegRefresh
}

var hexRe = regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`)

func sumsFileChecksum(name string) func(*ghRelease, pick) string {
	return func(rel *ghRelease, _ pick) string {
		if _, ok := rel.asset(name); ok {
			return name
		}
		return ""
	}
}

var engineSpecs = []EngineSpec{
	{
		Name: "yt-dlp", Repo: "yt-dlp/yt-dlp", Binary: "yt-dlp",
		VersionArgs: []string{"--version"}, Required: true,
		Choose: func(rel *ghRelease, goos, goarch string) (pick, error) {
			a, err := rel.platformAsset(map[string]string{
				"linux/amd64": "yt-dlp_linux",
				"linux/arm64": "yt-dlp_linux_aarch64",
			}, goos, goarch)
			if err != nil {
				return pick{}, err
			}
			return pick{Asset: a, Kind: assetRaw, Version: rel.Tag, Key: rel.Tag}, nil
		},
		Checksum: sumsFileChecksum("SHA2-256SUMS"),
	},
	{
		// gallery-dl moved to Codeberg; GitHub releases only carry source archives.
		Name: "gallery-dl", Repo: "mikf/gallery-dl", Forge: "codeberg", Binary: "gallery-dl",
		VersionArgs: []string{"--version"}, Required: true,
		Choose: func(rel *ghRelease, goos, goarch string) (pick, error) {
			a, err := rel.platformAsset(map[string]string{
				"linux/amd64": "gallery-dl.bin",
			}, goos, goarch)
			if err != nil {
				return pick{}, err
			}
			return pick{Asset: a, Kind: assetRaw, Version: strings.TrimPrefix(rel.Tag, "v"), Key: rel.Tag}, nil
		},
		Checksum: sumsFileChecksum("SHA256SUMS"),
	},
	{
		// Stable ffmpeg release branch builds (nX.Y), never the master snapshot.
		Name: "ffmpeg", Repo: "BtbN/FFmpeg-Builds", Tag: "latest", Binary: "ffmpeg",
		VersionArgs: []string{"-version"}, Required: true,
		Choose: func(rel *ghRelease, goos, goarch string) (pick, error) {
			plat := map[string]string{
				"linux/amd64": "linux64",
				"linux/arm64": "linuxarm64",
			}[goos+"/"+goarch]
			if plat == "" {
				return pick{}, errUnsupportedPlatform
			}
			re := regexp.MustCompile(`^ffmpeg-n(\d+)\.(\d+)(?:\.(\d+))?-latest-` + plat + `-gpl-[\d.]+\.tar\.xz$`)
			var best ghAsset
			bestV := [3]int{-1, -1, -1}
			for _, a := range rel.Assets {
				m := re.FindStringSubmatch(a.Name)
				if m == nil {
					continue
				}
				var v [3]int
				for i := range v {
					v[i], _ = strconv.Atoi(m[i+1])
				}
				if slices.Compare(v[:], bestV[:]) > 0 {
					best, bestV = a, v
				}
			}
			if best.Name == "" {
				return pick{}, fmt.Errorf("uygun ffmpeg (%s) bulunamadı", plat)
			}
			ver := fmt.Sprintf("n%d.%d", bestV[0], bestV[1])
			if bestV[2] > 0 {
				ver += fmt.Sprintf(".%d", bestV[2])
			}
			ver += " (" + best.UpdatedAt.Format("2006-01-02") + ")"
			return pick{Asset: best, Kind: assetTarXZ, Members: []string{"ffmpeg", "ffprobe"},
				Version: ver, Key: best.Name + "@" + best.UpdatedAt.UTC().Format(time.RFC3339)}, nil
		},
		Checksum: sumsFileChecksum("checksums.sha256"),
		Fresh:    ffmpegFresh,
	},
	{
		// yt-dlp needs an external JS runtime for full YouTube support.
		Name: "deno", Repo: "denoland/deno", Binary: "deno",
		VersionArgs: []string{"--version"}, Required: false,
		Choose: func(rel *ghRelease, goos, goarch string) (pick, error) {
			a, err := rel.platformAsset(map[string]string{
				"linux/amd64": "deno-x86_64-unknown-linux-gnu.zip",
				"linux/arm64": "deno-aarch64-unknown-linux-gnu.zip",
			}, goos, goarch)
			if err != nil {
				return pick{}, err
			}
			return pick{Asset: a, Kind: assetZip, Members: []string{"deno"},
				Version: strings.TrimPrefix(rel.Tag, "v"), Key: rel.Tag}, nil
		},
		Checksum: func(rel *ghRelease, p pick) string {
			n := p.Asset.Name + ".sha256sum"
			if _, ok := rel.asset(n); ok {
				return n
			}
			return ""
		},
	},
}

var errUnsupportedPlatform = errors.New("bu platform için resmi binary yok")

func specByName(name string) (EngineSpec, bool) {
	for _, sp := range engineSpecs {
		if sp.Name == name {
			return sp, true
		}
	}
	return EngineSpec{}, false
}

// EngineState is what we persist and show in the UI.
type EngineState struct {
	Name           string    `json:"name"`
	Source         string    `json:"source"` // managed | system | missing
	Version        string    `json:"version"`
	Key            string    `json:"key,omitempty"`
	Dir            string    `json:"dir,omitempty"`
	Path           string    `json:"path,omitempty"`
	SHA256         string    `json:"sha256,omitempty"`
	Verified       bool      `json:"verified"`
	Previous       string    `json:"previous,omitempty"` // previous install dir (rollback)
	Held           bool      `json:"held,omitempty"`     // rolled back: auto-update keeps this version
	Checked        time.Time `json:"checked,omitzero"`
	Updated        time.Time `json:"updated,omitzero"`
	Error          string    `json:"error,omitempty"`
	RateLimitUntil time.Time `json:"rate_limit_until,omitzero"` // GitHub said wait: the next try
	Updating       bool      `json:"updating"`
	Latest         string    `json:"latest,omitempty"`  // newest stable seen upstream at last check
	Channel        string    `json:"channel,omitempty"` // yt-dlp: stable | nightly (not stored)
	Required       bool      `json:"required"`
	Available      bool      `json:"available"`
}

type EngineManager struct {
	cfg      *Config
	root     string
	api      string     // GitHub API base
	codeberg string     // Codeberg (Forgejo) API base
	gh       *apiClient // both APIs: rate limit, ETag cache
	client   *http.Client
	mu       sync.Mutex
	states   map[string]*EngineState
	running  sync.Mutex // serializes update runs
	onEvent  func()
	goos     string
	goarch   string
}

func NewEngineManager(cfg *Config) *EngineManager {
	m := &EngineManager{
		cfg:      cfg,
		root:     cfg.Dir("engines"),
		api:      envOr("INDIRGEN_GITHUB_API", "https://api.github.com"),
		codeberg: envOr("INDIRGEN_CODEBERG_API", "https://codeberg.org/api/v1"),
		gh:       newAPIClient(cfg.Dir("api-cache.json")),
		client:   &http.Client{Timeout: 0},
		states:   map[string]*EngineState{},
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
	}
	_ = os.MkdirAll(m.root, 0o755)
	m.load()
	m.refreshFallbacks()
	return m
}

func (m *EngineManager) stateFile() string { return filepath.Join(m.root, "state.json") }

func (m *EngineManager) load() {
	raw, err := os.ReadFile(m.stateFile())
	if err == nil {
		var list []*EngineState
		if json.Unmarshal(raw, &list) == nil {
			for _, s := range list {
				s.Updating = false
				m.states[s.Name] = s
			}
		}
	}
	for _, sp := range engineSpecs {
		if _, ok := m.states[sp.Name]; !ok {
			m.states[sp.Name] = &EngineState{Name: sp.Name, Source: "missing"}
		}
		m.states[sp.Name].Required = sp.Required
	}
}

func (m *EngineManager) saveLocked() {
	list := make([]*EngineState, 0, len(engineSpecs))
	for _, sp := range engineSpecs {
		list = append(list, m.states[sp.Name])
	}
	raw, _ := json.MarshalIndent(list, "", "  ")
	if err := writeFileAtomic(m.stateFile(), raw, 0o644); err != nil {
		slog.Warn("motor durumu kaydedilemedi", "hata", err)
	}
}

func (m *EngineManager) emit() {
	if m.onEvent != nil {
		m.onEvent()
	}
}

// refreshFallbacks validates managed installs and falls back to a system
// binary in PATH when no managed copy exists.
//
// "--version" of a system binary can take seconds (Python startup), so it
// runs without the lock: engine lookups and the scheduler never wait for it.
func (m *EngineManager) refreshFallbacks() {
	type plan struct {
		managed bool   // keep the managed install
		system  string // else: system binary path ("" = missing)
		version string // freshly probed system version ("" = keep stored)
	}
	plans := make(map[string]*plan, len(engineSpecs))

	m.mu.Lock()
	for _, sp := range engineSpecs {
		s := m.states[sp.Name]
		p := &plan{}
		plans[sp.Name] = p
		preferSystem := sp.Name == "ffmpeg" && m.cfg.FFmpeg == "system"
		if !preferSystem && s.Source == "managed" && s.Path != "" {
			if _, err := os.Stat(s.Path); err == nil {
				p.managed = true
				continue
			}
		}
		p.system = m.systemBinary(sp.Binary)
		if p.system != "" && (s.Source != "system" || s.Path != p.system || s.Version == "") {
			p.version = "?" // needs probing
		}
	}
	m.mu.Unlock()

	for _, sp := range engineSpecs {
		if p := plans[sp.Name]; p.version == "?" {
			p.version = versionFromOutput(sp.Name, runVersion(p.system, sp.VersionArgs))
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sp := range engineSpecs {
		s, p := m.states[sp.Name], plans[sp.Name]
		switch {
		case p.managed:
			s.Available = true
		case p.system != "":
			if p.version != "" {
				s.Version = p.version
			}
			s.Source, s.Path, s.Dir, s.Available = "system", p.system, filepath.Dir(p.system), true
			s.Verified, s.Key, s.Held = false, "", false
		default:
			s.Source, s.Path, s.Version, s.Key, s.Available, s.Held = "missing", "", "", "", false, false
		}
	}
	m.saveLocked()
}

func (m *EngineManager) systemBinary(base string) string {
	p, err := exec.LookPath(base)
	if err != nil {
		return ""
	}
	abs, _ := filepath.Abs(p)
	root, _ := filepath.Abs(m.root)
	if isWithin(root, abs) {
		return ""
	}
	return abs
}

func (m *EngineManager) States() []EngineState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]EngineState, 0, len(engineSpecs))
	for _, sp := range engineSpecs {
		s := *m.states[sp.Name]
		if sp.Name == EngYTDLP {
			s.Channel = m.cfg.Channel()
		}
		out = append(out, s)
	}
	return out
}

func (m *EngineManager) Path(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.states[name]; ok && s.Available {
		return s.Path
	}
	return ""
}

func (m *EngineManager) Ready(name string) bool { return m.Path(name) != "" }

// Installable reports whether the update check installs a managed copy of
// this engine; false keeps the system binary (ffmpeg set to "system", or no
// official build for this platform). Decided without the network: Choose
// tells an unsupported platform apart before looking at the release.
func (m *EngineManager) Installable(name string) bool {
	if name == "ffmpeg" && m.cfg.FFmpeg == "system" {
		return false
	}
	for _, sp := range engineSpecs {
		if sp.Name == name {
			_, err := sp.Choose(&ghRelease{}, m.goos, m.goarch)
			return !errors.Is(err, errUnsupportedPlatform)
		}
	}
	return false
}

// Need returns the engine's path, or an error when it is not usable yet.
func (m *EngineManager) Need(name string) (string, error) {
	if p := m.Path(name); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("%s hazır değil", name)
}

// Env returns the environment for engine subprocesses: our engine dirs are
// prepended to PATH so yt-dlp finds deno/ffmpeg/ffprobe automatically.
func (m *EngineManager) Env() []string {
	m.mu.Lock()
	var dirs []string
	seen := map[string]bool{}
	for _, sp := range engineSpecs {
		s := m.states[sp.Name]
		if s.Available && s.Source == "managed" && !seen[s.Dir] {
			dirs = append(dirs, s.Dir)
			seen[s.Dir] = true
		}
	}
	m.mu.Unlock()
	sep := string(os.PathListSeparator)
	prefix := strings.Join(dirs, sep)
	env := os.Environ()
	out := make([]string, 0, len(env)+3)
	pathSet := false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "PATH") {
			if prefix != "" {
				v = prefix + sep + v
			}
			out = append(out, k+"="+v)
			pathSet = true
			continue
		}
		out = append(out, kv)
	}
	if !pathSet && prefix != "" {
		out = append(out, "PATH="+prefix)
	}
	return append(out, "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
}

// Loop keeps engines up to date in the background.
func (m *EngineManager) Loop(ctx context.Context) {
	m.UpdateAll(ctx, false)
	if !m.cfg.AutoUpdate {
		return
	}
	t := time.NewTicker(time.Duration(m.cfg.UpdateIntervalHours) * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.UpdateAll(ctx, false)
		}
	}
}

// UpdateAll checks every engine. force reinstalls even when the key matches.
func (m *EngineManager) UpdateAll(ctx context.Context, force bool) {
	if !m.running.TryLock() {
		return // a check is running: it covers this one
	}
	defer m.running.Unlock()
	m.updateAllLocked(ctx, force)
}

// UpdateAllWait waits for a running check to end, then checks: a channel
// switch must not be dropped because a periodic check was in progress.
func (m *EngineManager) UpdateAllWait(ctx context.Context) {
	m.running.Lock()
	defer m.running.Unlock()
	m.updateAllLocked(ctx, false)
}

func (m *EngineManager) updateAllLocked(ctx context.Context, force bool) {
	for _, sp := range engineSpecs {
		if ctx.Err() != nil {
			return
		}
		if sp.Name == "ffmpeg" && m.cfg.FFmpeg == "system" {
			continue
		}
		if err := m.update(ctx, sp, force); err != nil {
			if _, ok := errors.AsType[*RateLimitError](err); ok {
				continue // logged once by the API client
			}
			if errors.Is(err, errUnsupportedPlatform) {
				slog.Info("sistemdeki kurulu motor kullanılacak", "motor", sp.Name, "sebep", err)
			} else {
				slog.Warn("motor güncellenemedi", "motor", sp.Name, "hata", firstLine(err.Error()))
			}
		}
	}
	m.refreshFallbacks()
	m.emit()
}

// RateLimitUntil is when the engine check may ask GitHub again (zero: now).
func (m *EngineManager) RateLimitUntil() time.Time { return m.gh.blockedUntil(m.api) }

func (m *EngineManager) setState(name string, fn func(s *EngineState)) {
	m.mu.Lock()
	fn(m.states[name])
	m.saveLocked()
	m.mu.Unlock()
	m.emit()
}

func (m *EngineManager) update(ctx context.Context, sp EngineSpec, force bool) (err error) {
	m.setState(sp.Name, func(s *EngineState) { s.Updating = true })
	defer func() {
		m.setState(sp.Name, func(s *EngineState) {
			s.Updating = false
			if rl, ok := errors.AsType[*RateLimitError](err); ok { // not checked, not failed: the last result stands
				s.RateLimitUntil = rl.Until
				return
			}
			s.Checked, s.RateLimitUntil = time.Now(), time.Time{}
			if err != nil && !errors.Is(err, errUnsupportedPlatform) {
				s.Error = err.Error()
			} else if err == nil {
				s.Error = ""
			}
		})
	}()

	rel, err := m.release(ctx, sp)
	if err != nil {
		return err
	}
	p, err := sp.Choose(rel, m.goos, m.goarch)
	if err != nil {
		return err
	}
	m.mu.Lock()
	cur := *m.states[sp.Name]
	m.mu.Unlock()
	fresh := cur.Source == "managed" && sp.Fresh != nil && sp.Fresh(cur.Key, p.Key)
	m.setState(sp.Name, func(s *EngineState) {
		s.Latest = p.Version
		if fresh {
			s.Latest = cur.Version // a rebuild of the same version is no "update available"
		}
	})
	if !force && cur.Source == "managed" {
		if _, err := os.Stat(cur.Path); err == nil && (cur.Key == p.Key || cur.Held || fresh) {
			return nil // up to date, or rolled back on purpose: report Latest only
		}
	}

	expected, err := m.expectedSum(ctx, sp, rel, p)
	if err != nil {
		return err
	}
	slog.Info("motor indiriliyor", "motor", sp.Name, "sürüm", shortVersion(p.Version))
	dir, sum, verified, err := m.install(ctx, sp, p, expected)
	if err != nil {
		return err
	}
	bin := filepath.Join(dir, sp.Binary)
	m.setState(sp.Name, func(s *EngineState) {
		prev := ""
		if s.Source == "managed" && s.Dir != dir {
			prev = s.Dir
		}
		old := s.Previous
		s.Source, s.Version, s.Key, s.Dir, s.Path = "managed", p.Version, p.Key, dir, bin
		s.SHA256, s.Verified, s.Updated, s.Available = sum, verified, time.Now(), true
		s.Previous, s.Held = prev, false
		if old != "" && old != prev && old != dir {
			_ = os.RemoveAll(old)
		}
	})
	m.pruneEngineDir(sp.Name)
	slog.Info("motor hazır", "motor", sp.Name, "sürüm", shortVersion(p.Version), "sha256", map[bool]string{true: "doğrulandı", false: "yok"}[verified])
	return nil
}

// expectedSum is the SHA-256 the release publishes for the picked asset
// ("" when it publishes none).
func (m *EngineManager) expectedSum(ctx context.Context, sp EngineSpec, rel *ghRelease, p pick) (string, error) {
	if sp.Checksum == nil {
		return "", nil
	}
	sumName := sp.Checksum(rel, p)
	if sumName == "" {
		return "", nil
	}
	a, _ := rel.asset(sumName)
	expected, err := m.fetchChecksum(ctx, a, p.Asset.Name)
	if err != nil {
		return "", fmt.Errorf("checksum alınamadı: %w", err)
	}
	return expected, nil
}

// install downloads, verifies and unpacks the asset into a new folder and
// smoke-tests the binary there. On failure the folder is removed.
func (m *EngineManager) install(ctx context.Context, sp EngineSpec, p pick, expected string) (dir, sum string, verified bool, err error) {
	// A new folder for every install: a forced reinstall of the same release
	// must never touch the working copy or the one kept for rollback.
	dir = filepath.Join(m.root, sp.Name, safeName(p.Version+"-"+shortHash(p.Key)+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", false, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	dl := filepath.Join(dir, ".download")
	if sum, err = m.download(ctx, p.Asset, dl); err != nil {
		return "", "", false, err
	}
	if expected != "" {
		if !strings.EqualFold(expected, sum) {
			return "", "", false, fmt.Errorf("SHA-256 uyuşmadı (beklenen %s, gelen %s)", expected, sum)
		}
		verified = true
	}

	switch p.Kind {
	case assetRaw:
		err = os.Rename(dl, filepath.Join(dir, sp.Binary))
		if err != nil {
			return "", "", false, err
		}
	case assetZip:
		err = extractZip(dl, dir, p.Members)
	case assetTarXZ:
		err = extractTarXZ(ctx, dl, dir, p.Members)
	}
	if err != nil {
		return "", "", false, fmt.Errorf("arşiv açılamadı: %w", err)
	}
	_ = os.Remove(dl)
	bin := filepath.Join(dir, sp.Binary)
	if err = os.Chmod(bin, 0o755); err != nil {
		return "", "", false, err
	}
	for _, mem := range p.Members {
		_ = os.Chmod(filepath.Join(dir, mem), 0o755)
	}

	// Smoke test before switching.
	if out, serr := smokeTest(ctx, bin, sp.VersionArgs); serr != nil {
		return "", "", false, fmt.Errorf("smoke test başarısız: %v: %s", serr, firstLine(out))
	}
	return dir, sum, verified, nil
}

// pruneEngineDir removes install dirs that are neither current nor previous.
func (m *EngineManager) pruneEngineDir(name string) {
	m.mu.Lock()
	s := *m.states[name]
	m.mu.Unlock()
	base := filepath.Join(m.root, name)
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		p := filepath.Join(base, e.Name())
		if p != s.Dir && p != s.Previous {
			_ = os.RemoveAll(p)
		}
	}
}

// Rollback switches an engine back to its previous managed install. The
// smoke test runs without the lock so engine lookups (and with them the job
// scheduler) are not stalled while it runs.
func (m *EngineManager) Rollback(name string) error {
	errNone := errors.New("geri dönülecek önceki sürüm yok")
	sp, ok := specByName(name)
	if !ok {
		return errNone
	}
	m.mu.Lock()
	prev := m.states[name].Previous
	m.mu.Unlock()
	if prev == "" {
		return errNone
	}
	bin := filepath.Join(prev, sp.Binary)
	out, err := smokeTest(context.Background(), bin, sp.VersionArgs)
	if err != nil {
		return fmt.Errorf("önceki sürüm çalışmıyor: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.states[name]
	if s.Previous != prev {
		return errors.New("motor bu sırada güncellendi, tekrar dene")
	}
	s.Dir, s.Previous = s.Previous, s.Dir
	s.Path = bin
	s.Version = versionFromOutput(name, out)
	s.Key = ""
	s.Verified = false
	s.Held = true // otherwise the next scheduled check reinstalls the version just rolled back
	m.saveLocked()
	return nil
}

// Unhold lifts a rollback hold and installs the latest stable release.
func (m *EngineManager) Unhold(ctx context.Context, name string) error {
	if _, ok := specByName(name); !ok {
		return errors.New("bilinmeyen motor")
	}
	m.setState(name, func(s *EngineState) { s.Held = false })
	m.UpdateAll(ctx, false)
	return nil
}

func versionFromOutput(name, out string) string {
	l := firstLine(out)
	f := strings.Fields(l)
	switch {
	case name == "deno" && len(f) >= 2: // "deno 2.5.0 (stable, ...)"
		return f[1]
	case name == "ffmpeg" && len(f) >= 3: // "ffmpeg version n7.1-... Copyright"
		return f[2]
	}
	return l
}

func (m *EngineManager) release(ctx context.Context, sp EngineSpec) (*ghRelease, error) {
	base, forge := m.api, "GitHub"
	if sp.Forge == "codeberg" {
		base, forge = m.codeberg, "Codeberg"
	}
	repo := sp.Repo
	if sp.Name == EngYTDLP && m.cfg.Channel() == "nightly" {
		// Fixes for site changes (YouTube above all) land here days before
		// a stable release. Same assets and SHA2-256SUMS as stable.
		repo = "yt-dlp/yt-dlp-nightly-builds"
	}
	u := base + "/repos/" + repo + "/releases/latest"
	if sp.Tag != "" {
		u = base + "/repos/" + repo + "/releases/tags/" + sp.Tag
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hdr := http.Header{"Accept": {"application/json"}}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" && sp.Forge == "" {
		hdr.Set("Authorization", "Bearer "+tok)
	}
	b, err := m.gh.get(ctx, forge, u, hdr)
	if err != nil {
		return nil, err
	}
	var rel ghRelease
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, err
	}
	if rel.Draft || (rel.Prerelease && sp.Tag == "") {
		return nil, errors.New("en son release stable değil, atlandı")
	}
	return &rel, nil
}

func (m *EngineManager) fetchChecksum(ctx context.Context, a ghAsset, target string) (string, error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := httpGet(ctx2, m.client, a.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	var single []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		h := hexRe.FindString(line)
		if h == "" {
			continue
		}
		single = append(single, h)
		rest := strings.TrimSpace(strings.Replace(line, h, "", 1))
		rest = strings.TrimLeft(rest, " *")
		if filepath.Base(rest) == target || strings.HasSuffix(rest, "/"+target) {
			return strings.ToLower(h), nil
		}
	}
	// A *.sha256sum file for a single asset may only contain the hash.
	if len(single) == 1 {
		return strings.ToLower(single[0]), nil
	}
	return "", fmt.Errorf("%s için checksum satırı yok", target)
}

func (m *EngineManager) download(ctx context.Context, a ghAsset, dst string) (string, error) {
	// A stalled connection must not hold the update lock forever.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	resp, err := httpGet(ctx, m.client, a.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAsset+1))
	if err != nil {
		return "", err
	}
	if n > maxAsset {
		return "", errors.New("asset çok büyük")
	}
	if a.Size > 0 && n != a.Size {
		return "", fmt.Errorf("eksik indirme (%d/%d bayt)", n, a.Size)
	}
	return hex.EncodeToString(h.Sum(nil)), f.Sync()
}

func extractZip(src, dir string, members []string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	want := map[string]bool{}
	for _, mm := range members {
		want[mm] = true
	}
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if f.FileInfo().IsDir() || !want[base] {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dir, base), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(rc, maxAsset))
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr // a failed close (disk full) means a truncated binary
		}
		if err != nil {
			return err
		}
		delete(want, base)
	}
	if len(want) > 0 {
		return fmt.Errorf("arşivde bulunamadı: %s", strings.Join(slices.Sorted(maps.Keys(want)), ", "))
	}
	return nil
}

// extractTarXZ uses the system tar: Go's stdlib has no xz decoder and pulling
// one in would break the zero-dependency rule.
func extractTarXZ(ctx context.Context, src, dir string, members []string) error {
	tmp := filepath.Join(dir, ".x")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, "tar", "-xJf", src, "-C", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, firstLine(string(out)))
	}
	want := map[string]bool{}
	for _, mm := range members {
		want[mm] = true
	}
	err := filepath.WalkDir(tmp, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !want[d.Name()] {
			return err
		}
		if filepath.Base(filepath.Dir(p)) != "bin" {
			return nil
		}
		delete(want, d.Name())
		return os.Rename(p, filepath.Join(dir, d.Name()))
	})
	if err != nil {
		return err
	}
	if len(want) > 0 {
		return errors.New("arşivde beklenen binary'ler yok")
	}
	return nil
}

func smokeTest(ctx context.Context, bin string, args []string) (string, error) {
	ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, bin, args...).CombinedOutput()
	return string(out), err
}

func runVersion(bin string, args []string) string {
	out, _ := smokeTest(context.Background(), bin, args)
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = strings.ToValidUTF8(s[:160], "") // drop a rune cut in half
	}
	return strings.TrimSpace(s)
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = unsafeRe.ReplaceAllString(s, "_")
	return strings.Trim(s, "._")
}
