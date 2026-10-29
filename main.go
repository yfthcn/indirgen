// İndirgen — self-hosted media downloader.
// yt-dlp + gallery-dl behind one router, always on the latest stable engines.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"
)

// version is the release number; build.sh overrides it from the git tag.
var version = "1.0.0"

func usage() {
	fmt.Fprint(os.Stderr, `İndirgen — yapıştır, indir.

Kullanım:
  indirgen install [-no-linger]                                kur: ~/.local/bin, kullanıcı servisi, menü girişi
  indirgen uninstall [-purge [-yes]]                           kaldır (ayarlar -purge ile, indirilenler asla)
  indirgen status                                              kurulum ve çalışma durumu
  indirgen [serve] [-listen addr] [-data dir] [-library dir]   web arayüzünü başlat
  indirgen engines [update|force]                              motor durumu / güncelle
  indirgen token                                               erişim token'ını göster
  indirgen update                                              İndirgen'i en son sürüme güncelle
  indirgen health [-listen addr]                               çalışıyor mu (Docker healthcheck)
  indirgen version

  -listen ve -library sadece o çalıştırma için geçerlidir; kalıcı ayar config.json'dadır.

Ortam: INDIRGEN_DATA, GITHUB_TOKEN (opsiyonel, API limiti için)
`)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	fsg := flag.NewFlagSet("indirgen", flag.ExitOnError)
	fsg.Usage = usage
	dataDir := fsg.String("data", defaultDataDir(), "veri klasörü (config, motorlar, kuyruk)")
	listen := fsg.String("listen", "", "dinleme adresi (ör. 127.0.0.1:8899)")
	library := fsg.String("library", "", "indirme arşivi klasörü")
	debug := fsg.Bool("debug", false, "ayrıntılı log")
	purge := fsg.Bool("purge", false, "uninstall: ayarları da sil")
	yes := fsg.Bool("yes", false, "uninstall --purge: sormadan")
	noLinger := fsg.Bool("no-linger", false, "install: sadece oturum açıkken çalışsın")
	_ = fsg.Parse(args)

	switch cmd {
	case "version", "-v", "--version":
		fmt.Println(versionLine())
		return
	case "help", "-h", "--help":
		usage()
		return
	case "health":
		os.Exit(health(*listen))
	case "serve", "engines", "token", "update", "install", "uninstall", "status":
	default:
		usage()
		os.Exit(2)
	}

	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	setupLogging(lvl)

	dataFlag := false
	fsg.Visit(func(f *flag.Flag) { dataFlag = dataFlag || f.Name == "data" })
	if cmd == "install" && (*listen != "" || *library != "") {
		fatal(errors.New("install: -listen ve -library kalıcı değildir; adresi ve klasörü kurduktan sonra Ayarlar'dan ya da config.json'dan değiştir"))
	}
	if cmd == "uninstall" || cmd == "status" {
		cfg := installedConfig(*dataDir, dataFlag)
		if *listen != "" {
			cfg.Listen = *listen
		}
		if cmd == "status" {
			os.Exit(runStatus(cfg))
		}
		if err := runUninstall(cfg, *purge, *yes); err != nil {
			fatal(err)
		}
		return
	}

	cfg := runConfig(*dataDir, *listen, *library)

	switch cmd {
	case "serve":
		serve(cfg)
	case "engines":
		m := NewEngineManager(cfg)
		if fsg.NArg() > 0 && (fsg.Arg(0) == "update" || fsg.Arg(0) == "force") {
			m.UpdateAll(context.Background(), fsg.Arg(0) == "force")
		}
		printEngines(m)
	case "token":
		fmt.Println(cfg.Token)
	case "install":
		if err := runInstall(cfg, *noLinger); err != nil {
			fatal(err)
		}

	case "update":
		runUpdate()
	}
}

// runConfig loads the config for the commands that run İndirgen, with this
// run's -listen and -library on top.
func runConfig(dataDir, listen, library string) *Config {
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs // the unit file needs an absolute path
	}
	cfg, err := LoadConfig(dataDir)
	if err != nil {
		fatal(err)
	}
	// Flags apply to this run only. Persisting them meant a one-off
	// "-listen 0.0.0.0:…" kept the server exposed on every later start;
	// permanent changes belong in config.json.
	if listen != "" {
		cfg.Listen = listen
	}
	if library != "" {
		cfg.Library, cfg.libraryFlag = library, true
	}
	return cfg
}

// installedConfig reads, without creating anything, the config that
// status and uninstall look at: the data folder the installed service
// uses, unless -data names another one.
func installedConfig(dataDir string, dataFlag bool) *Config {
	if ip, err := userPaths(); err == nil && !dataFlag {
		if d := unitDataDir(ip.Unit); d != "" {
			dataDir = d
		}
	}
	return PeekConfig(dataDir)
}

// runUpdate is "indirgen update": install the newest release, if any.
func runUpdate() {
	u := NewUpdater(envOr("INDIRGEN_GITHUB_API", "https://api.github.com"))
	st := u.Check(context.Background())
	switch st.Status {
	case UpdAvailable:
		ver, err := u.Install(context.Background())
		if err != nil {
			fatal(err)
		}
		fmt.Printf("v%s kuruldu. Çalışan İndirgen'i yeniden başlat (systemd: systemctl --user restart indirgen).\n", ver)
	case UpdError:
		fatal(errors.New(st.Error))
	case UpdUnpublished:
		fmt.Println("Henüz yayınlanmış sürüm yok.")
	default:
		fmt.Printf("Güncel: v%s\n", version)
	}
}

// health exits 0 when the server answers on addr (default 127.0.0.1:8899).
func health(addr string) int { return healthCheck(addr, os.Stderr) }

// healthCheck asks the running İndirgen for its session; w gets the reason
// it failed (nil: quiet).
func healthCheck(addr string, w io.Writer) int {
	if w == nil {
		w = io.Discard
	}
	addr = cmp.Or(addr, "127.0.0.1:8899")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(w, "health:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/api/session")
	if err != nil {
		fmt.Fprintln(w, "health:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(w, "health:", resp.Status)
		return 1
	}
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return strings.TrimRight(v, "/")
	}
	return def
}

func printEngines(m *EngineManager) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MOTOR\tKAYNAK\tSÜRÜM\tDOĞRULANDI\tHATA")
	for _, s := range m.States() {
		v := "-"
		if s.Verified {
			v = "sha256 ✓"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Name, s.Source, orDash(s.Version), v, orDash(s.Error))
	}
	tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func serve(cfg *Config) {
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, shutdown := context.WithCancel(sigCtx)
	defer shutdown()
	// After a self-update: shut down cleanly (jobs re-queued), then restart.
	var restartReq atomic.Bool
	restart := func() { restartReq.Store(true); shutdown() }

	srv := newServer(ctx, cfg, restart)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fatal(friendlyListenError(cfg.Listen, err))
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	go srv.eng.Loop(ctx)
	if cfg.UpdateCheck == "on" {
		go srv.upd.Loop(ctx)
	}
	go srv.subs.Run(ctx)
	srv.tg.Start(ctx)
	queueDone := make(chan struct{})
	go func() { srv.queue.Run(ctx); close(queueDone) }()

	announce(cfg, srv.eng, ln.Addr())

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
	<-queueDone
	if restartReq.Load() {
		slog.Info("yeniden başlatılıyor")
		if err := reexec(); err != nil {
			fatal(fmt.Errorf("yeniden başlatılamadı, elle başlat: %w", err))
		}
		return
	}
	slog.Info("kapandı")
}

// newServer wires the parts together; nothing runs yet.
func newServer(ctx context.Context, cfg *Config, restart func()) *Server {
	hub := NewHub()
	eng := NewEngineManager(cfg)
	eng.onEvent = func() { hub.Publish("engines", eng.States()) }
	cookies := NewCookieStore(cfg.Dir("cookies"))
	cookies.cleanRunCopies() // left by a crash
	cookies.browser = browserCookieSource(cfg)
	cookies.ytdlp = func() string { return eng.Path(EngYTDLP) }
	router := &Router{eng: eng, cookies: cookies}
	queue := NewQueue(cfg, eng, router, cookies, hub)
	lib := NewLibrary(cfg.Library)
	queue.onLibraryChange = func() { lib.Invalidate(); hub.Publish("library", nil) }
	subs := NewSubs(cfg, queue, hub)
	tg := NewTelegram(cfg, queue, lib, envOr("INDIRGEN_TELEGRAM_API", "https://api.telegram.org"))
	queue.onDiskLow = tg.diskLow
	queue.onFinish = func(j Job) { subs.jobFinished(j); tg.jobFinished(j) }
	sessions := NewSessionStore(cfg.Dir("sessions.json"))
	sessions.onChange = func() { hub.Publish("sessions", nil) } // no content: the list is fetched with its own checks
	upd := NewUpdater(eng.api)
	upd.gh = eng.gh // one rate limit and one cache for all release checks
	upd.notify = func(s UpdateState) { hub.Publish("update", s) }
	return &Server{cfg: cfg, eng: eng, router: router, queue: queue, cookies: cookies, hub: hub, upd: upd, lib: lib, subs: subs, tg: tg, ctx: ctx, restart: restart, sessions: sessions}
}

// announce shows where İndirgen runs: a summary on a terminal, one log line
// for the journal.
func announce(cfg *Config, eng *EngineManager, addr net.Addr) {
	si := startInfo{Addr: addr, Library: cfg.Library, Data: cfg.dataDir, Auth: cfg.AuthRequired(),
		Engines: eng.States(), Telegram: cfg.TelegramConf()}
	for _, e := range si.Engines { // the first check (eng.Loop) replaces these with managed copies
		si.Fetching = si.Fetching || (e.Source != "managed" && eng.Installable(e.Name))
	}
	if cfg.tokenNew && si.Auth {
		si.NewToken = cfg.Token
	}
	if isTerminal(os.Stderr) {
		printBanner(os.Stderr, newPalette(colorWanted(os.Stderr)), si)
	} else {
		logStart(si)
	}
}

func fatal(err error) {
	p := newPalette(colorWanted(os.Stderr))
	fmt.Fprintf(os.Stderr, "%sindirgen: %v%s\n", p.err, err, p.reset)
	os.Exit(1)
}
