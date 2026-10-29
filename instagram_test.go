package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseInstagramProfile(t *testing.T) {
	cases := []struct{ in, user, sec string }{
		{"https://www.instagram.com/losev1998/", "losev1998", ""},
		{"https://instagram.com/ist_class?igsh=abc", "ist_class", ""},
		{"https://www.instagram.com/losev1998/reels/", "losev1998", "reels"},
		{"https://www.instagram.com/losev1998/tagged", "losev1998", "tagged"},
		{"https://www.instagram.com/me.self/saved/", "me.self", "saved"},
		{"https://www.instagram.com/stories/losev1998/", "losev1998", "stories"},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.in)
		user, sec, ok := parseInstagramProfile(u)
		if !ok || user != c.user || sec != c.sec {
			t.Errorf("%s → %q %q %v", c.in, user, sec, ok)
		}
	}
	for _, bad := range []string{
		"https://www.instagram.com/p/DeHgy0lDA0h/", "https://www.instagram.com/reel/abc123/",
		"https://www.instagram.com/explore/tags/x/", "https://www.instagram.com/accounts/login/",
		"https://www.instagram.com/stories/highlights/123/", "https://x.com/someone",
		"https://www.instagram.com/user/saved/coll/123/",
	} {
		u, _ := url.Parse(bad)
		if _, _, ok := parseInstagramProfile(u); ok {
			t.Errorf("accepted %s", bad)
		}
	}
}

// Shape taken from gallery-dl's DataJob: [2, post] then [3, url, file] per media.
const browseSample = `[
 [2, {"post_shortcode":"AAA111","post_url":"https://www.instagram.com/p/AAA111/","count":3,"likes":12,"description":"Ankara\nkonseri","date":"2026-10-01 10:00:00"}],
 [3, "https://scontent.cdninstagram.com/a1.jpg", {"display_url":"https://scontent.cdninstagram.com/a1.jpg","video_url":null}],
 [3, "https://scontent.cdninstagram.com/a2.jpg", {"display_url":"https://scontent.cdninstagram.com/a2.jpg","video_url":null}],
 [3, "https://scontent.cdninstagram.com/a3.mp4", {"display_url":"https://scontent.cdninstagram.com/a3.jpg","video_url":"https://scontent.cdninstagram.com/a3.mp4"}],
 [2, {"post_shortcode":"BBB222","post_url":"https://www.instagram.com/reel/BBB222/","count":1}],
 [3, "https://scontent.cdninstagram.com/b.mp4", {"display_url":"https://scontent.cdninstagram.com/b.jpg","video_url":"https://scontent.cdninstagram.com/b.mp4"}],
 [2, {"post_shortcode":"CCC333","count":1}],
 [3, "https://scontent.cdninstagram.com/c.jpg", {"display_url":"https://scontent.cdninstagram.com/c.jpg"}]
]`

func TestParseBrowse(t *testing.T) {
	items, errMsg := parseBrowse([]byte(browseSample))
	if errMsg != "" || len(items) != 3 {
		t.Fatalf("%v %q", items, errMsg)
	}
	if items[0].Type != "carousel" || items[0].Count != 3 || items[0].Thumb != "https://scontent.cdninstagram.com/a1.jpg" || items[0].Caption != "Ankara konseri" {
		t.Fatalf("carousel: %+v", items[0])
	}
	if items[1].Type != "video" || items[1].URL != "https://www.instagram.com/reel/BBB222/" {
		t.Fatalf("reel: %+v", items[1])
	}
	if items[2].Type != "image" || items[2].URL != "https://www.instagram.com/p/CCC333/" {
		t.Fatalf("image: %+v", items[2])
	}
	_, e := parseBrowse([]byte(`[[-1, {"error":"AbortExtraction","message":"HTTP redirect to login page"}]]`))
	if !strings.Contains(e, "redirect to login") {
		t.Fatal(e)
	}
}

func TestProfileJobArgs(t *testing.T) {
	o := JobOptions{Sections: []string{"reels", "bogus", "posts"}, NewOnly: true,
		Items: []string{"https://www.instagram.com/p/AAA111/", "https://evil.com/p/x/", "--exec x"}}
	o.normalize()
	if strings.Join(o.Sections, ",") != "posts,reels" || len(o.Items) != 1 {
		t.Fatalf("normalize: %+v", o)
	}
	pace := pacePresets["careful"]
	pace.MaxItems = 50
	a := strings.Join(gallerydlArgs(o, gallerydlRun{Dir: "/lib", Pace: pace, Archive: "/a.sqlite3",
		URLs: o.Items}), " ")
	for _, want := range []string{"--sleep-request 10-20", "--sleep 2-6", "--download-archive /a.sqlite3", "-- https://www.instagram.com/p/AAA111/"} {
		if !strings.Contains(a, want) {
			t.Errorf("missing %q in %s", want, a)
		}
	}
	if strings.Contains(a, "--post-range") {
		t.Error("post-range must not limit a hand-picked selection")
	}
	o.Items = nil
	a = strings.Join(gallerydlArgs(o, gallerydlRun{Dir: "/lib", Pace: pace, URLs: []string{"u"}}), " ")
	if !strings.Contains(a, "--post-range 1-50") {
		t.Error("max items not applied to section download")
	}
}

func TestPacingConfig(t *testing.T) {
	cfg := &Config{dataDir: t.TempDir()}
	cfg.applyDefaults()
	if p := cfg.Pacing(); p.Request != "6-12" || p.Pace != "normal" {
		t.Fatalf("%+v", p)
	}
	if err := cfg.SetPacing(Pacing{Pace: "custom", Request: "abc", File: "1"}); err == nil {
		t.Fatal("bad duration accepted")
	}
	if err := cfg.SetPacing(Pacing{Pace: "custom", Request: "15-30", File: "3", MaxItems: 100}); err != nil {
		t.Fatal(err)
	}
	if p := cfg.Pacing(); p.Request != "15-30" || p.File != "3" || p.MaxItems != 100 {
		t.Fatalf("%+v", p)
	}
}
