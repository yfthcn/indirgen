package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A rolled-back engine must stay rolled back until the user lifts the hold.
func TestRollbackSurvivesScheduledCheck(t *testing.T) {
	gh, m := setup(t)
	publishAll(t, gh, "2026.10.01", false)
	m.UpdateAll(context.Background(), false)
	publishAll(t, gh, "2026.10.06", false)
	m.UpdateAll(context.Background(), false)
	if err := m.Rollback(EngYTDLP); err != nil {
		t.Fatal(err)
	}
	m.UpdateAll(context.Background(), false)
	if s := m.States()[0]; s.Version != "2026.10.01" || !s.Held || s.Latest != "2026.10.06" {
		t.Fatalf("rollback undone by scheduled check: %+v", s)
	}
	if err := m.Unhold(context.Background(), EngYTDLP); err != nil {
		t.Fatal(err)
	}
	if s := m.States()[0]; s.Version != "2026.10.06" || s.Held {
		t.Fatalf("unhold did not update: %+v", s)
	}
}

// file/N must serve the N-th entry of file_names, even after a deletion.
func TestFileIndexMatchesUIList(t *testing.T) {
	cfg := &Config{dataDir: t.TempDir(), AllowedHosts: []string{"example.com"}}
	cfg.applyDefaults()
	cfg.Auth = "never"
	cfg.Library = t.TempDir()
	q := NewQueue(cfg, nil, nil, nil, NewHub())
	a, b := filepath.Join(cfg.Library, "a.txt"), filepath.Join(cfg.Library, "b.txt")
	os.WriteFile(a, []byte("AAA"), 0o644)
	os.WriteFile(b, []byte("BBB"), 0o644)
	q.jobs["x"] = &Job{ID: "x", Status: StDone, Files: []string{a, b}}
	os.Remove(a)
	srv := (&Server{cfg: cfg, queue: q, hub: q.hub, ctx: context.Background()}).Handler()
	get := func(n string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs/x/file/"+n, nil))
		return rec
	}
	if r := get("0"); r.Code != 404 {
		t.Fatalf("deleted file 0 served %d %q", r.Code, r.Body.String())
	}
	if r := get("1"); r.Code != 200 || r.Body.String() != "BBB" {
		t.Fatalf("file 1: %d %q", r.Code, r.Body.String())
	}
}

// -listen / -library are per run; config.json keeps its own values.
func TestFlagOverridesNotPersisted(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen, cfg.Library = "0.0.0.0:9999", "/elsewhere"
	if err := cfg.SetPacing(Pacing{Pace: "fast"}); err != nil { // any later save
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var disk Config
	json.Unmarshal(raw, &disk)
	if disk.Listen != "127.0.0.1:8899" || disk.Library == "/elsewhere" || disk.Pace != "fast" {
		t.Fatalf("override leaked into config.json: %s", raw)
	}
}

// "Only new" with nothing new is a done job, not an error.
func TestNewOnlyEmptyRunIsDone(t *testing.T) {
	cfg := &Config{dataDir: t.TempDir(), AllowedHosts: []string{"example.com"}}
	cfg.applyDefaults()
	q := NewQueue(cfg, nil, nil, nil, NewHub())
	ctx := t.Context()
	q.ctx = ctx
	q.jobs["n"] = &Job{ID: "n", Engine: EngGalleryDL, Status: StRunning, Opts: JobOptions{NewOnly: true}}
	q.finish(ctx, "n", nil) // a successful run that produced no files
	if s := q.jobs["n"]; s.Status != StDone || s.Note == "" || s.Error != "" {
		t.Fatalf("empty only-new run: %+v", s)
	}
}
