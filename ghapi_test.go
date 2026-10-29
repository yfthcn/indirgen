package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a settable now() for the API client.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestAPIClient(file string, clk *fakeClock) *apiClient {
	c := newAPIClient(file)
	c.now = clk.now
	return c
}

func TestAPIRateLimitRecognised(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	reset := clk.t.Add(37 * time.Minute)
	cases := []struct {
		name  string
		code  int
		hdr   map[string]string
		body  string
		until time.Time // zero: not a rate limit
	}{
		{"403 remaining 0", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, `{"message":"API rate limit exceeded for 1.2.3.4."}`, reset},
		{"429 remaining 0", 429, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, "", reset},
		{"403 retry-after", 403, map[string]string{"Retry-After": "120"}, `{"message":"You have exceeded a secondary rate limit."}`, clk.t.Add(2 * time.Minute)},
		{"429 bare", 429, nil, "", clk.t.Add(time.Minute)},
		{"403 secondary, no headers", 403, nil, `{"message":"You have exceeded a secondary rate limit."}`, clk.t.Add(time.Minute)},
		{"403 forbidden", 403, map[string]string{"X-RateLimit-Remaining": "41"}, `{"message":"Resource not accessible"}`, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.hdr {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newTestAPIClient("", clk)
			_, err := c.get(context.Background(), "GitHub", srv.URL+"/repos/a/b/releases/latest", nil)
			var rl *RateLimitError
			if tc.until.IsZero() {
				if errors.As(err, &rl) {
					t.Fatalf("taken for a rate limit: %v", err)
				}
				var se *APIStatusError
				if !errors.As(err, &se) || se.Code != tc.code {
					t.Fatalf("want APIStatusError %d, got %v", tc.code, err)
				}
				return
			}
			if !errors.As(err, &rl) {
				t.Fatalf("want RateLimitError, got %v", err)
			}
			if !rl.Until.Equal(tc.until) {
				t.Fatalf("until = %v, want %v", rl.Until, tc.until)
			}
		})
	}
}

// Until the reset time, no request leaves for that API, whichever path.
func TestAPIRateLimitHoldsRequestsUntilReset(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	reset := clk.t.Add(10 * time.Minute)
	var hits atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if limited.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(403)
			return
		}
		w.Write([]byte(`{"tag_name":"v1"}`))
	}))
	defer srv.Close()
	c := newTestAPIClient("", clk)
	ctx := context.Background()
	if _, err := c.get(ctx, "GitHub", srv.URL+"/repos/a/b/releases/latest", nil); err == nil {
		t.Fatal("want an error")
	}
	for _, p := range []string{"/repos/a/b/releases/latest", "/repos/c/d/releases/latest"} {
		clk.add(4 * time.Minute)
		_, err := c.get(ctx, "GitHub", srv.URL+p, nil)
		var rl *RateLimitError
		if !errors.As(err, &rl) || !rl.Until.Equal(reset) {
			t.Fatalf("%s: want the held RateLimitError, got %v", p, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d requests sent while limited, want 1", n)
	}
	if u := c.blockedUntil(srv.URL + "/x"); !u.Equal(reset) {
		t.Fatalf("blockedUntil = %v, want %v", u, reset)
	}
	limited.Store(false)
	clk.add(3 * time.Minute) // past the reset
	if _, err := c.get(ctx, "GitHub", srv.URL+"/repos/a/b/releases/latest", nil); err != nil {
		t.Fatalf("after the reset: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("hits = %d after the reset, want 2", n)
	}
}

// fakeETagAPI answers 200 with an ETag and Last-Modified, then 304 to a
// request that sends them back.
type fakeETagAPI struct {
	srv               *httptest.Server
	full, notModified atomic.Int32
	sawINM, sawIMS    atomic.Value
}

func newFakeETagAPI(t *testing.T) *fakeETagAPI {
	f := &fakeETagAPI{}
	f.sawINM.Store("")
	f.sawIMS.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sawINM.Store(r.Header.Get("If-None-Match"))
		f.sawIMS.Store(r.Header.Get("If-Modified-Since"))
		if r.Header.Get("If-None-Match") == `W/"abc"` {
			f.notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.full.Add(1)
		w.Header().Set("ETag", `W/"abc"`)
		w.Header().Set("Last-Modified", "Wed, 28 Oct 2026 10:00:00 GMT")
		w.Write([]byte(`{"tag_name":"v2.0.0","assets":[]}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestAPIConditionalRequest304(t *testing.T) {
	f := newFakeETagAPI(t)
	c := newAPIClient(filepath.Join(t.TempDir(), "api-cache.json"))
	ctx, u := context.Background(), f.srv.URL+"/repos/a/b/releases/latest"
	first, err := c.get(ctx, "GitHub", u, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.get(ctx, "GitHub", u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.full.Load() != 1 || f.notModified.Load() != 1 {
		t.Fatalf("full=%d 304=%d, want 1 and 1", f.full.Load(), f.notModified.Load())
	}
	if got := f.sawINM.Load(); got != `W/"abc"` {
		t.Fatalf("If-None-Match = %q", got)
	}
	if got := f.sawIMS.Load(); got != "Wed, 28 Oct 2026 10:00:00 GMT" {
		t.Fatalf("If-Modified-Since = %q", got)
	}
	if string(second) != string(first) || !strings.Contains(string(second), "v2.0.0") {
		t.Fatalf("304 body = %q, want the cached %q", second, first)
	}
}

// The ETags and a running rate-limit hold survive a restart.
func TestAPICacheSurvivesRestart(t *testing.T) {
	f := newFakeETagAPI(t)
	file := filepath.Join(t.TempDir(), "api-cache.json")
	ctx, u := context.Background(), f.srv.URL+"/repos/a/b/releases/latest"
	if _, err := newAPIClient(file).get(ctx, "GitHub", u, nil); err != nil {
		t.Fatal(err)
	}
	body, err := newAPIClient(file).get(ctx, "GitHub", u, nil) // a new process
	if err != nil {
		t.Fatal(err)
	}
	if f.full.Load() != 1 || f.notModified.Load() != 1 || !strings.Contains(string(body), "v2.0.0") {
		t.Fatalf("after a restart: full=%d 304=%d body=%q", f.full.Load(), f.notModified.Load(), body)
	}

	clk := &fakeClock{t: time.Now()}
	reset := clk.t.Add(20 * time.Minute).Truncate(time.Second)
	var hits atomic.Int32
	lim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(429)
	}))
	defer lim.Close()
	newTestAPIClient(file, clk).get(ctx, "GitHub", lim.URL+"/repos/a/b/releases/latest", nil)
	_, err = newTestAPIClient(file, clk).get(ctx, "GitHub", lim.URL+"/repos/a/b/releases/latest", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || !rl.Until.Equal(reset) || hits.Load() != 1 {
		t.Fatalf("hold after a restart: err=%v hits=%d", err, hits.Load())
	}
}

// An API error never carries the response body to the UI.
func TestAPIErrorHidesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(`{"message":"Server Error","documentation_url":"https://docs.github.com/rest"}`))
	}))
	defer srv.Close()
	_, err := newAPIClient("").get(context.Background(), "GitHub", srv.URL+"/repos/a/b/releases/latest", nil)
	if err == nil || strings.Contains(err.Error(), "{") || strings.Contains(err.Error(), "documentation_url") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "GitHub") || !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %q, want the forge and the status", err)
	}
}

// Engines: a rate limit is not an error. The engine stays ready, the state
// says when the next try is, and the log has one line for the whole run.
func TestEngineRateLimitState(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"API rate limit exceeded for 1.2.3.4."}`))
	}))
	defer srv.Close()
	t.Setenv("INDIRGEN_GITHUB_API", srv.URL)
	t.Setenv("INDIRGEN_CODEBERG_API", srv.URL+"/cb")
	cfg := &Config{dataDir: t.TempDir(), AutoUpdate: true}
	cfg.applyDefaults()
	m := NewEngineManager(cfg)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	m.UpdateAll(context.Background(), false)
	m.UpdateAll(context.Background(), false)
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	for _, s := range m.States() {
		if s.Error != "" {
			t.Errorf("%s: error %q, want none", s.Name, s.Error)
		}
		if !s.RateLimitUntil.Equal(reset) {
			t.Errorf("%s: rate_limit_until = %v, want %v", s.Name, s.RateLimitUntil, reset)
		}
	}
	if n := strings.Count(logs.String(), "istek sınırı"); n != 1 {
		t.Fatalf("%d rate-limit log lines, want 1:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "motor güncellenemedi") {
		t.Fatalf("a rate limit logged as a failure:\n%s", logs.String())
	}
	if u := m.RateLimitUntil(); !u.Equal(reset) {
		t.Fatalf("RateLimitUntil = %v", u)
	}
}

// İndirgen's own update check: the last known result stays, the hold is added.
func TestUpdaterRateLimitState(t *testing.T) {
	reset := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	var limited atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		w.Write([]byte(`{"tag_name":"v99.0.0","html_url":"https://example.invalid/r"}`))
	}))
	defer srv.Close()
	u := NewUpdater(srv.URL)
	if st := u.Check(context.Background()); st.Status != UpdAvailable {
		t.Fatalf("first check: %+v", st)
	}
	limited.Store(true)
	st := u.Check(context.Background())
	if st.Status != UpdAvailable || st.Latest != "99.0.0" || st.Error != "" || !st.RateLimitUntil.Equal(reset) {
		t.Fatalf("rate-limited check: %+v", st)
	}
}

// "Check now" during a hold starts nothing and says until when it waits.
func TestEngineUpdateEndpointDuringHold(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("INDIRGEN_GITHUB_API", srv.URL)
	t.Setenv("INDIRGEN_CODEBERG_API", srv.URL+"/cb")
	s, h := testServer(t, "auto", "127.0.0.1:8899")
	s.eng = NewEngineManager(s.cfg)
	until := time.Now().Add(25 * time.Minute).Truncate(time.Second)
	s.eng.gh.data.Blocked[apiHost(s.eng.api)] = until
	req := httptest.NewRequest("POST", "/api/engines/update", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body struct {
		RateLimitUntil time.Time `json:"rate_limit_until"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 || !body.RateLimitUntil.Equal(until) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body)
	}
	time.Sleep(200 * time.Millisecond) // a started check would have asked by now
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests during the hold", n)
	}
}
