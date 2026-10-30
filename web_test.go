package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func assetHash(t *testing.T, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(webFS, "web/"+name)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])[:12]
}

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	_, h := testServer(t, "never", "127.0.0.1:8899")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// The page is a skeleton: CSS and JS come from their own files, each with
// the hash of its content, so a browser never mixes an old cached file with
// a new page after an update.
func TestIndexLoadsVersionedAssets(t *testing.T) {
	r := get(t, "/")
	body := r.Body.String()
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("%d %v", r.Code, r.Header())
	}
	for _, name := range []string{"app.css", "i18n.js", "app.js"} {
		if want := "/" + name + "?v=" + assetHash(t, name); !strings.Contains(body, want) {
			t.Errorf("index lacks %s", want)
		}
	}
	// Nothing inline: the CSP below forbids it.
	for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(tag, " src=") {
			t.Errorf("inline script in index.html: %q", tag)
		}
	}
	for _, re := range []string{`<style`, `\sstyle="`, `\son[a-z]+="`} {
		if m := regexp.MustCompile(re).FindString(body); m != "" {
			t.Errorf("inline code in index.html: %q", m)
		}
	}
	for _, name := range []string{"app.js", "i18n.js"} {
		raw, _ := fs.ReadFile(webFS, "web/"+name)
		for _, re := range []string{`\sstyle="`, `\son[a-z]+="`} {
			if m := regexp.MustCompile(re).FindString(string(raw)); m != "" {
				t.Errorf("%s writes inline %q into the page", name, m)
			}
		}
	}
	csp := r.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") ||
		!strings.Contains(csp, "object-src 'none'") || !strings.Contains(csp, "base-uri 'none'") {
		t.Fatalf("CSP %q", csp)
	}
}

func TestAssetCachingAndTypes(t *testing.T) {
	for _, c := range []struct{ path, ctype, cache string }{
		{"/app.js?v=" + assetHash(t, "app.js"), "text/javascript", "public, max-age=31536000, immutable"},
		{"/app.css?v=" + assetHash(t, "app.css"), "text/css", "public, max-age=31536000, immutable"},
		{"/i18n.js?v=" + assetHash(t, "i18n.js"), "text/javascript", "public, max-age=31536000, immutable"},
		{"/app.js?v=000000000000", "text/javascript", "no-cache"}, // not this build's file: do not keep it
		{"/app.js", "text/javascript", "no-cache"},
		{"/manifest.webmanifest", "application/manifest+json", "no-cache"},
		{"/icon.svg", "image/svg+xml", "no-cache"},
	} {
		r := get(t, c.path)
		if r.Code != 200 || !strings.HasPrefix(r.Header().Get("Content-Type"), c.ctype) || r.Header().Get("Cache-Control") != c.cache {
			t.Errorf("%s: %d %q %q", c.path, r.Code, r.Header().Get("Content-Type"), r.Header().Get("Cache-Control"))
		}
		if r.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", c.path)
		}
	}
}

// Added to the home screen, İndirgen opens like an app: the manifest names
// it and every icon it lists is in the binary at its stated size.
func TestWebAppManifestAndIcons(t *testing.T) {
	raw, err := fs.ReadFile(webFS, "web/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name, ShortName, StartURL, Scope, Display, ThemeColor, BackgroundColor string
		Icons                                                                  []struct{ Src, Sizes, Type, Purpose string }
	}
	dec := json.NewDecoder(strings.NewReader(strings.NewReplacer(`"short_name"`, `"ShortName"`, `"start_url"`, `"StartURL"`,
		`"theme_color"`, `"ThemeColor"`, `"background_color"`, `"BackgroundColor"`).Replace(string(raw))))
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "İndirgen" || m.ShortName != "İndirgen" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" ||
		m.ThemeColor == "" || m.BackgroundColor == "" {
		t.Fatalf("%+v", m)
	}
	have := map[string]bool{}
	for _, ic := range m.Icons {
		if ic.Type != "image/png" {
			continue
		}
		f, err := webFS.Open("web" + ic.Src)
		if err != nil {
			t.Fatalf("%s: %v", ic.Src, err)
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil || ic.Sizes != fmt.Sprintf("%dx%d", cfg.Width, cfg.Height) {
			t.Fatalf("%s: %v, %dx%d, manifest says %s", ic.Src, err, cfg.Width, cfg.Height, ic.Sizes)
		}
		have[ic.Sizes+" "+ic.Purpose] = true
	}
	for _, want := range []string{"192x192 any", "512x512 any", "512x512 maskable"} {
		if !have[want] {
			t.Errorf("no %s PNG icon", want)
		}
	}
	f, err := webFS.Open("web/apple-touch-icon.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if cfg, err := png.DecodeConfig(f); err != nil || cfg.Width != 180 || cfg.Height != 180 {
		t.Fatalf("apple-touch-icon: %v %dx%d", err, cfg.Width, cfg.Height)
	}

	body := get(t, "/").Body.String()
	for _, want := range []string{
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<meta name="mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-title" content="İndirgen">`,
		`<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">`,
		`<meta name="theme-color" content="#0f1115" media="(prefers-color-scheme: dark)">`,
		`<meta name="theme-color" content="#f5f3ef" media="(prefers-color-scheme: light)">`,
		`viewport-fit=cover`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %s", want)
		}
	}
	if strings.Contains(body, "serviceWorker") {
		t.Error("no service worker: it would not run over plain http on the LAN")
	}
}

// A marking check says the same for yt-dlp and gallery-dl and gives no
// count: gallery-dl lists already known items too, so a number would mislead.
func TestMarkedTextHasNoCount(t *testing.T) {
	b, err := webFS.ReadFile("web/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	want := map[string]int{
		`sub_marked:"Mevcut öğeler işaretlendi; bundan sonra yalnızca yeniler inecek."`:      1,
		`note_marked:"Mevcut öğeler işaretlendi; bundan sonra yalnızca yeniler inecek."`:     1,
		`sub_marked:"Existing items marked; only new ones will be downloaded from now on."`:  1,
		`note_marked:"Existing items marked; only new ones will be downloaded from now on."`: 1,
	}
	for s, n := range want {
		if got := strings.Count(src, s); got != n {
			t.Errorf("%d× %s", got, s)
		}
	}
	for _, k := range []string{"sub_marked_one", "note_marked_one"} {
		if strings.Contains(src, k) {
			t.Errorf("%s still there", k)
		}
	}
}
