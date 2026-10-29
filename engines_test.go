package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves /repos/<repo>/releases/... and asset downloads.
type fakeGitHub struct {
	mu       sync.Mutex
	srv      *httptest.Server
	releases map[string]*ghRelease // key: repo or repo@tag
	blobs    map[string][]byte
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{releases: map[string]*ghRelease{}, blobs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		if b, ok := f.blobs[p]; ok {
			w.Write(b)
			return
		}
		prefix := ""
		if strings.HasPrefix(p, "/cb/") { // fake Codeberg API base
			prefix, p = "cb:", strings.TrimPrefix(p, "/cb")
		}
		if rest, ok := strings.CutPrefix(p, "/repos/"); ok {
			var key string
			if before, _, ok := strings.Cut(rest, "/releases/latest"); ok {
				key = before
			} else if before, after, ok := strings.Cut(rest, "/releases/tags/"); ok {
				key = before + "@" + after
			}
			if rel, ok := f.releases[prefix+key]; ok {
				json.NewEncoder(w).Encode(rel)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) publish(key, tag string, files map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel := &ghRelease{Tag: tag}
	for name, b := range files {
		path := "/dl/" + key + "/" + tag + "/" + name
		f.blobs[path] = b
		rel.Assets = append(rel.Assets, ghAsset{Name: name, URL: f.srv.URL + path, Size: int64(len(b)), UpdatedAt: time.Now()})
	}
	f.releases[key] = rel
}

func script(out string) []byte { return []byte("#!/bin/sh\necho '" + out + "'\n") }

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func zipOf(t *testing.T, files map[string][]byte) []byte {
	var buf strings.Builder
	zw := zip.NewWriter(&writerAdapter{&buf})
	for n, b := range files {
		h := &zip.FileHeader{Name: n, Method: zip.Deflate}
		h.SetMode(0o755)
		w, _ := zw.CreateHeader(h)
		w.Write(b)
	}
	zw.Close()
	return []byte(buf.String())
}

type writerAdapter struct{ b *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { return w.b.Write(p) }

func tarXZOf(t *testing.T, root string, files map[string][]byte) []byte {
	dir := t.TempDir()
	for n, b := range files {
		p := filepath.Join(dir, root, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o755)
	}
	out := filepath.Join(t.TempDir(), "a.tar.xz")
	if b, err := exec.Command("tar", "-cJf", out, "-C", dir, root).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, b)
	}
	b, _ := os.ReadFile(out)
	return b
}

func setup(t *testing.T) (*fakeGitHub, *EngineManager) {
	if runtime.GOOS != "linux" {
		t.Skip("engine install test uses shell scripts as fake binaries")
	}
	gh := newFakeGitHub(t)
	t.Setenv("INDIRGEN_GITHUB_API", gh.srv.URL)
	t.Setenv("INDIRGEN_CODEBERG_API", gh.srv.URL+"/cb")
	t.Setenv("PATH", "/usr/bin:/bin") // no system yt-dlp/gallery-dl fallback
	cfg := &Config{dataDir: t.TempDir(), AutoUpdate: true}
	cfg.applyDefaults()
	m := NewEngineManager(cfg)
	m.goos, m.goarch = "linux", "amd64"
	return gh, m
}

func publishAll(t *testing.T, gh *fakeGitHub, ytv string, badSum bool) {
	yt := script(ytv)
	sums := fmt.Sprintf("%s  yt-dlp_linux\n%s  yt-dlp.exe\n", sum(yt), strings.Repeat("0", 64))
	if badSum {
		sums = strings.Repeat("a", 64) + "  yt-dlp_linux\n"
	}
	gh.publish("yt-dlp/yt-dlp", ytv, map[string][]byte{
		"yt-dlp_linux": yt, "yt-dlp.exe": []byte("x"), "SHA2-256SUMS": []byte(sums),
	})
	gdl := script("1.40.0")
	// GitHub mirror only has source archives; binaries live on Codeberg.
	gh.publish("mikf/gallery-dl", "v1.40.0", map[string][]byte{"v1.40.0.tar.gz": []byte("src")})
	gh.publish("cb:mikf/gallery-dl", "v1.40.0", map[string][]byte{
		"gallery-dl.bin": gdl, "gallery-dl.exe": []byte("x"),
		"SHA256SUMS": []byte(sum(gdl) + "  gallery-dl.bin\n" + strings.Repeat("1", 64) + "  gallery-dl.exe\n"),
	})

	ffBins := map[string][]byte{"bin/ffmpeg": script("ffmpeg version n8.0-x Copyright"), "bin/ffprobe": script("ffprobe")}
	ff80 := tarXZOf(t, "ffmpeg-n8.0-latest-linux64-gpl-8.0", ffBins)
	ff71 := tarXZOf(t, "ffmpeg-n7.1-latest-linux64-gpl-7.1", ffBins)
	master := []byte("should never be picked")
	gh.publish("BtbN/FFmpeg-Builds@latest", "latest", map[string][]byte{
		"ffmpeg-n8.0-latest-linux64-gpl-8.0.tar.xz": ff80,
		"ffmpeg-n7.1-latest-linux64-gpl-7.1.tar.xz": ff71,
		"ffmpeg-master-latest-linux64-gpl.tar.xz":   master,
		"checksums.sha256": []byte(fmt.Sprintf("%s  ffmpeg-n8.0-latest-linux64-gpl-8.0.tar.xz\n%s  ffmpeg-n7.1-latest-linux64-gpl-7.1.tar.xz\n",
			sum(ff80), sum(ff71))),
	})
	dz := zipOf(t, map[string][]byte{"deno": script("deno 2.9.1 (stable, release)")})
	gh.publish("denoland/deno", "v2.9.1", map[string][]byte{
		"deno-x86_64-unknown-linux-gnu.zip":           dz,
		"deno-x86_64-unknown-linux-gnu.zip.sha256sum": []byte(sum(dz) + "\n"),
	})
}

func TestEngineInstallUpdateAndRollbackSafety(t *testing.T) {
	gh, m := setup(t)
	publishAll(t, gh, "2026.10.01", false)
	m.UpdateAll(context.Background(), false)

	st := map[string]EngineState{}
	for _, s := range m.States() {
		st[s.Name] = s
	}
	want := map[string]struct {
		ver      string
		verified bool
	}{
		"yt-dlp": {"2026.10.01", true}, "gallery-dl": {"1.40.0", true},
		"deno": {"2.9.1", true}, "ffmpeg": {"n8.0", true},
	}
	for name, w := range want {
		s := st[name]
		if s.Source != "managed" || !s.Available || !strings.HasPrefix(s.Version, w.ver) || s.Verified != w.verified {
			t.Fatalf("%s: unexpected state %+v", name, s)
		}
	}
	for name, s := range st {
		if s.Latest == "" || s.Latest != s.Version {
			t.Fatalf("%s: latest %q vs version %q", name, s.Latest, s.Version)
		}
	}
	// ffprobe extracted next to ffmpeg; PATH env includes engine dirs.
	if _, err := os.Stat(filepath.Join(st["ffmpeg"].Dir, "ffprobe")); err != nil {
		t.Fatal("ffprobe missing")
	}
	if env := strings.Join(m.Env(), "\n"); !strings.Contains(env, st["deno"].Dir) {
		t.Fatal("deno dir not on PATH")
	}

	// Same version again: nothing reinstalled.
	oldDir := st["yt-dlp"].Dir
	m.UpdateAll(context.Background(), false)
	if m.States()[0].Dir != oldDir {
		t.Fatal("reinstalled unchanged version")
	}

	// New release with a BAD checksum: must be rejected, old one stays.
	publishAll(t, gh, "2026.10.05", true)
	m.UpdateAll(context.Background(), false)
	s := m.States()[0]
	if s.Version != "2026.10.01" || s.Latest != "2026.10.05" || s.Dir != oldDir || s.Error == "" || !strings.Contains(s.Error, "SHA-256") {
		t.Fatalf("bad checksum not rejected: %+v", s)
	}

	// Good new release: switches, keeps previous for rollback. ffmpeg was
	// rebuilt too (same n8.0, newer date): that is not worth a reinstall.
	ffDir := st["ffmpeg"].Dir
	time.Sleep(1100 * time.Millisecond) // a newer build time (RFC3339 seconds)
	publishAll(t, gh, "2026.10.06", false)
	m.UpdateAll(context.Background(), false)
	for _, e := range m.States() {
		if e.Name == "ffmpeg" && (e.Dir != ffDir || e.Latest != e.Version) {
			t.Fatalf("same-version ffmpeg rebuild reinstalled or flagged: %+v", e)
		}
	}
	s = m.States()[0]
	if s.Version != "2026.10.06" || s.Previous != oldDir || s.Error != "" {
		t.Fatalf("update failed: %+v", s)
	}
	if err := m.Rollback("yt-dlp"); err != nil {
		t.Fatal(err)
	}
	s = m.States()[0]
	if s.Version != "2026.10.01" || s.Dir != oldDir {
		t.Fatalf("rollback failed: %+v", s)
	}
}

func TestBrokenBinaryNeverReplacesWorkingOne(t *testing.T) {
	gh, m := setup(t)
	publishAll(t, gh, "2026.10.01", false)
	m.UpdateAll(context.Background(), false)
	good := m.States()[0]

	broken := []byte("#!/bin/sh\nexit 3\n")
	gh.publish("yt-dlp/yt-dlp", "2026.10.09", map[string][]byte{
		"yt-dlp_linux": broken, "SHA2-256SUMS": []byte(sum(broken) + "  yt-dlp_linux\n"),
	})
	m.UpdateAll(context.Background(), false)
	s := m.States()[0]
	if s.Version != good.Version || s.Path != good.Path || !strings.Contains(s.Error, "smoke") {
		t.Fatalf("broken update was accepted: %+v", s)
	}
	if _, err := os.Stat(s.Path); err != nil {
		t.Fatal("working binary removed")
	}
}

func TestPrereleaseIgnored(t *testing.T) {
	gh, m := setup(t)
	gh.publish("yt-dlp/yt-dlp", "2026.11.01", map[string][]byte{"yt-dlp_linux": script("2026.11.01")})
	gh.mu.Lock()
	gh.releases["yt-dlp/yt-dlp"].Prerelease = true
	gh.mu.Unlock()
	m.UpdateAll(context.Background(), false)
	if s := m.States()[0]; s.Available {
		t.Fatalf("prerelease installed: %+v", s)
	}
}

func TestFFmpegFresh(t *testing.T) {
	k := func(name, day string) string { return name + "@" + day + "T03:00:00Z" }
	n90 := "ffmpeg-n9.0-latest-linux64-gpl-9.0.tar.xz"
	n91 := "ffmpeg-n9.1-latest-linux64-gpl-9.1.tar.xz"
	cases := []struct {
		cur, latest string
		fresh       bool
	}{
		{k(n90, "2026-10-26"), k(n90, "2026-10-27"), true},  // daily rebuild: skip
		{k(n90, "2026-10-01"), k(n90, "2026-10-30"), true},  // 29 days
		{k(n90, "2026-09-20"), k(n90, "2026-10-27"), false}, // over a month: refresh
		{k(n90, "2026-10-26"), k(n91, "2026-10-27"), false}, // new version: at once
		{"", k(n90, "2026-10-27"), false},                   // nothing installed
	}
	for _, c := range cases {
		if got := ffmpegFresh(c.cur, c.latest); got != c.fresh {
			t.Errorf("%s → %s = %v", c.cur, c.latest, got)
		}
	}
}
