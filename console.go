package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// Terminal output.
//
// Started by hand (a terminal): a short summary of where İndirgen runs, then
// aligned, colored log lines ("12:25:03  BİLGİ  indirildi  site=youtube.com").
// Under systemd or Docker (no terminal): plain key=value lines for the
// journal; journald stamps them itself, so the time is left out there.
// The access token is never logged; it is shown once, when it is created.
// ---------------------------------------------------------------------------

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// palette holds ANSI codes, or nothing when colors are off (NO_COLOR, pipe).
type palette struct{ dim, bold, reset, info, warn, err, debug, ok, accent string }

func newPalette(color bool) palette {
	if !color {
		return palette{}
	}
	return palette{dim: "\x1b[2m", bold: "\x1b[1m", reset: "\x1b[0m", info: "\x1b[36m",
		warn: "\x1b[33m", err: "\x1b[31m", debug: "\x1b[35m", ok: "\x1b[32m", accent: "\x1b[33m"}
}

func colorWanted(f *os.File) bool {
	return isTerminal(f) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// setupLogging picks the handler for where the output goes.
func setupLogging(level slog.Level) {
	var h slog.Handler
	switch {
	case isTerminal(os.Stderr):
		h = &consoleHandler{w: os.Stderr, level: level, p: newPalette(colorWanted(os.Stderr)), mu: &sync.Mutex{}}
	default:
		opts := &slog.HandlerOptions{Level: level}
		if os.Getenv("JOURNAL_STREAM") != "" { // systemd: the journal has the time
			opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
				if len(groups) == 0 && a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			}
		}
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

type consoleHandler struct {
	w     io.Writer
	level slog.Level
	p     palette
	attrs []slog.Attr
	mu    *sync.Mutex
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *consoleHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &c
}

func (h *consoleHandler) WithGroup(string) slog.Handler { return h }

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	p := h.p
	lv, col := "BİLGİ", p.info
	switch {
	case r.Level >= slog.LevelError:
		lv, col = "HATA ", p.err
	case r.Level >= slog.LevelWarn:
		lv, col = "UYARI", p.warn
	case r.Level < slog.LevelInfo:
		lv, col = "AYRNT", p.debug
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s%s  %s%s%s  ", p.dim, r.Time.Format("15:04:05"), p.reset, col, lv, p.reset)
	msg := r.Message
	if r.Level >= slog.LevelWarn {
		msg = col + msg + p.reset
	}
	b.WriteString(msg)
	write := func(a slog.Attr) {
		v := a.Value.Resolve()
		if a.Key == "" || (v.Kind() == slog.KindString && v.String() == "") {
			return
		}
		val := v.String()
		if r := []rune(val); len(r) > 160 { // one line per event, even for long engine errors
			val = string(r[:159]) + "…"
		}
		fmt.Fprintf(&b, "  %s%s=%s%s", p.dim, a.Key, p.reset, quoteValue(val))
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(func(a slog.Attr) bool { write(a); return true })
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

// quoteValue quotes values with spaces so the line stays readable.
func quoteValue(s string) string {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '"' || r == '=' }) {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// ---- startup summary --------------------------------------------------------

type startInfo struct {
	Addr     net.Addr
	Library  string
	Data     string
	Auth     bool
	NewToken string // set only on the very first start
	Engines  []EngineState
	Fetching bool // a managed engine is being installed over a system or missing one
	Telegram TelegramConfig
}

// serveURLs: the address to open on this computer, and the ones other
// devices use when İndirgen listens on the network.
func serveURLs(addr net.Addr) (local string, lan []string) {
	host, port, _ := net.SplitHostPort(addr.String())
	ip := net.ParseIP(host)
	switch {
	case ip == nil || ip.IsUnspecified():
		return "http://127.0.0.1:" + port, lanURLs(port)
	case ip.IsLoopback():
		return "http://" + net.JoinHostPort(host, port), nil
	default:
		return "http://" + net.JoinHostPort(host, port), nil
	}
}

// printBanner writes the summary for a person at a terminal.
func printBanner(w io.Writer, p palette, si startInfo) {
	local, lan := serveURLs(si.Addr)
	rule := p.dim + strings.Repeat("─", 58) + p.reset
	row := func(label, value string) { fmt.Fprintf(w, "  %s%-10s%s %s\n", p.dim, label, p.reset, value) }

	fmt.Fprintf(w, "\n  %s%s↓ İndirgen%s %s%s%s  %syapıştır, indir%s\n", p.bold, p.accent, p.reset, p.bold, version, p.reset, p.dim, p.reset)
	fmt.Fprintln(w, rule)
	row("Arayüz", p.bold+local+p.reset)
	for _, u := range lan {
		row("", u+p.dim+"  (ağdaki cihazlar)"+p.reset)
	}
	free := ""
	if f := diskFree(si.Library); f >= 0 {
		free = p.dim + "  (" + humanBytes(f) + " boş)" + p.reset
	}
	row("Arşiv", si.Library+free)
	row("Veri", si.Data)
	switch {
	case !si.Auth:
		row("Erişim", "sadece bu bilgisayar, token gerekmez")
	case si.NewToken != "":
		row("Erişim", "token gerekli")
		row("Token", p.bold+si.NewToken+p.reset)
		row("", p.dim+"yalnızca bu ilk açılışta gösterilir; sonra: indirgen token"+p.reset)
	default:
		row("Erişim", "token gerekli "+p.dim+"(göster: indirgen token)"+p.reset)
	}
	var eng []string
	for _, e := range si.Engines {
		switch {
		case e.Available && e.Version != "" && e.Source == "system":
			eng = append(eng, e.Name+" "+p.dim+shortVersion(e.Version)+" (sistem)"+p.reset)
		case e.Available && e.Version != "":
			eng = append(eng, e.Name+" "+p.dim+shortVersion(e.Version)+p.reset)
		case e.Required:
			eng = append(eng, p.warn+e.Name+" yok"+p.reset)
		}
	}
	if len(eng) > 0 {
		row("Motorlar", strings.Join(eng, "  "))
	}
	if si.Fetching {
		row("", p.dim+"en son sürümler indiriliyor; hazır olunca aşağıda \"motor hazır\" yazar"+p.reset)
	}
	if si.Telegram.Token != "" {
		tg := fmt.Sprintf("açık · %d sohbet", len(si.Telegram.Chats))
		if len(si.Telegram.Chats) == 0 {
			tg = "açık · eşlenmiş sohbet yok"
		}
		row("Telegram", tg)
	}
	fmt.Fprintln(w, rule)
	fmt.Fprintf(w, "  %sDurdurmak için Ctrl+C%s\n\n", p.dim, p.reset)
}

// shortVersion: "n9.0 (2026-10-26)" → "n9.0".
func shortVersion(v string) string {
	if i := strings.Index(v, " ("); i > 0 {
		return v[:i]
	}
	return v
}

// logStart is the one-line equivalent for the journal.
func logStart(si startInfo) {
	local, lan := serveURLs(si.Addr)
	args := []any{"sürüm", version, "adres", local}
	if len(lan) > 0 {
		args = append(args, "ağ", strings.Join(lan, ","))
	}
	args = append(args, "arşiv", si.Library, "veri", si.Data, "token_gerekli", si.Auth)
	slog.Info("İndirgen hazır", args...)
	if si.NewToken != "" {
		slog.Info("erişim token'ı oluşturuldu; görmek için: indirgen token")
	}
}

// friendlyListenError explains the usual reason a start fails.
func friendlyListenError(addr string, err error) error {
	if strings.Contains(err.Error(), "address already in use") {
		_, port, _ := net.SplitHostPort(addr)
		return fmt.Errorf("%s portu kullanımda. İndirgen zaten çalışıyor olabilir: tarayıcıda http://127.0.0.1:%s adresine bak, "+
			"durumu görmek için: indirgen status", port, port)
	}
	return err
}

func durationText(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1 sn"
	case d < time.Minute:
		return fmt.Sprintf("%d sn", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d dk %d sn", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d sa %d dk", int(d.Hours()), int(d.Minutes())%60)
}
