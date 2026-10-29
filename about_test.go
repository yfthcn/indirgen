package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.3.0", "0.2.1", true}, {"0.2.1", "0.2.1", false}, {"0.2.0", "0.2.1", false},
		{"1.0.0", "0.99.99", true}, {"v0.2.2", "0.2.1", true}, {"0.2.1", "0.2.1-dev", true},
		{"0.2.1-dev", "0.2.1", false}, {"garbage", "0.2.1", false}, {"0.2", "0.1.0", false},
	}
	for _, c := range cases {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestParseChangelog(t *testing.T) {
	md := "# Changelog\n\n## 0.2.1 · 2026-10-26\n\n### Fixed\n- one\n- two\n\n## 0.2.0 · 2026-10-25\n\nText.\n\n- a\n\n## 0.1.0 · 2026-10-01\n- old\n"
	got := parseChangelog(md, 2, 8)
	if len(got) != 2 || got[0].Version != "0.2.1" || got[0].Date != "2026-10-26" ||
		len(got[0].Items) != 2 || got[1].Version != "0.2.0" || got[1].Items[0] != "a" {
		t.Fatalf("%+v", got)
	}
	// the embedded CHANGELOG starts with the running version
	if e := parseChangelog(changelogRaw, 1, 8); len(e) != 1 || e[0].Version != version {
		t.Fatalf("CHANGELOG top %+v, version %s", e, version)
	}
}

func TestUpdaterCheck(t *testing.T) {
	var status int
	var body string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+project.Repo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer gh.Close()
	u := NewUpdater(gh.URL)

	status, body = 404, `{"message":"Not Found"}`
	if s := u.Check(context.Background()); s.Status != UpdUnpublished {
		t.Fatalf("404: %+v", s)
	}
	status, body = 200, `{"tag_name":"v99.0.0","html_url":"https://github.com/x/releases/v99.0.0"}`
	if s := u.Check(context.Background()); s.Status != UpdAvailable || s.Latest != "99.0.0" || s.URL == "" {
		t.Fatalf("newer: %+v", s)
	}
	status, body = 200, `{"tag_name":"v`+version+`"}`
	if s := u.Check(context.Background()); s.Status != UpdCurrent {
		t.Fatalf("same: %+v", s)
	}
	status, body = 500, `boom`
	if s := u.Check(context.Background()); s.Status != UpdError || s.Error == "" {
		t.Fatalf("error: %+v", s)
	}
}
