package main

// The release APIs (GitHub, Codeberg) behind the engine and app update
// checks. Without a token GitHub allows 60 requests an hour per address, so:
// a rate limit is recognised and no request goes to that API until it
// resets, every response is kept with its ETag and asked for again
// conditionally (a 304 reuses it), and both survive a restart.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitError: the API said wait; nothing is sent to it until Until.
type RateLimitError struct {
	Forge string
	Until time.Time
}

func (e *RateLimitError) Error() string {
	return e.Forge + " istek sınırı doldu, " + e.Until.Local().Format("15:04") + "'de tekrar denenecek"
}

// APIStatusError is any other answer than 200 or 304. It carries no body:
// that goes to the log, not to the UI.
type APIStatusError struct {
	Forge, Status string
	Code          int
}

func (e *APIStatusError) Error() string { return fmt.Sprintf("%s API yanıtı: %s", e.Forge, e.Status) }

type apiCached struct {
	ETag         string          `json:"etag,omitempty"`
	LastModified string          `json:"last_modified,omitempty"`
	Body         json.RawMessage `json:"body"`
}

type apiCacheFile struct {
	Responses map[string]apiCached `json:"responses"`
	Blocked   map[string]time.Time `json:"blocked,omitempty"` // API host → no requests before
}

type apiClient struct {
	http *http.Client
	file string // "" keeps the cache in memory only
	now  func() time.Time
	mu   sync.Mutex
	data apiCacheFile
}

func newAPIClient(file string) *apiClient {
	c := &apiClient{http: &http.Client{}, file: file, now: time.Now,
		data: apiCacheFile{Responses: map[string]apiCached{}, Blocked: map[string]time.Time{}}}
	if file != "" {
		if b, err := os.ReadFile(file); err == nil {
			var d apiCacheFile
			if json.Unmarshal(b, &d) == nil {
				if d.Responses != nil {
					c.data.Responses = d.Responses
				}
				if d.Blocked != nil {
					c.data.Blocked = d.Blocked
				}
			}
		}
	}
	return c
}

// apiHost keys a rate limit: GitHub counts every endpoint of an API together.
func apiHost(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return u
}

// blockedUntil is when requests to u's API may start again (zero: now).
func (c *apiClient) blockedUntil(u string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.data.Blocked[apiHost(u)]; c.now().Before(t) {
		return t
	}
	return time.Time{}
}

// get fetches an API URL and returns the body of a 200, or the kept body
// on a 304. hdr adds request headers (Accept, Authorization).
func (c *apiClient) get(ctx context.Context, forge, u string, hdr http.Header) ([]byte, error) {
	if t := c.blockedUntil(u); !t.IsZero() {
		return nil, &RateLimitError{Forge: forge, Until: t}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	maps.Copy(req.Header, hdr)
	req.Header.Set("User-Agent", project.Name+"/"+version)
	c.mu.Lock()
	kept, haveKept := c.data.Responses[u]
	c.mu.Unlock()
	if haveKept {
		if kept.ETag != "" {
			req.Header.Set("If-None-Match", kept.ETag)
		}
		if kept.LastModified != "" {
			req.Header.Set("If-Modified-Since", kept.LastModified)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && haveKept:
		return kept.Body, nil
	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		et, lm := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
		if (et != "" || lm != "") && json.Valid(b) {
			c.mu.Lock()
			c.data.Responses[u] = apiCached{ETag: et, LastModified: lm, Body: compactJSON(b)}
			c.saveLocked()
			c.mu.Unlock()
		}
		return b, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if until, ok := rateLimitReset(resp, b, c.now()); ok {
		c.mu.Lock()
		held := c.now().Before(c.data.Blocked[apiHost(u)]) // a parallel request got here first
		c.data.Blocked[apiHost(u)] = until
		c.saveLocked()
		c.mu.Unlock()
		if !held { // once per hold: later requests stop above without a word
			slog.Warn(forge+" istek sınırı doldu", "tekrar", until.Local().Format("15:04"), "adres", u)
		}
		return nil, &RateLimitError{Forge: forge, Until: until}
	}
	lvl := slog.LevelWarn
	if resp.StatusCode == http.StatusNotFound {
		lvl = slog.LevelDebug // a repo without releases yet: the caller says so
	}
	slog.Log(ctx, lvl, forge+" API hatası", "adres", u, "durum", resp.Status, "yanıt", firstLine(strings.TrimSpace(string(b))))
	return nil, &APIStatusError{Forge: forge, Status: resp.Status, Code: resp.StatusCode}
}

// rateLimitReset tells a rate limit from another 403/429 and says when it
// ends, as GitHub documents it: remaining 0 waits for x-ratelimit-reset,
// retry-after waits that many seconds, a secondary limit without either
// waits a minute.
func rateLimitReset(resp *http.Response, body []byte, now time.Time) (time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(s, 0), true
		}
		return now.Add(time.Minute), true
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil && s >= 0 {
			return now.Add(time.Duration(s) * time.Second), true
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t, true
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests || bytes.Contains(bytes.ToLower(body), []byte("rate limit")) {
		return now.Add(time.Minute), true
	}
	return time.Time{}, false
}

// httpGet fetches a release asset (a binary, a checksum file); anything but
// 200 is an error.
func httpGet(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", project.Name+"/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp, nil
}

func compactJSON(b []byte) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return b
	}
	return buf.Bytes()
}

func (c *apiClient) saveLocked() {
	if c.file == "" {
		return
	}
	now := c.now()
	for h, t := range c.data.Blocked {
		if !now.Before(t) {
			delete(c.data.Blocked, h)
		}
	}
	b, err := json.Marshal(c.data)
	if err == nil {
		err = writeFileAtomic(c.file, b, 0o600)
	}
	if err != nil {
		slog.Warn("API önbelleği yazılamadı", "hata", err)
	}
}
