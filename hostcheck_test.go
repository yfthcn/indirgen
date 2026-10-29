package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A rebinding page (its own name, resolved to 127.0.0.1) must get nothing,
// not even from loopback where no token is asked.
func TestDNSRebindingBlocked(t *testing.T) {
	s, h := testServer(t, "auto", "0.0.0.0:8899")
	s.cfg.AllowedHosts = []string{"indirgen.example"}
	name, _ := os.Hostname()
	get := func(host, path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		req.RemoteAddr = "127.0.0.1:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, bad := range []string{"rebind.attacker.example:8899", "attacker.example", "indirgen.example.attacker.example"} {
		for _, p := range []string{"/api/connect", "/api/telegram", "/", "/api/library/file?path=x"} {
			if c := get(bad, p); c != 421 {
				t.Errorf("%s %s → %d", bad, p, c)
			}
		}
	}
	for _, ok := range []string{"127.0.0.1:8899", "[::1]:8899", "localhost:8899", "192.168.0.10:8899", "10.0.0.1:8899", "[fe80::1%25eth0]:8899",
		"indirgen.example", "INDIRGEN.EXAMPLE:8899", strings.ToLower(name) + ":8899", strings.ToLower(name) + ".local"} {
		if c := get(ok, "/api/connect"); c != 200 {
			t.Errorf("%s refused: %d", ok, c)
		}
	}
	t.Setenv("INDIRGEN_ALLOWED_HOSTS", " nas.lan , ")
	if c := get("nas.lan:8899", "/api/connect"); c != 200 {
		t.Errorf("env host refused: %d", c)
	}
}
