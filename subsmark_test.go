package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// "Only what comes next": the first check records the newest N in the
// download archive without downloading them.
func TestMarkOnlyArgs(t *testing.T) {
	o := JobOptions{Preset: "best", NewOnly: true, Limit: 10, Mark: true, Playlist: true, Embed: true, Subs: true, Sponsor: true}
	o.normalize()
	y := strings.Join(ytdlpArgs(o, ytdlpPaths{Home: "/lib", Temp: "/part", Archive: "/data/archive/yt-dlp.txt"}, "https://www.youtube.com/@x/videos"), " ")
	for _, want := range []string{"--skip-download", "--force-write-archive", "--download-archive /data/archive/yt-dlp.txt",
		"--playlist-items 1:10", "--yes-playlist", "--print video:" + markPrefix + "%(id)s", "-- https://www.youtube.com/@x/videos"} {
		if !strings.Contains(y, want) {
			t.Errorf("yt-dlp: no %q in %s", want, y)
		}
	}
	for _, not := range []string{"--embed-metadata", "--write-subs", "--sponsorblock", "--merge-output-format", "after_move:"} {
		if strings.Contains(y, not) {
			t.Errorf("yt-dlp: %q in a marking run: %s", not, y)
		}
	}

	g := strings.Join(gallerydlArgs(JobOptions{NewOnly: true, Limit: 5, Mark: true}, gallerydlRun{Dir: "/lib", Archive: "/data/archive/gallery-dl.sqlite3", URLs: []string{"https://example.com/g"}}), " ")
	for _, want := range []string{"-s ", "-o archive-event=skip", "--download-archive /data/archive/gallery-dl.sqlite3", "--range 1-5"} {
		if !strings.Contains(g, want) {
			t.Errorf("gallery-dl: no %q in %s", want, g)
		}
	}

	// Marking needs the archive and a limit: anywhere else it is dropped.
	for _, o := range []JobOptions{{Mark: true}, {Mark: true, NewOnly: true}, {Mark: true, Limit: 5}} {
		o.normalize()
		if o.Mark {
			t.Errorf("mark kept without new_only and limit: %+v", o)
		}
	}
}

// A fake engine that keeps a download archive the way yt-dlp and gallery-dl
// do: an entry already in it is skipped, a marking run writes entries
// without files, a normal run writes the file and the entry.
const fakeYTDLP = `#!/bin/sh
[ "$1" = "--version" ] && { echo 2026.10.26; exit 0; }
echo "$@" >> "$FAKE_LOG"
arch= skip= n=1000 home=
while [ $# -gt 0 ]; do
  case "$1" in
    --download-archive) arch=$2; shift ;;
    --skip-download) skip=1 ;;
    --playlist-items) n=${2#1:}; shift ;;
    -P) case "$2" in home:*) home=${2#home:} ;; esac; shift ;;
  esac
  shift
done
i=0
for id in $FAKE_IDS; do
  i=$((i+1)); [ $i -gt $n ] && break
  grep -qx "youtube $id" "$arch" 2>/dev/null && continue
  if [ -n "$skip" ]; then echo "SGR:M|$id"; else f="$home/$id.mp4"; echo x > "$f"; echo "SGR:F|$f"; fi
  echo "youtube $id" >> "$arch"
done
`

const fakeGalleryDL = `#!/bin/sh
[ "$1" = "--version" ] && { echo 1.32.18; exit 0; }
echo "$@" >> "$FAKE_LOG"
arch= sim= n=1000 dir=
while [ $# -gt 0 ]; do
  case "$1" in
    --download-archive) arch=$2; shift ;;
    -s) sim=1 ;;
    --range|--post-range) n=${2#1-}; shift ;;
    -d|-D) dir=$2; shift ;;
    -o|-r|-R|--http-timeout|--sleep|--sleep-request) shift ;;
  esac
  shift
done
i=0
for id in $FAKE_IDS; do
  i=$((i+1)); [ $i -gt $n ] && break
  grep -qx "g $id" "$arch" 2>/dev/null && { echo "# $dir/$id.jpg"; continue; }
  if [ -n "$sim" ]; then echo "# $id.jpg"; else echo x > "$dir/$id.jpg"; echo "$dir/$id.jpg"; fi
  echo "g $id" >> "$arch"
done
`

// fakeEngines puts fake yt-dlp and gallery-dl first on PATH and returns a
// queue and subscriptions that use them.
func fakeEngines(t *testing.T) (*Config, *Queue, *Subs) {
	if runtime.GOOS != "linux" {
		t.Skip("uses shell scripts as fake engines")
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(fakeYTDLP), 0o755)
	os.WriteFile(filepath.Join(bin, "gallery-dl"), []byte(fakeGalleryDL), 0o755)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "args.log"))
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	cfg.Library = t.TempDir()
	eng := NewEngineManager(cfg)
	hub := NewHub()
	q := NewQueue(cfg, eng, &Router{eng: eng}, NewCookieStore(cfg.Dir("cookies")), hub)
	return cfg, q, NewSubs(cfg, q, hub)
}

// check runs the subscription's due check to the end, as the scheduler would.
func check(t *testing.T, cfg *Config, q *Queue, subs *Subs, id string) Job {
	t.Helper()
	ctx := context.Background()
	subs.tick()
	x := subs.List()[slices.IndexFunc(subs.List(), func(s Subscription) bool { return s.ID == id })]
	j, ok := q.Get(x.LastJob)
	if !ok {
		t.Fatalf("no job queued for %s", id)
	}
	var err error
	if j.Engine == EngGalleryDL {
		err = q.runGalleryDL(ctx, j, cfg.Library)
	} else {
		err = q.runYTDLP(ctx, j, cfg.Library)
	}
	q.finish(ctx, j.ID, err)
	j, _ = q.Get(j.ID)
	subs.jobFinished(j)
	return j
}

func libraryFiles(t *testing.T, dir string) []string {
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, filepath.Base(p))
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func lastArgs(t *testing.T) string {
	b, _ := os.ReadFile(os.Getenv("FAKE_LOG"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

func TestSubscribeFromNowYTDLP(t *testing.T) {
	cfg, q, subs := fakeEngines(t)
	t.Setenv("FAKE_IDS", "v5 v4 v3 v2 v1 v0") // newest first
	x, err := subs.Add(NewSub{URL: "https://www.youtube.com/@blender/videos", Engine: EngYTDLP, Title: "Blender",
		Opts: JobOptions{Limit: 5}, FromNow: true})
	if err != nil {
		t.Fatal(err)
	}
	if !x.MarkPending {
		t.Fatalf("from_now not kept: %+v", x)
	}

	// First check: the newest 5 are marked, nothing is downloaded.
	j := check(t, cfg, q, subs, x.ID)
	if !j.Opts.Mark || j.Marked != 5 || len(j.Files) != 0 || j.Status != StDone || j.Note != "marked" {
		t.Fatalf("marking check: %+v", j)
	}
	if a := lastArgs(t); !strings.Contains(a, "--skip-download") || !strings.Contains(a, "--force-write-archive") {
		t.Fatalf("marking args: %s", a)
	}
	if f := libraryFiles(t, cfg.Library); len(f) != 0 {
		t.Fatalf("marking downloaded %v", f)
	}
	got := subs.List()[0]
	if got.MarkPending || got.LastMarked != 5 || got.LastNew != 0 || got.TotalNew != 0 {
		t.Fatalf("after marking: %+v", got)
	}

	// "Check now" with nothing new: nothing is downloaded or marked.
	subs.CheckNow(x.ID)
	j = check(t, cfg, q, subs, x.ID)
	if j.Opts.Mark || len(j.Files) != 0 || j.Note != "no_new" {
		t.Fatalf("check with nothing new: %+v", j)
	}
	if a := lastArgs(t); strings.Contains(a, "--skip-download") {
		t.Fatalf("second check marks again: %s", a)
	}

	// Two new videos: only they are downloaded and counted.
	t.Setenv("FAKE_IDS", "v7 v6 v5 v4 v3 v2 v1 v0")
	subs.CheckNow(x.ID)
	j = check(t, cfg, q, subs, x.ID)
	if f := libraryFiles(t, cfg.Library); !slices.Equal(f, []string{"v6.mp4", "v7.mp4"}) {
		t.Fatalf("downloaded %v, want only v6 and v7", f)
	}
	got = subs.List()[0]
	if got.LastNew != 2 || got.TotalNew != 2 || got.LastMarked != 0 {
		t.Fatalf("after new videos: %+v", got)
	}

	// Survives a restart.
	again := NewSubs(cfg, q, NewHub())
	if l := again.List(); l[0].TotalNew != 2 || l[0].MarkPending {
		t.Fatalf("not persisted: %+v", l[0])
	}
}

func TestSubscribeFromNowGalleryDL(t *testing.T) {
	cfg, q, subs := fakeEngines(t)
	t.Setenv("FAKE_IDS", "p4 p3 p2 p1")
	x, _ := subs.Add(NewSub{URL: "https://example.com/gallery/x", Engine: EngGalleryDL, Title: "Galeri",
		Opts: JobOptions{Limit: 5}, FromNow: true})
	j := check(t, cfg, q, subs, x.ID)
	if j.Marked != 4 || len(j.Files) != 0 || len(j.Existing) != 0 || j.Note != "marked" {
		t.Fatalf("marking check: %+v", j)
	}
	if a := lastArgs(t); !strings.Contains(a, "-s ") || !strings.Contains(a, "-o archive-event=skip") {
		t.Fatalf("marking args: %s", a)
	}
	if f := libraryFiles(t, cfg.Library); len(f) != 0 {
		t.Fatalf("marking downloaded %v", f)
	}

	t.Setenv("FAKE_IDS", "p5 p4 p3 p2 p1")
	subs.CheckNow(x.ID)
	check(t, cfg, q, subs, x.ID)
	if f := libraryFiles(t, cfg.Library); !slices.Equal(f, []string{"p5.jpg"}) {
		t.Fatalf("downloaded %v, want only p5", f)
	}
	if got := subs.List()[0]; got.LastNew != 1 || got.TotalNew != 1 || got.LastMarked != 0 {
		t.Fatalf("counters: %+v", got)
	}
}

// The dialog's values reach the subscription; without them, today's defaults.
func TestSubscribeAPIOptions(t *testing.T) {
	s, h := testServer(t, "never", "127.0.0.1:8899")
	post := func(body string) {
		req := httptest.NewRequest("POST", "/api/subs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	post(`{"url":"https://www.youtube.com/@a/videos","engine":"yt-dlp","title":"A","interval_h":24,"opts":{"limit":25},"from_now":true}`)
	post(`{"url":"https://www.youtube.com/@b/videos","engine":"yt-dlp","title":"B"}`)
	post(`{"url":"https://www.instagram.com/blender/","engine":"gallery-dl","title":"@blender","interval_h":168,"opts":{"sections":["posts","reels"],"limit":50},"from_now":true}`)
	by := map[string]Subscription{}
	for _, x := range s.subs.List() {
		by[x.Title] = x
	}
	if a := by["A"]; a.IntervalH != 24 || a.Opts.Limit != 25 || !a.MarkPending {
		t.Fatalf("dialog values: %+v", a)
	}
	if b := by["B"]; b.IntervalH != 6 || b.Opts.Limit != 10 || b.MarkPending {
		t.Fatalf("defaults: %+v", b)
	}
	ig := by["@blender"]
	if ig.IntervalH != 168 || ig.Opts.Limit != 50 || !ig.MarkPending || !slices.Equal(ig.Opts.Sections, []string{"posts", "reels"}) {
		t.Fatalf("profile: %+v", ig)
	}

	s.subs.tick()
	for _, j := range s.queue.jobs {
		x := s.subs.List()[slices.IndexFunc(s.subs.List(), func(x Subscription) bool { return x.ID == j.Sub })]
		if j.Opts.Mark != x.MarkPending {
			t.Errorf("%s: job mark=%v, subscription from_now=%v", x.Title, j.Opts.Mark, x.MarkPending)
		}
		if x.Title == "@blender" {
			g := strings.Join(gallerydlArgs(j.Opts, gallerydlRun{Dir: "/l", Archive: "/a", URLs: []string{"u"}}), " ")
			if !strings.Contains(g, "--post-range 1-50") || !strings.Contains(g, "-s ") || !slices.Equal(j.Opts.Sections, []string{"posts", "reels"}) {
				t.Errorf("profile marking job: %+v %s", j.Opts, g)
			}
		}
	}
}
