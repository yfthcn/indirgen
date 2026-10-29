package main

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// install / uninstall / status: the binary is its own installer.
//
// Everything goes to the standard per-user places (no sudo, nothing spread
// over the system):
//
//	~/.local/bin/indirgen                                   the program
//	~/.local/share/indirgen/                                settings, engines, queue, cookies
//	~/.config/systemd/user/indirgen.service                 starts it with the session (or at boot, with linger)
//	~/.local/share/applications/indirgen.desktop            menu entry, opens the web UI
//	~/.local/share/icons/hicolor/scalable/apps/indirgen.svg its icon
//
// The archive folder belongs to the user: uninstall never touches it, and
// keeps the data folder unless --purge is given.
// ---------------------------------------------------------------------------

const serviceName = "indirgen.service"

type installPaths struct {
	Bin, Unit, Desktop, Icon string
}

func userPaths() (installPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return installPaths{}, err
	}
	cfgHome := envDir("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dataHome := envDir("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	return installPaths{
		Bin:     filepath.Join(home, ".local", "bin", project.Name),
		Unit:    filepath.Join(cfgHome, "systemd", "user", serviceName),
		Desktop: filepath.Join(dataHome, "applications", project.Name+".desktop"),
		Icon:    filepath.Join(dataHome, "icons", "hicolor", "scalable", "apps", project.Name+".svg"),
	}, nil
}

func envDir(key, def string) string {
	if v := os.Getenv(key); filepath.IsAbs(v) {
		return v
	}
	return def
}

// unitFile: %h is the user's home; the data folder is passed explicitly so a
// custom -data at install time is kept.
func unitFile(bin, dataDir string) string {
	return fmt.Sprintf(`# Written by "indirgen install"; "indirgen uninstall" removes it.
[Unit]
Description=İndirgen media downloader
Documentation=https://github.com/%s
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s serve -data %s
Restart=on-failure
RestartSec=5
# Leave running downloads time to be re-queued.
TimeoutStopSec=20
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=default.target
`, project.Repo, systemdQuote(bin), systemdQuote(dataDir))
}

// systemdQuote makes a path one literal ExecStart word: "%" and "$" would
// otherwise expand (specifiers, environment variables).
func systemdQuote(s string) string {
	s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
	if strings.ContainsAny(s, " \t\"'\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

// unitDataDir reads -data back from an installed unit ("" if none): the
// folder uninstall -purge must delete is the one the service uses.
func unitDataDir(unit string) string {
	raw, err := os.ReadFile(unit)
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(raw)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		_, after, ok := strings.Cut(rest, " -data ")
		if !ok {
			return ""
		}
		v := strings.TrimSpace(after)
		if strings.HasPrefix(v, `"`) {
			end := 1
			for end < len(v) && (v[end] != '"' || v[end-1] == '\\') {
				end++
			}
			v = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v[1:min(end, len(v))])
		} else if i := strings.IndexByte(v, ' '); i > 0 {
			v = v[:i]
		}
		return strings.NewReplacer("%%", "%", "$$", "$").Replace(v)
	}
	return ""
}

func desktopFile(url, icon string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=İndirgen
GenericName=Media downloader
Comment=yapıştır, indir
Exec=xdg-open %s
Icon=%s
Terminal=false
Categories=Network;AudioVideo;
Keywords=youtube;download;yt-dlp;gallery-dl;video;indir;
`, url, icon)
}

// localURL is where this computer opens the UI.
func localURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		host, port = "", "8899"
	}
	if ip := net.ParseIP(host); host == "" || host == "localhost" || (ip != nil && (ip.IsUnspecified() || ip.IsLoopback())) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

type out struct {
	p palette
	w io.Writer
}

func newOut() out { return out{p: newPalette(colorWanted(os.Stdout)), w: os.Stdout} }

func (o out) ok(format string, a ...any) {
	fmt.Fprintf(o.w, "  %s✓%s %s\n", o.p.ok, o.p.reset, fmt.Sprintf(format, a...))
}
func (o out) warn(format string, a ...any) {
	fmt.Fprintf(o.w, "  %s!%s %s\n", o.p.warn, o.p.reset, fmt.Sprintf(format, a...))
}
func (o out) row(label, value string) {
	fmt.Fprintf(o.w, "  %s%-10s%s %s\n", o.p.dim, label, o.p.reset, value)
}
func (o out) title(s string) { fmt.Fprintf(o.w, "\n  %s%s%s\n\n", o.p.bold, s, o.p.reset) }

func hasSystemdUser() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

func systemctl(args ...string) error {
	b, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), cmpOr(strings.TrimSpace(string(b)), err.Error()))
	}
	return nil
}

func serviceActive() bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", serviceName).Run() == nil
}

func portBusy(listen string) bool {
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(localURL(listen), "http://"), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// copyExecutable writes src to dst through a temp file and a rename: a
// running binary at dst is replaced, never truncated ("text file busy").
func copyExecutable(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

func writeFile(path, content string, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(content), perm)
}

func installRefusal() error {
	switch {
	case inDocker():
		return errors.New("Docker'da kurulum yapılmaz; konteyneri compose ile çalıştır")
	case os.Geteuid() == 0:
		return errors.New("root olarak kurma: İndirgen kendi kullanıcınla çalışır (sudo olmadan çalıştır)")
	}
	return nil
}

// runInstall: copy the binary, write the service and the menu entry, start.
func runInstall(cfg *Config, noLinger bool) error {
	if err := installRefusal(); err != nil {
		return err
	}
	o := newOut()
	ip, err := userPaths()
	if err != nil {
		return err
	}
	self, err := runningExe()
	if err != nil {
		return fmt.Errorf("çalışan program bulunamadı: %w", err)
	}
	o.title("İndirgen " + version + " kuruluyor")

	sd := hasSystemdUser()
	wasActive := sd && serviceActive()
	if !wasActive && portBusy(cfg.Listen) {
		return fmt.Errorf("%s şu an başka bir İndirgen tarafından kullanılıyor: elle çalıştırdığın İndirgen'i kapat (Ctrl+C), sonra tekrar dene", localURL(cfg.Listen))
	}

	if same, _ := sameFile(self, ip.Bin); !same {
		if err := copyExecutable(self, ip.Bin); err != nil {
			return fmt.Errorf("program kopyalanamadı: %w", err)
		}
	}
	o.ok("program        %s", ip.Bin)

	icon, _ := webFS.ReadFile("web/icon.svg")
	if err := writeFile(ip.Icon, string(icon), 0o644); err == nil {
		if err := writeFile(ip.Desktop, desktopFile(localURL(cfg.Listen), ip.Icon), 0o644); err == nil {
			o.ok("menü girişi    %s", ip.Desktop)
		}
	}

	if !sd {
		o.warn("systemd kullanıcı servisi yok: İndirgen'i elle başlat: %s", ip.Bin)
	} else if err := installService(o, ip, cfg, wasActive, noLinger); err != nil {
		return err
	}

	fmt.Fprintln(o.w)
	o.row("Arayüz", o.p.bold+localURL(cfg.Listen)+o.p.reset)
	o.row("Veri", cfg.dataDir)
	o.row("Arşiv", cfg.Library)
	if cfg.tokenNew {
		o.row("Token", cfg.Token+o.p.dim+"  (ağdan erişimde gerekir; sonra: indirgen token)"+o.p.reset)
	}
	if !onPath(filepath.Dir(ip.Bin)) {
		o.warn("%s PATH'te değil: yeni bir terminal aç ya da ~/.profile'a ekle", filepath.Dir(ip.Bin))
	}
	fmt.Fprintf(o.w, "\n  Kaldırmak için: %s uninstall\n\n", project.Name)
	return nil
}

// installService writes the user service, starts it (restarts a running
// one) and, unless noLinger, lets it start at boot without a login.
func installService(o out, ip installPaths, cfg *Config, wasActive, noLinger bool) error {
	if err := writeFile(ip.Unit, unitFile(ip.Bin, cfg.dataDir), 0o644); err != nil {
		return fmt.Errorf("servis dosyası yazılamadı: %w", err)
	}
	o.ok("servis         %s", ip.Unit)
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", serviceName); err != nil {
		return err
	}
	action := "start"
	if wasActive {
		action = "restart"
	}
	if err := systemctl(action, serviceName); err != nil {
		return err
	}
	o.ok("servis çalışıyor (oturum açınca kendiliğinden başlar)")
	if !noLinger {
		user := cmp.Or(os.Getenv("USER"), filepath.Base(os.Getenv("HOME")))
		if exec.Command("loginctl", "enable-linger", user).Run() == nil {
			o.ok("açılışta başlar, oturum açmasan da çalışır (linger)")
		} else {
			o.warn("oturum açmadan çalışması için bir kez: sudo loginctl enable-linger %s", user)
		}
	}
	return nil
}

func runUninstall(cfg *Config, purge, yes bool) error {
	if inDocker() {
		return errors.New("Docker'da: docker compose down")
	}
	o := newOut()
	ip, err := userPaths()
	if err != nil {
		return err
	}
	o.title("İndirgen kaldırılıyor")
	if purge && exists(cfg.dataDir) {
		if err := purgeAllowed(cfg.dataDir); err != nil {
			return err
		}
	}
	if purge && !yes && !confirm(fmt.Sprintf("Ayarlar, motorlar, kuyruk ve çerezler silinecek (%s). İndirilenlere dokunulmaz. Devam?", cfg.dataDir)) {
		return errors.New("vazgeçildi")
	}

	bus := hasSystemdUser()
	if bus {
		_ = systemctl("disable", "--now", serviceName)
	}
	// Without a user bus (su, sudo -u, ssh without a session) systemctl cannot
	// stop it: remove the files anyway so it does not start again.
	wants := filepath.Join(filepath.Dir(ip.Unit), "default.target.wants", serviceName)
	hadUnit := exists(ip.Unit)
	_ = os.Remove(wants)
	if hadUnit {
		if err := os.Remove(ip.Unit); err != nil {
			return err
		}
	}
	switch {
	case hadUnit && bus:
		_ = systemctl("daemon-reload")
		o.ok("servis durduruldu ve silindi")
	case hadUnit:
		o.warn("servis dosyası silindi, ama şu an çalışıyor olabilir: kendi oturumunda: systemctl --user stop indirgen")
	}
	for _, f := range []string{ip.Desktop, ip.Icon} {
		_ = os.Remove(f)
	}
	o.ok("menü girişi silindi")
	for _, f := range []string{ip.Bin, ip.Bin + ".old", ip.Bin + ".new"} {
		_ = os.Remove(f)
	}
	o.ok("program silindi        %s", ip.Bin)
	if purge {
		kept, err := purgeData(cfg.dataDir, cfg.Library)
		if err != nil {
			return fmt.Errorf("veri klasörü silinemedi: %w", err)
		}
		o.ok("veri klasörü silindi   %s", cfg.dataDir)
		if kept != "" {
			o.warn("arşiv veri klasörünün içinde, korundu: %s", kept)
		}
	} else {
		o.row("Kaldı", cfg.dataDir+o.p.dim+"  (ayarlar; tamamen silmek için: --purge)"+o.p.reset)
	}
	o.row("Kaldı", cfg.Library+o.p.dim+"  (indirdiklerin, sana ait)"+o.p.reset)
	fmt.Fprintln(o.w)
	return nil
}

// purgeAllowed refuses to delete a folder that is not İndirgen's data folder:
// a mistyped -data or a hand-edited unit must not take the home folder with it.
func purgeAllowed(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	refuse := func(why string) error {
		return fmt.Errorf("%s silinmedi: %s (ayarlar yerinde kaldı)", abs, why)
	}
	if abs == string(filepath.Separator) {
		return refuse("kök klasör")
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots := []string{home,
			envDir("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
			envDir("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
			envDir("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
			envDir("XDG_STATE_HOME", filepath.Join(home, ".local", "state")),
			filepath.Join(home, ".local")}
		for _, r := range roots {
			if r, err := filepath.Abs(r); err == nil && r == abs {
				return refuse("ev klasörü ya da XDG kökü")
			}
		}
	}
	for _, name := range []string{"config.json", "jobs.json"} {
		if st, err := os.Stat(filepath.Join(abs, name)); err == nil && st.Mode().IsRegular() {
			return nil
		}
	}
	return refuse("içinde İndirgen'e ait config.json ya da jobs.json yok")
}

// dataEntries is everything İndirgen keeps in its data folder.
var dataEntries = []string{"config.json", "jobs.json", "subscriptions.json", "telegram.json", "sessions.json",
	"archive", "cookies", "engines", "parts", "temp"}

// purgeData deletes the data folder for uninstall -purge, but never the
// archive: when the archive is inside the data folder everything else goes,
// and when it is the data folder itself only İndirgen's own entries go.
// kept is the archive folder that stayed ("" when it was elsewhere).
// Paths are compared with symbolic links resolved.
func purgeData(dataDir, library string) (kept string, err error) {
	data, lib := realPath(dataDir), realPath(library)
	switch {
	case lib == data:
		for _, e := range dataEntries {
			if err := os.RemoveAll(filepath.Join(data, e)); err != nil {
				return "", err
			}
			left, _ := filepath.Glob(filepath.Join(data, "."+e+".*.tmp")) // interrupted atomic writes
			for _, f := range left {
				_ = os.Remove(f)
			}
		}
		return library, nil
	case isWithin(data, lib) && exists(lib): // (no archive folder: nothing to keep)
		rel, err := filepath.Rel(data, lib)
		if err != nil {
			return "", err
		}
		// Walk down to the archive, deleting everything beside the way.
		dir := data
		for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return "", err
			}
			for _, e := range entries {
				if e.Name() != part {
					if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
						return "", err
					}
				}
			}
			dir = filepath.Join(dir, part)
		}
		return library, nil
	}
	return "", os.RemoveAll(dataDir)
}

// realPath resolves symbolic links in p; for a path that does not exist (yet),
// in its longest existing part.
func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs
	}
	return filepath.Join(realPath(parent), filepath.Base(abs))
}

func runStatus(cfg *Config) int {
	o := newOut()
	ip, _ := userPaths()
	o.title("İndirgen " + version)
	switch {
	case inDocker():
		o.row("Kurulum", "Docker")
	case exists(ip.Unit):
		state := "durmuş"
		if serviceActive() {
			state = o.p.ok + "çalışıyor" + o.p.reset
		}
		o.row("Kurulum", "servis ("+state+")")
	default:
		o.row("Kurulum", "kurulu değil (elle çalıştırılıyor olabilir)")
	}
	up := healthCheck(cfg.Listen, nil) == 0
	o.row("Arayüz", localURL(cfg.Listen)+map[bool]string{true: o.p.ok + "  yanıt veriyor" + o.p.reset, false: o.p.warn + "  yanıt yok" + o.p.reset}[up])
	o.row("Program", cmpOr(func() string { p, _ := runningExe(); return p }(), "?"))
	o.row("Veri", cfg.dataDir)
	o.row("Arşiv", cfg.Library)
	if exists(ip.Unit) {
		o.row("Kayıtlar", "journalctl --user -u indirgen -f")
	}
	fmt.Fprintln(o.w)
	if up {
		return 0
	}
	return 1
}

func confirm(q string) bool {
	fmt.Printf("  %s [e/H] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "e", "evet", "y", "yes":
		return true
	}
	return false
}

func sameFile(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}
