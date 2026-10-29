package main

import (
	"net"
	"strings"
	"testing"
)

// Before the managed engines arrive, the banner shows the system ones for
// what they are, and says the newest are on their way.
func TestBannerMarksSystemEngines(t *testing.T) {
	si := startInfo{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8898}, Library: t.TempDir(),
		Engines: []EngineState{
			{Name: "yt-dlp", Source: "system", Version: "2026.09.27", Available: true, Required: true},
			{Name: "gallery-dl", Source: "managed", Version: "1.32.18", Available: true, Required: true},
			{Name: "ffmpeg", Source: "missing", Required: true},
		}, Fetching: true}
	var b strings.Builder
	printBanner(&b, palette{}, si)
	out := b.String()
	for _, want := range []string{"yt-dlp 2026.09.27 (sistem)", "gallery-dl 1.32.18  ", "ffmpeg yok", "en son sürümler indiriliyor"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "1.32.18 (sistem)") {
		t.Error("managed engine marked as system")
	}
	si.Fetching = false
	b.Reset()
	printBanner(&b, palette{}, si)
	if strings.Contains(b.String(), "indiriliyor") {
		t.Error("fetching note without a fetch")
	}
}

func TestEngineInstallable(t *testing.T) {
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	m := NewEngineManager(cfg)
	for _, c := range []struct {
		goos, goarch, ffmpeg string
		want                 map[string]bool
	}{
		{"linux", "amd64", "managed", map[string]bool{"yt-dlp": true, "gallery-dl": true, "ffmpeg": true, "deno": true}},
		{"linux", "amd64", "system", map[string]bool{"ffmpeg": false, "yt-dlp": true}},
		{"linux", "arm64", "managed", map[string]bool{"gallery-dl": false, "ffmpeg": true, "yt-dlp": true}}, // no gallery-dl build
		{"darwin", "arm64", "managed", map[string]bool{"ffmpeg": false, "gallery-dl": false, "deno": true}},
	} {
		m.goos, m.goarch, cfg.FFmpeg = c.goos, c.goarch, c.ffmpeg
		for name, want := range c.want {
			if got := m.Installable(name); got != want {
				t.Errorf("%s/%s ffmpeg=%s: Installable(%s) = %v", c.goos, c.goarch, c.ffmpeg, name, got)
			}
		}
	}
}
