package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Sending links from outside the web UI: the browser extension and the iOS
// Shortcut both POST to /api/add. The body may be JSON or plain text; any
// http(s) links in it are queued with default options, and the engine is
// picked when the job starts (the same probe the UI uses).
// ---------------------------------------------------------------------------

var linkRe = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)

// extractURLs finds up to max distinct http(s) links in free text, such as
// what a share sheet hands over ("Watch this! https://youtu.be/x").
func extractURLs(text string, max int) []string {
	var out []string
	for _, m := range linkRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:!?)]}»”’")
		if u, err := ValidateURL(m); err == nil && !slices.Contains(out, u.String()) {
			out = append(out, u.String())
		}
		if len(out) == max {
			break
		}
	}
	return out
}

type addRequest struct {
	URL    string `json:"url"`
	Text   string `json:"text"`
	Mode   string `json:"mode"`   // library (default) | temp
	Preset string `json:"preset"` // best (default) | compat | mp3 | audio
	Wait   bool   `json:"wait"`   // answer with the downloaded file ("Telefona indir")
	Force  bool   `json:"force"`  // download even if it was downloaded before
	Night  bool   `json:"night"`  // start in the night window
}

type addResponse struct {
	Added    int            `json:"added"`
	Message  string         `json:"message"` // one line for a notification
	Jobs     []JobView      `json:"jobs"`
	Existing []ExistingView `json:"existing,omitempty"` // links not queued: downloaded before / in the queue
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, 400, "istek okunamadı")
		return
	}
	var req addRequest
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, 400, "geçersiz JSON")
			return
		}
	} else {
		req.Text = string(raw) // text/plain, or a Shortcut sending "Text"
	}
	qv := r.URL.Query()
	req.Mode = cmp.Or(req.Mode, qv.Get("mode"))
	req.Preset = cmp.Or(req.Preset, qv.Get("preset"))
	req.Wait = req.Wait || qv.Get("wait") == "1"
	req.Force = req.Force || qv.Get("force") == "1"
	req.Night = req.Night || qv.Get("night") == "1"
	links := extractURLs(req.URL+"\n"+req.Text, 20)
	if len(links) == 0 {
		writeErr(w, 400, "gönderilen metinde bağlantı yok")
		return
	}
	for i, l := range links { // share links → the video's own link, before "already downloaded?"
		links[i] = resolveShortLink(r.Context(), l)
	}
	if req.Wait {
		s.addAndWait(w, r, links[0], req)
		return
	}
	res := addResponse{Jobs: []JobView{}}
	for _, link := range links {
		if !req.Force {
			if ex, ok := FindExisting(s.queue, s.lib, link); ok {
				res.Existing = append(res.Existing, ex.View(s.lib))
				continue
			}
		}
		v, err := s.queue.Add(NewJob{URL: link, Opts: JobOptions{Preset: cmp.Or(req.Preset, "best"), Embed: true, Mode: req.Mode, Night: req.Night}, Source: "api"})
		if err != nil {
			continue
		}
		res.Jobs = append(res.Jobs, v)
	}
	res.Added = len(res.Jobs)
	var parts []string
	switch res.Added {
	case 0:
	case 1:
		parts = append(parts, "Kuyruğa alındı: "+res.Jobs[0].Host)
		if req.Night {
			parts[0] = "Gece indirilecek: " + res.Jobs[0].Host
		}
	default:
		parts = append(parts, strconv.Itoa(res.Added)+" bağlantı kuyruğa alındı")
	}
	switch {
	case len(res.Existing) == 1 && res.Added == 0 && res.Existing[0].Active:
		parts = append(parts, "Zaten kuyrukta: "+clip(res.Existing[0].Title, 80))
	case len(res.Existing) == 1 && res.Added == 0:
		parts = append(parts, "Zaten indirilmiş: "+clip(res.Existing[0].Title, 80))
	case len(res.Existing) > 0:
		parts = append(parts, strconv.Itoa(len(res.Existing))+" bağlantı zaten indirilmiş")
	}
	if len(parts) == 0 {
		writeErr(w, 400, "bağlantı eklenemedi")
		return
	}
	res.Message = strings.Join(parts, " · ")
	writeJSON(w, 200, res)
}

// addAndWait downloads one link and answers with its (first) file, so an
// iOS Shortcut can save it to Photos. Defaults suit a phone: "compat"
// (H.264/AAC MP4, the only video Photos accepts) and browser mode, so the
// copy on the computer is cleaned up after temp_retention_hours.
func (s *Server) addAndWait(w http.ResponseWriter, r *http.Request, link string, req addRequest) {
	jobID := ""
	if !req.Force {
		if ex, ok := FindExisting(s.queue, s.lib, link); ok {
			switch {
			case ex.Active:
				jobID = ex.JobID // already downloading: wait for that one
			case len(ex.Files) > 0 && phoneReady(ex.Files[0]):
				w.Header().Set("X-Indirgen-Existing", "1") // downloaded before: no new download
				if s.serveFile(w, r, ex.Files[0], len(ex.Files)) {
					return
				}
				w.Header().Del("X-Indirgen-Existing")
			}
			// Otherwise (e.g. an MKV Photos cannot take) download a phone copy.
		}
	}
	if jobID == "" {
		mode := cmp.Or(req.Mode, "temp")
		v, err := s.queue.Add(NewJob{URL: link, Opts: JobOptions{Preset: cmp.Or(req.Preset, "compat"), Embed: true, Mode: mode}, Source: "api"})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		jobID = v.ID
	}
	v := struct{ ID string }{jobID}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(20 * time.Minute)
	var j Job
	for {
		select {
		case <-r.Context().Done():
			return // the phone gave up; the download still finishes on the computer
		case <-deadline:
			writeErr(w, 504, "indirme 20 dakikada bitmedi, İndirgen'de devam ediyor")
			return
		case <-tick.C:
		}
		var ok bool
		if j, ok = s.queue.Get(v.ID); !ok {
			writeErr(w, 410, "iş İndirgen'de silindi")
			return
		}
		if j.finished() {
			break
		}
	}
	if j.Status != StDone {
		writeErr(w, 422, cmp.Or(firstLine(j.Error), "indirme iptal edildi"))
		return
	}
	files := j.Deliverable()
	for _, f := range files {
		if s.serveFile(w, r, f, len(files)) {
			return
		}
	}
	writeErr(w, 422, "indirme bitti ama dosya bulunamadı")
}

// serveFile answers with one downloaded file; false if it is not servable
// (nothing has been written then).
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, f string, count int) bool {
	if !s.queue.allowedPath(f) {
		return false
	}
	fh, err := os.Open(f)
	if err != nil {
		return false
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	w.Header().Set("X-Indirgen-Files", strconv.Itoa(count))
	serveContent(w, r, fh, st, filepath.Base(f), true)
	return true
}

// ---- reachability ---------------------------------------------------------

type ConnectInfo struct {
	Listen  string   `json:"listen"`
	LAN     bool     `json:"lan"`      // reachable from other devices
	URLs    []string `json:"urls"`     // addresses to use from another device
	Token   string   `json:"token"`    // needed by the Shortcut / extension on the network
	CanEdit bool     `json:"can_edit"` // the UI may switch LAN access (not in Docker)
	Docker  bool     `json:"docker"`
}

func (s *Server) connectInfo() ConnectInfo {
	host, port, _ := net.SplitHostPort(s.cfg.Listen)
	ci := ConnectInfo{Listen: s.cfg.Listen, Token: s.cfg.AccessToken(), Docker: inDocker(), URLs: []string{}}
	ci.CanEdit = !ci.Docker
	ip := net.ParseIP(host)
	switch {
	case host == "localhost" || (ip != nil && ip.IsLoopback()):
		ci.URLs = append(ci.URLs, "http://"+net.JoinHostPort("127.0.0.1", port))
	case host == "" || (ip != nil && ip.IsUnspecified()):
		ci.LAN = true
		ci.URLs = lanURLs(port)
	default:
		ci.LAN = true
		ci.URLs = append(ci.URLs, "http://"+net.JoinHostPort(host, port))
	}
	return ci
}

// lanURLs lists the addresses other devices reach this machine on: private
// IPv4 (home Wi-Fi, LAN, WireGuard) and 100.64.0.0/10 (Tailscale). The
// interface of the default route comes first. Container and VM bridges
// (Docker, libvirt, Podman, CNI) are left out, unless nothing else is left.
// The startup summary, the network access panel and the pairing QR codes
// all use this list.
func lanURLs(port string) []string {
	def := defaultRouteIface()
	var first, rest, bridges []string
	for _, ifc := range listIfaces() {
		for _, ip := range ifc.Addrs {
			if ip.To4() == nil || !(ip.IsPrivate() || cgnat.Contains(ip)) {
				continue
			}
			u := "http://" + net.JoinHostPort(ip.String(), port)
			switch {
			case virtualIface(ifc.Name):
				bridges = append(bridges, u)
			case ifc.Name == def:
				first = append(first, u)
			default:
				rest = append(rest, u)
			}
		}
	}
	if len(first)+len(rest) == 0 {
		return append([]string{}, bridges...)
	}
	return append(first, rest...)
}

// cgnat is the shared address space Tailscale gives its devices.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// virtualIface: bridges and links of containers and virtual machines, which
// a phone cannot reach.
func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "vnet", "cni", "flannel", "podman"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

type netIface struct {
	Name  string
	Addrs []net.IP
}

// listIfaces and defaultRouteIface are variables so tests can set them.
var listIfaces = func() []netIface {
	ifs, _ := net.Interfaces()
	out := make([]netIface, 0, len(ifs))
	for _, ifc := range ifs {
		f := netIface{Name: ifc.Name}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				f.Addrs = append(f.Addrs, n.IP)
			}
		}
		out = append(out, f)
	}
	return out
}

var defaultRouteIface = func() string {
	f, err := os.Open("/proc/net/route") // Linux; elsewhere no order is preferred
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseDefaultRoute(f)
}

// parseDefaultRoute reads /proc/net/route: the up route to 0.0.0.0/0 with
// the lowest metric.
func parseDefaultRoute(r io.Reader) string {
	best, bestMetric := "", -1
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		metric, err2 := strconv.Atoi(f[6])
		if err1 != nil || err2 != nil || flags&1 == 0 { // RTF_UP
			continue
		}
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = f[0], metric
		}
	}
	return best
}

// setLAN opens or closes access from other devices: rewrites "listen" in
// config.json (same port) and restarts. Docker controls its own listen flag.
func (s *Server) setLAN(on bool) error {
	if inDocker() {
		return errors.New("Docker'da erişim compose.yaml'daki ports ile ayarlanır")
	}
	_, port, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return err
	}
	host := "127.0.0.1"
	if on {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, port)
	if err := s.cfg.SetListen(addr); err != nil {
		return fmt.Errorf("config.json yazılamadı: %w", err)
	}
	return nil
}

// isLocal: the request comes from this machine and not through a proxy.
func isLocal(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Cf-Connecting-Ip") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
