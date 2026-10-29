package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakeAppRelease struct {
	srv   *httptest.Server
	blobs map[string][]byte
	rel   appRelease
}

func newFakeAppRelease(t *testing.T, tag string, files map[string][]byte) *fakeAppRelease {
	f := &fakeAppRelease{blobs: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/"+project.Repo+"/releases/latest" {
			json.NewEncoder(w).Encode(f.rel)
			return
		}
		if b, ok := f.blobs[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	f.rel = appRelease{Tag: tag, URL: "https://example/rel"}
	for name, b := range files {
		p := "/dl/" + name
		f.blobs[p] = b
		f.rel.Assets = append(f.rel.Assets, ghAsset{Name: name, URL: f.srv.URL + p, Size: int64(len(b))})
	}
	return f
}

func appScript(ver string) []byte {
	return []byte("#!/bin/sh\necho '" + project.Name + " " + ver + " (test)'\n")
}

func setupSelfUpdate(t *testing.T) (exe string) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	orig := inDocker
	inDocker = func() bool { return false }
	t.Cleanup(func() { inDocker = orig })
	exe = filepath.Join(t.TempDir(), "indirgen")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestSelfUpdateInstalls(t *testing.T) {
	exe := setupSelfUpdate(t)
	name := appAssetName(runtime.GOOS, runtime.GOARCH)
	bin := appScript("99.0.0")
	gh := newFakeAppRelease(t, "v99.0.0", map[string][]byte{
		name: bin, sumsAsset: []byte(sum(bin) + "  " + name + "\n" + strings.Repeat("0", 64) + "  other\n"),
	})
	u := NewUpdater(gh.srv.URL)
	u.exePath = func() (string, error) { return exe, nil }
	if m := u.Mode(); !m.Can {
		t.Fatalf("mode: %+v", m)
	}
	ver, err := u.Install(context.Background())
	if err != nil || ver != "99.0.0" {
		t.Fatalf("install: %q %v", ver, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(bin) {
		t.Fatal("binary not replaced")
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "OLD" {
		t.Fatal("previous binary not kept as .old")
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestSelfUpdateRefusesUnsafeReleases(t *testing.T) {
	name := appAssetName(runtime.GOOS, runtime.GOARCH)
	good := appScript("99.0.0")
	cases := map[string]map[string][]byte{
		"bad checksum":      {name: good, sumsAsset: []byte(strings.Repeat("a", 64) + "  " + name + "\n")},
		"no SHA256SUMS":     {name: good},
		"wrong version":     {name: appScript("98.0.0"), sumsAsset: []byte(sum(appScript("98.0.0")) + "  " + name + "\n")},
		"no platform asset": {sumsAsset: []byte("")},
	}
	for label, files := range cases {
		t.Run(label, func(t *testing.T) {
			exe := setupSelfUpdate(t)
			u := NewUpdater(newFakeAppRelease(t, "v99.0.0", files).srv.URL)
			u.exePath = func() (string, error) { return exe, nil }
			if _, err := u.Install(context.Background()); err == nil {
				t.Fatal("installed an unsafe release")
			}
			if b, _ := os.ReadFile(exe); string(b) != "OLD" {
				t.Fatal("working binary was touched")
			}
		})
	}
}

func TestSelfUpdateBlockedInDocker(t *testing.T) {
	orig := inDocker
	inDocker = func() bool { return true }
	defer func() { inDocker = orig }()
	u := NewUpdater("http://127.0.0.1:1")
	if m := u.Mode(); m.Can || m.Reason != "docker" {
		t.Fatalf("%+v", m)
	}
	if _, err := u.Install(context.Background()); err == nil {
		t.Fatal("self-update ran in Docker")
	}
}

func TestHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if health(strings.TrimPrefix(ok.URL, "http://")) != 0 {
		t.Fatal("healthy server reported down")
	}
	if health("127.0.0.1:1") == 0 {
		t.Fatal("closed port reported healthy")
	}
}
