package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// A sibling dir that merely shares the library's name prefix is outside it.
func TestRemoveEmptyParentsStaysInsideRoot(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "İndirgen")
	sibling := filepath.Join(base, "İndirgen2", "empty")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	removeEmptyParents(sibling, lib)
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("dir outside the library was removed: %v", err)
	}
}

func TestFirstLineKeepsUTF8(t *testing.T) {
	s := strings.Repeat("a", 159) + "şğüöçı"
	if got := firstLine(s); !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got[len(got)-4:])
	}
}

// Rolling back runs a smoke test; engine lookups must not wait for it.
func TestRollbackDoesNotBlockLookups(t *testing.T) {
	gh, m := setup(t)
	slow := []byte("#!/bin/sh\nsleep 1\necho 2026.10.01\n")
	gh.publish("yt-dlp/yt-dlp", "2026.10.01", map[string][]byte{"yt-dlp_linux": slow})
	m.UpdateAll(context.Background(), false)
	gh.publish("yt-dlp/yt-dlp", "2026.10.06", map[string][]byte{"yt-dlp_linux": script("2026.10.06")})
	m.UpdateAll(context.Background(), false)
	if m.States()[0].Previous == "" {
		t.Fatal("no previous install to roll back to")
	}

	done := make(chan error, 1)
	go func() { done <- m.Rollback(EngYTDLP) }()
	time.Sleep(200 * time.Millisecond) // rollback is now inside its smoke test
	t0 := time.Now()
	m.Path(EngYTDLP)
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Fatalf("Path blocked for %v during rollback", d)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := m.States()[0]; s.Version != "2026.10.01" {
		t.Fatalf("rollback did not switch: %+v", s)
	}
}

// Run with -race: reading settings while they are saved must not race.
func TestSettingsReadDuringSave(t *testing.T) {
	cfg := &Config{dataDir: t.TempDir(), AllowedHosts: []string{"example.com"}}
	cfg.applyDefaults()
	cfg.Auth = "never"
	srv := (&Server{cfg: cfg, hub: NewHub(), ctx: context.Background()}).Handler()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 20 {
			_ = cfg.SetPacing(Pacing{Pace: "custom", Request: "5-9", File: "1"})
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
			if rec.Code != 200 {
				t.Errorf("status %d", rec.Code)
			}
		}
	}()
	wg.Wait()
}
