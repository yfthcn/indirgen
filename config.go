package main

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Config is persisted as <data>/config.json. Every field has a sane default,
// so the file can be edited by hand and partially filled.
type Config struct {
	Listen              string         `json:"listen"`                // e.g. 127.0.0.1:8899
	Library             string         `json:"library"`               // permanent "archive" output dir
	Concurrency         int            `json:"concurrency"`           // total parallel jobs
	PerHost             int            `json:"per_host"`              // parallel jobs per site
	TempRetentionHours  int            `json:"temp_retention_hours"`  // browser-mode files are deleted after this
	MinFreeMB           int64          `json:"min_free_mb"`           // refuse to start jobs below this
	AutoUpdate          bool           `json:"auto_update"`           // keep engines on latest stable
	UpdateIntervalHours int            `json:"update_interval_hours"` // how often to check
	Auth                string         `json:"auth"`                  // auto | always | never
	Token               string         `json:"token"`                 // access token (generated)
	SubLangs            string         `json:"sub_langs"`             // yt-dlp --sub-langs
	FFmpeg              string         `json:"ffmpeg"`                // managed | system
	Pace                string         `json:"pace"`                  // careful | normal | fast | custom
	SleepRequest        string         `json:"sleep_request"`         // custom: seconds between API requests ("6-12")
	SleepFile           string         `json:"sleep_file"`            // custom: seconds before each file ("1-3")
	MaxItems            int            `json:"max_items"`             // posts per profile section per job, 0 = all
	UpdateCheck         string         `json:"update_check"`          // on | off: look for new İndirgen releases
	RateLimitMB         int            `json:"rate_limit_mb"`         // total download speed cap in MB/s, 0 = none
	NightStart          string         `json:"night_start"`           // "night" jobs start inside this window (HH:MM)
	NightEnd            string         `json:"night_end"`
	Layout              string         `json:"layout"`                  // site: Site/Channel/… folders | flat: everything in one folder
	AllowedHosts        []string       `json:"allowed_hosts,omitempty"` // extra names İndirgen answers to (reverse proxy, VPN DNS)
	BrowserCookies      string         `json:"browser_cookies"`         // "firefox": use its cookies where no cookies.txt is imported | "" off
	YtdlpChannel        string         `json:"ytdlp_channel"`           // stable | nightly
	Telegram            TelegramConfig `json:"telegram"`

	dataDir     string
	fileListen  string       // Listen as stored in config.json
	fileLibrary string       // Library as stored in config.json
	libraryFlag bool         // -library given: the folder is not ours to change
	tokenNew    bool         // the token was created on this start (shown once)
	mu          sync.RWMutex // guards the user-editable fields
	saveMu      sync.Mutex   // one Save at a time: an older snapshot never overwrites a newer one
}

// TelegramConfig: the bot that takes links and reports results. Chats are
// the paired chat IDs; nothing else is answered.
type TelegramConfig struct {
	Token   string  `json:"token,omitempty"`
	Chats   []int64 `json:"chats,omitempty"`
	Quiet   bool    `json:"quiet,omitempty"`    // no "done / failed" messages
	NoFiles bool    `json:"no_files,omitempty"` // do not send downloaded files back
}

func (c *Config) TelegramConf() TelegramConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t := c.Telegram
	t.Chats = append([]int64(nil), t.Chats...)
	return t
}

// AccessToken is the token other devices sign in with.
func (c *Config) AccessToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Token
}

// RotateToken replaces the token with a new random one and saves it.
func (c *Config) RotateToken() (string, error) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	c.mu.Lock()
	c.Token = tok
	c.mu.Unlock()
	return tok, c.Save()
}

// SetTelegram changes the Telegram settings and saves config.json.
func (c *Config) SetTelegram(fn func(*TelegramConfig)) error {
	c.mu.Lock()
	fn(&c.Telegram)
	c.mu.Unlock()
	return c.Save()
}

// Pacing is the anti-bot behaviour applied to engine runs.
type Pacing struct {
	Pace     string `json:"pace"`
	Request  string `json:"sleep_request"` // gallery-dl --sleep-request / yt-dlp --sleep-requests
	File     string `json:"sleep_file"`    // gallery-dl --sleep / yt-dlp --sleep-interval
	MaxItems int    `json:"max_items"`
}

// gallery-dl's own Instagram default is 6–12 s between API requests.
var pacePresets = map[string]Pacing{
	"careful": {Pace: "careful", Request: "10-20", File: "2-6"},
	"normal":  {Pace: "normal", Request: "6-12", File: "1-3"},
	"fast":    {Pace: "fast", Request: "2-5", File: "0"},
}

var durRe = regexp.MustCompile(`^\d{1,3}(\.\d{1,2})?(-\d{1,3}(\.\d{1,2})?)?$`)

func validDuration(s string) bool { return durRe.MatchString(s) }

func (c *Config) Pacing() Pacing {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p := Pacing{Pace: c.Pace, Request: c.SleepRequest, File: c.SleepFile}
	if pr, ok := pacePresets[c.Pace]; ok {
		p.Request, p.File = pr.Request, pr.File
	}
	p.MaxItems = c.MaxItems
	return p
}

// CustomPacing returns the stored "custom" durations, whatever Pace is active.
func (c *Config) CustomPacing() (request, file string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SleepRequest, c.SleepFile
}

func (c *Config) SetPacing(p Pacing) error {
	if _, ok := pacePresets[p.Pace]; !ok && p.Pace != "custom" {
		return errors.New("geçersiz hız profili")
	}
	if p.Pace == "custom" && (!validDuration(p.Request) || !validDuration(p.File)) {
		return errors.New("süreler '6' ya da '6-12' biçiminde olmalı (saniye)")
	}
	if p.MaxItems < 0 || p.MaxItems > 100000 {
		return errors.New("geçersiz öğe sınırı")
	}
	c.mu.Lock()
	c.Pace, c.MaxItems = p.Pace, p.MaxItems
	if p.Pace == "custom" {
		c.SleepRequest, c.SleepFile = p.Request, p.File
	}
	c.mu.Unlock()
	return c.Save()
}

// durationBounds splits "6-12" into ("6", "12"); "3" into ("3", "3").
func durationBounds(s string) (string, string) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return s, s
	}
	return lo, hi
}

func defaultDataDir() string {
	if v := os.Getenv("INDIRGEN_DATA"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "indirgen")
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "indirgen")
	}
	return filepath.Join(home, ".local", "share", "indirgen")
}

func defaultLibrary() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "İndirgen")
}

func (c *Config) applyDefaults() {
	c.Listen = cmp.Or(c.Listen, "127.0.0.1:8899")
	c.Library = cmp.Or(c.Library, defaultLibrary())
	if c.Concurrency <= 0 {
		c.Concurrency = 3
	}
	if c.PerHost <= 0 {
		c.PerHost = 2
	}
	if c.TempRetentionHours <= 0 {
		c.TempRetentionHours = 24
	}
	if c.MinFreeMB <= 0 {
		c.MinFreeMB = 1024
	}
	if c.UpdateIntervalHours <= 0 {
		c.UpdateIntervalHours = 6
	}
	c.Auth = cmp.Or(c.Auth, "auto")
	c.SubLangs = cmp.Or(c.SubLangs, "tr.*,en.*")
	c.FFmpeg = cmp.Or(c.FFmpeg, "managed")
	if c.UpdateCheck != "off" {
		c.UpdateCheck = "on"
	}
	if _, ok := pacePresets[c.Pace]; !ok && c.Pace != "custom" {
		c.Pace = "normal"
	}
	if !validDuration(c.SleepRequest) {
		c.SleepRequest = "6-12"
	}
	if !validDuration(c.SleepFile) {
		c.SleepFile = "1-3"
	}
	if c.MaxItems < 0 {
		c.MaxItems = 0
	}
	c.RateLimitMB = min(max(c.RateLimitMB, 0), 1000)
	if c.Layout != "flat" {
		c.Layout = "site"
	}
	if _, ok := parseHM(c.NightStart); !ok {
		c.NightStart = "01:00"
	}
	if _, ok := parseHM(c.NightEnd); !ok || c.NightEnd == c.NightStart {
		c.NightEnd = "07:00"
	}
	if c.Token == "" {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		c.Token = hex.EncodeToString(b)
		c.tokenNew = true
	}
}

func (c *Config) path() string { return filepath.Join(c.dataDir, "config.json") }

// LoadConfig reads (or creates) config.json in dataDir.
func LoadConfig(dataDir string) (*Config, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	c := &Config{AutoUpdate: true}
	raw, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, errors.New("config.json okunamadı: " + err.Error())
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	c.dataDir = dataDir
	c.applyDefaults()
	c.fileListen, c.fileLibrary = c.Listen, c.Library
	return c, c.Save()
}

// PeekConfig reads config.json without creating or changing anything (for
// status and uninstall). A missing file gives the defaults.
func PeekConfig(dataDir string) *Config {
	c := &Config{AutoUpdate: true}
	if raw, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		_ = json.Unmarshal(raw, c)
	}
	c.dataDir = dataDir
	c.applyDefaults()
	c.fileListen, c.fileLibrary = c.Listen, c.Library
	return c
}

func (c *Config) Save() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.RLock()
	raw, err := json.Marshal(c)
	fileListen, fileLibrary := c.fileListen, c.fileLibrary
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	// Command-line overrides (-listen, -library) are for this run only:
	// write back what config.json held, not the override.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if fileListen != "" {
		m["listen"] = fileListen
	}
	if fileLibrary != "" {
		m["library"] = fileLibrary
	}
	raw, _ = json.MarshalIndent(m, "", "  ")
	return writeFileAtomic(c.path(), raw, 0o600)
}

// SetListen changes the stored listen address; it takes effect on restart.
func (c *Config) SetListen(addr string) error {
	c.mu.Lock()
	c.fileListen = addr
	c.mu.Unlock()
	return c.Save()
}

// ListenOverridden: -listen was given on the command line, so config.json's
// value is not the one in use.
func (c *Config) ListenOverridden() bool { return c.fileListen != "" && c.Listen != c.fileListen }

// AuthRequired: "auto" means a token is needed only when listening beyond loopback.
func (c *Config) AuthRequired() bool {
	switch c.Auth {
	case "always":
		return true
	case "never":
		return false
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

func (c *Config) Dir(parts ...string) string {
	return filepath.Join(append([]string{c.dataDir}, parts...)...)
}

// writeFileAtomic writes via a unique temp file in the same directory, so two
// concurrent writers of the same path never share (and corrupt) one temp file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	return err
}
