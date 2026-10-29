package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestYTDLPNightlyChannel(t *testing.T) {
	gh, m := setup(t)
	publishAll(t, gh, "2026.10.01", false)
	m.UpdateAll(context.Background(), false)
	yt := script("2026.10.28.232835")
	gh.publish("yt-dlp/yt-dlp-nightly-builds", "2026.10.28.232835", map[string][]byte{
		"yt-dlp_linux": yt, "SHA2-256SUMS": []byte(sum(yt) + "  yt-dlp_linux\n"),
	})
	if err := m.cfg.SetChannel("nightly"); err != nil {
		t.Fatal(err)
	}
	m.UpdateAll(context.Background(), false)
	if s := m.States()[0]; s.Version != "2026.10.28.232835" || s.Channel != "nightly" || !s.Verified {
		t.Fatalf("nightly: %+v", s)
	}
	m.cfg.SetChannel("stable")
	m.UpdateAll(context.Background(), false)
	if s := m.States()[0]; s.Version != "2026.10.01" || s.Channel != "stable" {
		t.Fatalf("back to stable: %+v", s)
	}
	if err := m.cfg.SetChannel("beta"); err == nil {
		t.Fatal("bad channel accepted")
	}
}

func TestFindFirefoxProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, ok := findFirefoxProfile(); ok {
		t.Fatal("found a profile in an empty home")
	}
	mk := func(dir string, age time.Duration) {
		os.MkdirAll(dir, 0o700)
		f := filepath.Join(dir, "cookies.sqlite")
		os.WriteFile(f, []byte("x"), 0o600)
		ts := time.Now().Add(-age)
		os.Chtimes(f, ts, ts)
	}
	mk(filepath.Join(home, ".mozilla/firefox/old.default"), 48*time.Hour)
	mk(filepath.Join(home, "snap/firefox/common/.mozilla/firefox/abc.default-release"), time.Minute)
	p, ok := findFirefoxProfile()
	if !ok || p.Install != "snap" || filepath.Base(p.Dir) != "abc.default-release" {
		t.Fatalf("%+v", p)
	}
}
