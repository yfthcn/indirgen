package main

import (
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// DNS rebinding guard.
//
// A web page on attacker.example can make its own name resolve to 127.0.0.1;
// the browser then sends that page's requests to İndirgen and treats the
// answers as same-origin, and they arrive from loopback, where no token is
// asked. Such requests carry the attacker's name in Host. So İndirgen only
// answers to IP addresses (a name is needed to rebind), "localhost", this
// machine's own name, and names listed in allowed_hosts (a reverse proxy or
// a VPN DNS name, e.g. "indirgen.example").
// ---------------------------------------------------------------------------

func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if i := strings.IndexByte(host, '%'); i > 0 && net.ParseIP(host[:i]) != nil {
		host = host[:i] // fe80::1%eth0: a link-local address with its zone
	}
	if host == "" { // HTTP/1.0 without Host: not a browser
		return true
	}
	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if name, err := os.Hostname(); err == nil {
		name = strings.ToLower(name)
		if host == name || host == name+".local" || host == name+".lan" {
			return true
		}
	}
	return slices.Contains(s.cfg.AllowedHostList(), host)
}

func (s *Server) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeErr(w, http.StatusMisdirectedRequest,
				"bu adres İndirgen'e tanıtılmamış: "+r.Host+" (config.json → allowed_hosts listesine ekle ya da IP adresiyle aç)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AllowedHostList: allowed_hosts from config.json plus INDIRGEN_ALLOWED_HOSTS
// (comma separated, handy in Docker).
func (c *Config) AllowedHostList() []string {
	c.mu.RLock()
	list := append([]string(nil), c.AllowedHosts...)
	c.mu.RUnlock()
	for h := range strings.SplitSeq(os.Getenv("INDIRGEN_ALLOWED_HOSTS"), ",") {
		list = append(list, h)
	}
	out := list[:0]
	for _, h := range list {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, strings.TrimSuffix(h, "."))
		}
	}
	return out
}
