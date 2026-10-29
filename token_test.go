package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The token is hex; typed on a phone it may come in capitals.
func TestTokenIgnoresCase(t *testing.T) {
	s, h := testServer(t, "auto", "0.0.0.0:8899")
	up := strings.ToUpper(s.cfg.Token)
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"token":" `+up+` "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || sessionOf(rec) == nil {
		t.Fatalf("login with capitals: %d %s", rec.Code, rec.Body)
	}
	for tok, want := range map[string]int{up: 200, s.cfg.Token: 200, up[:len(up)-1] + "Z": 401, "": 401} {
		req := httptest.NewRequest("GET", "/api/jobs", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("bearer %q: %d, want %d", tok, rec.Code, want)
		}
	}
}

// A token set by hand (not hex) keeps its case.
func TestHandSetTokenKeepsCase(t *testing.T) {
	s, _ := testServer(t, "auto", "0.0.0.0:8899")
	s.cfg.Token = "Gizli-Anahtar-42"
	if !s.tokenMatches("Gizli-Anahtar-42") || s.tokenMatches("gizli-anahtar-42") {
		t.Fatal("hand-set token compared without case")
	}
}
