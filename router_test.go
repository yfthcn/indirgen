package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Reproduces the real Instagram image-post failure: yt-dlp says "no video",
// gallery-dl (-j mode) reports the login redirect inside its JSON with exit 0.
func TestProbeInstagramImagePostAsksForCookies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses shell scripts as fake engines")
	}
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("yt-dlp", `[ "$1" = "--version" ] && { echo 2026.08.19; exit 0; }
echo "ERROR: [Instagram] DeHgyYjjK3o: No video formats found!; please report this issue" >&2; exit 1
`)
	write("gallery-dl", `[ "$1" = "--version" ] && { echo 1.32.15; exit 0; }
printf '[\n  [\n    -1,\n    {\n      "error": "HttpError",\n      "message": "HTTP redirect to login page (https://www.instagram.com/accounts/login/)"\n    }\n  ]\n]\n'
`)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	r := &Router{eng: NewEngineManager(cfg), cookies: NewCookieStore(cfg.Dir("cookies"))}

	_, err := r.Probe(context.Background(), "https://www.instagram.com/p/DeHgy0lDA0h/?utm_source=ig_web_copy_link", "")
	if err == nil || !strings.Contains(err.Error(), "instagram.com için cookies.txt ekle") {
		t.Fatalf("want cookie hint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "redirect to login") {
		t.Fatalf("original reason lost: %v", err)
	}
}

// gallery-dl -j output: metadata, two images and a queued child URL.
func TestProbeGalleryCountsAndThumbnail(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses shell scripts as fake engines")
	}
	bin := t.TempDir()
	body := `#!/bin/sh
[ "$1" = "--version" ] && { echo 1.32.15; exit 0; }
cat <<'J'
[[2, {"category": "imgur", "title": "  Ankara\n gezisi ", "author": {"name": "kuzgun"}}],
 [3, "https://i.imgur.com/a.mp4", {"extension": "mp4"}],
 [3, "https://i.imgur.com/b.jpg", {"extension": "jpg"}],
 [6, "https://imgur.com/a/child", {}]]
J
`
	if err := os.WriteFile(filepath.Join(bin, "gallery-dl"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	r := &Router{eng: NewEngineManager(cfg), cookies: NewCookieStore(cfg.Dir("cookies"))}

	res, err := r.Probe(context.Background(), "https://imgur.com/a/xyz", EngGalleryDL)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 3 || res.Thumbnail != "https://i.imgur.com/b.jpg" || res.Title != "Ankara gezisi" ||
		res.Uploader != "kuzgun" || res.Extractor != "imgur" {
		t.Fatalf("%+v", res)
	}
}
