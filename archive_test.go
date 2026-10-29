package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func mkLib(t *testing.T) (lib string, put func(rel, body string, age time.Duration) string) {
	lib = t.TempDir()
	put = func(rel, body string, age time.Duration) string {
		p := filepath.Join(lib, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
		return p
	}
	return
}

func names[T any](list []T, f func(T) string) []string {
	var out []string
	for _, x := range list {
		out = append(out, f(x))
	}
	return out
}

func TestLibraryList(t *testing.T) {
	dir, put := mkLib(t)
	put("Youtube/Kanal A/2026-01-01 - eski [a].mp4", "aaaa", 48*time.Hour)
	put("Youtube/Kanal A/2026-10-20 - yeni [b].mp4", "bbbbbbbb", time.Hour)
	put("Youtube/Kanal B/sarki [c].mp3", "cc", 2*time.Hour)
	put("Instagram/kedi/foto [d].jpg", "d", 3*time.Hour)
	put("tek.webm", "eeeee", 30*time.Minute)
	put("Youtube/Kanal A/yarim.mp4.part", "zz", 0)       // unfinished: hidden
	put("Youtube/.gizli/x.mp4", "zz", 0)                 // dotfolder: hidden
	os.MkdirAll(filepath.Join(dir, "Bos Klasor"), 0o755) // empty: listed
	l := NewLibrary(dir)

	root, err := l.List(LibQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if root.Total.Files != 5 || root.Total.Size != 4+8+2+1+5 || root.Flat {
		t.Fatalf("total %+v", root.Total)
	}
	if got := names(root.Dirs, func(d LibDir) string { return d.Name }); !slices.Equal(got, []string{"Youtube", "Instagram", "Bos Klasor"}) {
		t.Fatalf("dirs by newest: %v", got)
	}
	if len(root.Files) != 1 || root.Files[0].Name != "tek.webm" || root.Files[0].Kind != "video" {
		t.Fatalf("root files %+v", root.Files)
	}

	yt, _ := l.List(LibQuery{Path: "Youtube", Sort: "size"})
	if got := names(yt.Dirs, func(d LibDir) string { return d.Name }); !slices.Equal(got, []string{"Kanal A", "Kanal B"}) {
		t.Fatalf("by size: %v", got)
	}
	if yt.Dirs[0].Files != 2 || yt.Dirs[0].Size != 12 {
		t.Fatalf("Kanal A aggregate %+v", yt.Dirs[0])
	}

	flat, _ := l.List(LibQuery{Flat: true, Sort: "new", Limit: 2})
	if flat.Matches != 5 || len(flat.Files) != 2 || flat.Files[0].Name != "tek.webm" {
		t.Fatalf("flat %+v", flat)
	}
	page2, _ := l.List(LibQuery{Flat: true, Sort: "new", Limit: 2, Offset: 4})
	if len(page2.Files) != 1 || page2.Files[0].Kind != "video" || page2.Files[0].Name != "2026-01-01 - eski [a].mp4" {
		t.Fatalf("page 2 %+v", page2.Files)
	}

	// Search covers folder names (the uploader) and file names, in a subtree.
	s, _ := l.List(LibQuery{Q: "kanal b"})
	if s.Matches != 1 || s.Files[0].Kind != "audio" || s.Files[0].Path != "Youtube/Kanal B/sarki [c].mp3" {
		t.Fatalf("search %+v", s.Files)
	}
	if s, _ := l.List(LibQuery{Path: "Instagram", Q: "kanal"}); s.Matches != 0 {
		t.Fatal("search left its folder")
	}
}

func TestLibraryStaysInside(t *testing.T) {
	dir, put := mkLib(t)
	put("ic/a.mp4", "a", 0)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET"), 0o644)
	if runtime.GOOS != "windows" {
		os.Symlink(outside, filepath.Join(dir, "kacis"))
		os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "ic", "link.txt"))
	}
	l := NewLibrary(dir)

	for _, p := range []string{"../" + filepath.Base(outside) + "/secret.txt", "kacis/secret.txt", "ic/link.txt", "/etc/passwd", "..\\..\\x"} {
		if f, _, err := l.OpenFile(p); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if string(b) == "SECRET" || p == "/etc/passwd" {
				t.Fatalf("read outside the library via %q", p)
			}
		}
	}
	if _, err := l.List(LibQuery{Path: "kacis"}); err == nil {
		t.Fatal("listed a symlinked folder outside the library")
	}
	if all, _ := l.List(LibQuery{Flat: true}); all.Matches != 1 {
		t.Fatalf("symlinks indexed: %+v", all.Files)
	}
	if _, _, err := l.Delete("kacis/secret.txt"); err == nil {
		t.Fatal("deleted through a symlink")
	}
	if _, _, err := l.Delete("../" + filepath.Base(outside)); err == nil {
		t.Fatal("deleted outside the library")
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatal("file outside the library is gone")
	}
	if _, _, err := l.Delete(""); err != errLibRoot {
		t.Fatalf("library root deletable: %v", err)
	}
}

func TestLibraryDeleteAndJobs(t *testing.T) {
	dir, put := mkLib(t)
	a := put("Youtube/Kanal A/a.mp4", "aaaa", 0)
	b := put("Youtube/Kanal A/b.mp4", "bb", 0)
	put("Youtube/Kanal A/b.mp4.part", "x", 0)
	c := put("Youtube/Kanal B/c.mp4", "c", 0)
	cfg := &Config{AllowedHosts: []string{"example.com"}, dataDir: t.TempDir()}
	cfg.applyDefaults()
	cfg.Auth, cfg.Library = "never", dir
	q := NewQueue(cfg, nil, nil, nil, NewHub())
	q.jobs["j1"] = &Job{ID: "j1", Status: StDone, Files: []string{a, b}}
	q.jobs["j2"] = &Job{ID: "j2", Status: StDone, Files: []string{c}}
	l := NewLibrary(dir)
	srv := (&Server{cfg: cfg, queue: q, hub: q.hub, lib: l, ctx: context.Background()}).Handler()
	do := func(method, url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(method, url, nil))
		return rec
	}

	// one file
	if r := do("DELETE", "/api/library?path=Youtube/Kanal%20B/c.mp4"); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "Youtube", "Kanal B")); err == nil {
		t.Fatal("empty parent folder kept")
	}
	if len(q.jobs["j2"].Files) != 0 {
		t.Fatal("job still lists the deleted file")
	}

	// a folder, including its partial file
	r := do("DELETE", "/api/library?path=Youtube/Kanal%20A")
	var res LibDeleted
	json.Unmarshal(r.Body.Bytes(), &res)
	if r.Code != 200 || res.Files != 3 || res.Bytes != 7 {
		t.Fatalf("%d %+v", r.Code, res)
	}
	if _, err := os.Stat(filepath.Join(dir, "Youtube")); err == nil {
		t.Fatal("Youtube folder left empty")
	}
	if len(q.jobs["j1"].Files) != 0 {
		t.Fatal("job still lists files of the deleted folder")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("library folder itself removed")
	}
	if r := do("DELETE", "/api/library?path=yok.mp4"); r.Code != 404 {
		t.Fatalf("missing file: %d", r.Code)
	}
}

func TestLibraryServeAndZip(t *testing.T) {
	dir, put := mkLib(t)
	put("Y/K/video.mp4", "0123456789", 0)
	put("Y/K/foto.jpg", "img", 0)
	put("Y/K/yarim.part", "x", 0)
	cfg := &Config{AllowedHosts: []string{"example.com"}, dataDir: t.TempDir()}
	cfg.applyDefaults()
	cfg.Auth, cfg.Library = "never", dir
	srv := (&Server{cfg: cfg, hub: NewHub(), lib: NewLibrary(dir), ctx: context.Background()}).Handler()

	req := httptest.NewRequest("GET", "/api/library/file?path=Y/K/video.mp4&inline=1", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 206 || rec.Body.String() != "2345" || rec.Header().Get("Content-Type") != "video/mp4" ||
		rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("range: %d %q %v", rec.Code, rec.Body, rec.Header())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/api/library/zip?path=Y/K", nil))
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := names(zr.File, func(f *zip.File) string { return f.Name })
	slices.Sort(got)
	if !slices.Equal(got, []string{"foto.jpg", "video.mp4"}) {
		t.Fatalf("zip entries %v", got)
	}
	if cd := rec.Header().Get("Content-Disposition"); !bytes.Contains([]byte(cd), []byte(`filename="K.zip"`)) {
		t.Fatalf("zip name %q", cd)
	}
}

func TestLibraryMissingFolder(t *testing.T) {
	l := NewLibrary(filepath.Join(t.TempDir(), "yok"))
	res, err := l.List(LibQuery{})
	if err != nil || res.Exists || res.Total.Files != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

// Names keep the form the site sent (Instagram: decomposed "a" + U+0308);
// the search matches whatever form was typed, and folds Turkish i's.
func TestLibrarySearchNormalizesAndFoldsTurkish(t *testing.T) {
	lib, put := mkLib(t)
	put("Instagram/Alina Ja\u0308ger/2020-07-31 - Video by clippedinandfree [CDUMkliABpa].mp4", "v", time.Hour)
	put("Youtube/Sezen/IŞIK - Klip [aaaaaaaaaaa].mkv", "v", time.Hour)
	put("Youtube/Gezi/İstanbul Vlog [bbbbbbbbbbb].mkv", "v", time.Hour)
	put("Youtube/Gezi/I\u0307zmir Turu [ccccccccccc].mkv", "v", time.Hour) // İ decomposed
	put("Youtube/Viet/Pho\u031B\u0309 Ha\u0300 No\u0323\u0302i [ddddddddddd].mkv", "v", time.Hour)
	put("Youtube/English/Interview with Ian [eeeeeeeeeee].mkv", "v", time.Hour)
	l := NewLibrary(lib)
	find := func(q string) []string {
		res, err := l.List(LibQuery{Q: q})
		if err != nil {
			t.Fatal(err)
		}
		return names(res.Files, func(f LibFile) string { return f.Name })
	}
	for q, want := range map[string]string{
		"Jäger":                       "clippedinandfree", // typed composed, stored decomposed
		"Ja\u0308ger":                 "clippedinandfree", // typed decomposed
		"JÄGER":                       "clippedinandfree",
		"ışık":                        "IŞIK",
		"Işık":                        "IŞIK",
		"IŞIK":                        "IŞIK",
		"istanbul":                    "İstanbul",
		"İSTANBUL":                    "İstanbul",
		"izmir":                       "I\u0307zmir",
		"İzmir":                       "I\u0307zmir",
		"Phở Hà Nội":                  "Pho\u031B\u0309", // several marks, composed step by step
		"INTERVIEW":                   "Interview",
		"ian":                         "Ian",
		"clippedinandfree [CDUMkliAB": "clippedinandfree",
	} {
		got := find(q)
		if len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%q → %v, want one match containing %q", q, got, want)
		}
	}
	if got := find("Jager"); len(got) != 0 {
		t.Errorf("accents are not ignored: %v", got)
	}
}
