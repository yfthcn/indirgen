package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageSettings(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	s.cfg.fileLibrary = s.cfg.Library
	call := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	newLib := filepath.Join(t.TempDir(), "Arşiv", "Videolar")
	code, m := call("POST", "/api/storage", `{"library":"`+newLib+`","layout":"flat"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	st := s.cfg.Storage()
	if st.Pending != newLib || st.Library == newLib || st.Layout != "flat" || !isDir(newLib) {
		t.Fatalf("%+v", st)
	}
	raw, _ := os.ReadFile(s.cfg.path())
	if !strings.Contains(string(raw), `"library": "`+newLib+`"`) || !strings.Contains(string(raw), `"layout": "flat"`) {
		t.Fatalf("config.json:\n%s", raw)
	}
	// Choosing the folder in use again clears the pending change.
	if code, _ := call("POST", "/api/storage", `{"library":"`+s.cfg.Library+`"}`); code != 200 || s.cfg.Storage().Pending != "" {
		t.Fatalf("revert: %d %+v", code, s.cfg.Storage())
	}
	for _, bad := range []string{"relative/dir", "/", s.cfg.dataDir, filepath.Join(s.cfg.dataDir, "x"), filepath.Dir(s.cfg.dataDir)} {
		if code, _ := call("POST", "/api/storage", `{"library":"`+bad+`"}`); code != 400 {
			t.Errorf("accepted %q", bad)
		}
	}
	if code, _ := call("POST", "/api/storage", `{"layout":"weird"}`); code != 400 {
		t.Error("bad layout accepted")
	}
	// -library on the command line (Docker): read-only here.
	s.cfg.libraryFlag = true
	if code, _ := call("POST", "/api/storage", `{"library":"`+newLib+`"}`); code != 400 {
		t.Error("changed a -library folder")
	}
	s.cfg.libraryFlag = false

	// Folder picker: lists folders only, creates one, rejects odd names.
	base := t.TempDir()
	os.Mkdir(filepath.Join(base, "B"), 0o755)
	os.Mkdir(filepath.Join(base, "a"), 0o755)
	os.Mkdir(filepath.Join(base, ".gizli"), 0o755)
	os.WriteFile(filepath.Join(base, "dosya.txt"), nil, 0o644)
	code, m = call("GET", "/api/dirs?path="+base, "")
	if dirs, _ := json.Marshal(m["dirs"]); code != 200 || string(dirs) != `["a","B"]` || m["writable"] != true {
		t.Fatalf("%d %v", code, m)
	}
	if code, m = call("POST", "/api/dirs", `{"path":"`+base+`","name":"Yeni Klasör"}`); code != 200 || m["path"] != filepath.Join(base, "Yeni Klasör") {
		t.Fatalf("mkdir %d %v", code, m)
	}
	for _, bad := range []string{"..", "a/b", "", "."} {
		if code, _ := call("POST", "/api/dirs", `{"path":"`+base+`","name":"`+bad+`"}`); code != 400 {
			t.Errorf("mkdir accepted %q", bad)
		}
	}
}

func TestFlatLayoutArgs(t *testing.T) {
	a := strings.Join(ytdlpArgs(JobOptions{Preset: "best"}, ytdlpPaths{Flat: true}, "https://a.b/c"), "\n")
	if !strings.Contains(a, "-o\n%(upload_date>%Y-%m-%d|0000-00-00)s - %(title).150B [%(id)s].%(ext)s") {
		t.Fatalf("%s", a)
	}
	a = strings.Join(ytdlpArgs(JobOptions{Preset: "best"}, ytdlpPaths{}, "https://a.b/c"), "\n")
	if !strings.Contains(a, "-o\n%(extractor_key)s/") {
		t.Fatal("site layout changed")
	}
}
