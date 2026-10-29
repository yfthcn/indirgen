package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Telegram bot: send it a link from any app (share → Telegram), İndirgen
// queues it, answers when it is done or failed and sends the file back when
// it fits (bots may upload up to 50 MB). Results of jobs from the iPhone
// Shortcut, the browser extension and subscriptions are reported too.
//
// İndirgen asks Telegram for new messages (long polling), so it needs no
// open port and works wherever the computer has internet. Only paired chats
// are answered; pairing is a one-time code from Settings.
// ---------------------------------------------------------------------------

const (
	tgMaxUpload = 50 << 20 // Bot API limit for files sent by a bot
	tgMaxPhoto  = 10 << 20
	tgMaxFiles  = 10 // files sent back per job; the rest stay in the archive
)

type TGStatus struct {
	HasToken  bool    `json:"has_token"`
	Bot       string  `json:"bot,omitempty"` // @username
	Chats     []int64 `json:"chats"`
	Quiet     bool    `json:"quiet"`
	NoFiles   bool    `json:"no_files"`
	Connected bool    `json:"connected"`
	Error     string  `json:"error,omitempty"`
	PairCode  string  `json:"pair_code,omitempty"`
	PairLink  string  `json:"pair_link,omitempty"` // t.me deep link that sends /start CODE
}

type Telegram struct {
	cfg    *Config
	q      *Queue
	lib    *Library // to find earlier downloads of a link
	api    string   // https://api.telegram.org
	client *http.Client

	mu        sync.Mutex
	parent    context.Context
	cancel    context.CancelFunc
	bot       string
	connected bool
	lastErr   string
	pairCode  string
	pairUntil time.Time
	offset    int64
}

func NewTelegram(cfg *Config, q *Queue, lib *Library, api string) *Telegram {
	t := &Telegram{cfg: cfg, q: q, lib: lib, api: api, client: &http.Client{}}
	t.loadOffset()
	return t
}

// ---- Bot API -------------------------------------------------------------

type tgUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type tgEntity struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type tgMessage struct {
	ID              int                `json:"message_id"`
	Chat            struct{ ID int64 } `json:"chat"`
	Text            string             `json:"text"`
	Caption         string             `json:"caption"`
	Entities        []tgEntity         `json:"entities"`
	CaptionEntities []tgEntity         `json:"caption_entities"`
}

type tgUpdate struct {
	ID      int64      `json:"update_id"`
	Message *tgMessage `json:"message"`
}

type tgReply struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

func (t *Telegram) endpoint(token, method string) string {
	return t.api + "/bot" + token + "/" + method
}

// call posts JSON params and decodes "result" into out.
func (t *Telegram) call(ctx context.Context, token, method string, params, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint(token, method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return t.do(req, out)
}

func (t *Telegram) do(req *http.Request, out any) error {
	resp, err := t.client.Do(req)
	if err != nil {
		// The token is part of the URL: never let it reach logs or the UI.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			return fmt.Errorf("Telegram'a ulaşılamadı: %v", ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	var r tgReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return fmt.Errorf("Telegram: %s", resp.Status)
	}
	if !r.OK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
			return errors.New("bot token geçersiz")
		}
		return errors.New("Telegram: " + r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (t *Telegram) getMe(ctx context.Context, token string) (string, error) {
	var u tgUser
	if err := t.call(ctx, token, "getMe", map[string]any{}, &u); err != nil {
		return "", err
	}
	return u.Username, nil
}

// send writes a plain-text message (no Markdown: titles contain anything).
func (t *Telegram) send(ctx context.Context, token string, chat int64, replyTo int, text string) error {
	p := map[string]any{"chat_id": chat, "text": clip(text, 4000), "link_preview_options": map[string]bool{"is_disabled": true}}
	if replyTo > 0 {
		p["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	return t.call(ctx, token, "sendMessage", p, nil)
}

// sendFile uploads one file, streamed (no copy in memory), with the method
// that shows it best in the chat.
func (t *Telegram) sendFile(ctx context.Context, token string, chat int64, replyTo int, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	method, field := "sendDocument", "document"
	switch kindOf(path) {
	case "video":
		if ext := strings.ToLower(filepath.Ext(path)); ext == ".mp4" || ext == ".mov" || ext == ".m4v" {
			method, field = "sendVideo", "video"
		}
	case "audio":
		method, field = "sendAudio", "audio"
	case "image":
		if st.Size() <= tgMaxPhoto && !strings.EqualFold(filepath.Ext(path), ".gif") {
			method, field = "sendPhoto", "photo"
		}
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		_ = mw.WriteField("chat_id", strconv.FormatInt(chat, 10))
		if replyTo > 0 {
			rp, _ := json.Marshal(map[string]any{"message_id": replyTo, "allow_sending_without_reply": true})
			_ = mw.WriteField("reply_parameters", string(rp))
		}
		if method == "sendVideo" {
			_ = mw.WriteField("supports_streaming", "true")
		}
		part, err := mw.CreateFormFile(field, filepath.Base(path))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint(token, method), pr)
	if err != nil {
		pr.Close()
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return t.do(req, nil)
}

// ---- lifecycle -----------------------------------------------------------

// Start runs the bot until ctx ends; Reconfigure restarts it after changes.
func (t *Telegram) Start(ctx context.Context) {
	t.mu.Lock()
	t.parent = ctx
	t.mu.Unlock()
	t.Reconfigure()
}

func (t *Telegram) Reconfigure() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.connected, t.lastErr, t.bot = false, "", ""
	token := t.cfg.TelegramConf().Token
	if token == "" || t.parent == nil {
		return
	}
	ctx, cancel := context.WithCancel(t.parent)
	t.cancel = cancel
	go t.poll(ctx, token)
}

func (t *Telegram) setConn(ok bool, bot string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.connected = ok
	if bot != "" {
		t.bot = bot
	}
	t.lastErr = ""
	if err != nil {
		t.lastErr = err.Error()
	}
}

func (t *Telegram) poll(ctx context.Context, token string) {
	backoff := 5 * time.Second
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
			backoff = min(backoff*2, 2*time.Minute)
			return true
		}
	}
	for ctx.Err() == nil {
		bot, err := t.getMe(ctx, token)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			t.setConn(false, "", err)
			slog.Warn("Telegram botu bağlanamadı", "hata", err)
			if !wait() {
				return
			}
			continue
		}
		t.setConn(true, bot, nil)
		backoff = 5 * time.Second
		break
	}
	for ctx.Err() == nil {
		t.mu.Lock()
		offset := t.offset
		t.mu.Unlock()
		var ups []tgUpdate
		pctx, cancel := context.WithTimeout(ctx, 70*time.Second)
		err := t.call(pctx, token, "getUpdates", map[string]any{
			"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}, &ups)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			t.setConn(false, "", err)
			if !wait() {
				return
			}
			continue
		}
		t.setConn(true, "", nil)
		backoff = 5 * time.Second
		for _, u := range ups {
			if u.Message != nil {
				t.handle(ctx, token, u.Message)
			}
			t.mu.Lock()
			t.offset = u.ID + 1
			t.mu.Unlock()
		}
		if len(ups) > 0 {
			t.saveOffset()
		}
	}
}

// The update offset survives restarts, so a message is never handled twice.
func (t *Telegram) offsetFile() string { return t.cfg.Dir("telegram.json") }

func (t *Telegram) loadOffset() {
	raw, err := os.ReadFile(t.offsetFile())
	if err != nil {
		return
	}
	var v struct{ Offset int64 }
	if json.Unmarshal(raw, &v) == nil {
		t.offset = v.Offset
	}
}

func (t *Telegram) saveOffset() {
	t.mu.Lock()
	raw, _ := json.Marshal(map[string]int64{"offset": t.offset})
	t.mu.Unlock()
	_ = writeFileAtomic(t.offsetFile(), raw, 0o600)
}

// ---- pairing -------------------------------------------------------------

// NewPairCode starts pairing: the first chat that sends "/start CODE" within
// ten minutes is allowed.
func (t *Telegram) NewPairCode() {
	n, _ := rand.Int(rand.Reader, big.NewInt(900000))
	t.mu.Lock()
	t.pairCode = strconv.FormatInt(n.Int64()+100000, 10)
	t.pairUntil = time.Now().Add(10 * time.Minute)
	t.mu.Unlock()
}

func (t *Telegram) Status() TGStatus {
	c := t.cfg.TelegramConf()
	t.mu.Lock()
	defer t.mu.Unlock()
	s := TGStatus{HasToken: c.Token != "", Bot: t.bot, Chats: c.Chats, Quiet: c.Quiet, NoFiles: c.NoFiles,
		Connected: t.connected, Error: t.lastErr}
	if s.Chats == nil {
		s.Chats = []int64{}
	}
	if t.pairCode != "" && time.Now().Before(t.pairUntil) {
		s.PairCode = t.pairCode
		if t.bot != "" {
			s.PairLink = "https://t.me/" + t.bot + "?start=" + t.pairCode
		}
	}
	return s
}

// ---- incoming messages ---------------------------------------------------

const tgHelp = "Bana bir video, gönderi, profil ya da playlist bağlantısı gönder; İndirgen'in kuyruğuna ekleyeyim. " +
	"Bitince haber veririm, 50 MB'a kadar dosyaları buraya da gönderirim.\n\n" +
	"/durum: kuyrukta ne var\n/gecici <bağlantı>: arşive değil, geçici olarak indir\n" +
	"/yeniden <bağlantı>: daha önce indirilmiş olsa da tekrar indir\n" +
	"/gece <bağlantı>: gece saatlerinde indir (Ayarlar → İndirme)"

func (t *Telegram) handle(ctx context.Context, token string, m *tgMessage) {
	text := strings.TrimSpace(m.Text + "\n" + m.Caption)
	c := t.cfg.TelegramConf()

	if !slices.Contains(c.Chats, m.Chat.ID) {
		t.pair(ctx, token, m, text)
		return
	}

	switch cmd := strings.Fields(text + " ")[0]; {
	case cmd == "/start" || cmd == "/yardim" || cmd == "/help":
		_ = t.send(ctx, token, m.Chat.ID, m.ID, tgHelp)
		return
	case cmd == "/durum":
		_ = t.send(ctx, token, m.Chat.ID, m.ID, t.summary())
		return
	}
	t.queueLinks(ctx, token, m, text)
}

// pair links an unknown chat that sends the valid pairing code.
func (t *Telegram) pair(ctx context.Context, token string, m *tgMessage, text string) {
	// Strangers get no answer at all, only a valid pairing code counts.
	code, ok := strings.CutPrefix(text, "/start ")
	t.mu.Lock()
	valid := ok && t.pairCode != "" && strings.TrimSpace(code) == t.pairCode && time.Now().Before(t.pairUntil)
	if valid {
		t.pairCode = ""
	}
	t.mu.Unlock()
	if !valid {
		return
	}
	if err := t.cfg.SetTelegram(func(tc *TelegramConfig) {
		if !slices.Contains(tc.Chats, m.Chat.ID) {
			tc.Chats = append(tc.Chats, m.Chat.ID)
		}
	}); err != nil {
		slog.Warn("Telegram sohbeti kaydedilemedi", "hata", err)
		return
	}
	slog.Info("Telegram sohbeti eşlendi", "sohbet", m.Chat.ID)
	_ = t.send(ctx, token, m.Chat.ID, 0, "✓ İndirgen'e bağlandı.\n\n"+tgHelp)
}

// queueLinks queues the links of a message (/gecici, /gece, /yeniden change
// how) and answers with what happened.
func (t *Telegram) queueLinks(ctx context.Context, token string, m *tgMessage, text string) {
	mode := "library"
	if strings.HasPrefix(text, "/gecici") {
		mode = "temp"
	}
	force := strings.HasPrefix(text, "/yeniden")
	night := strings.HasPrefix(text, "/gece")
	// Links hidden behind text ("watch this") come as text_link entities.
	var hidden []string
	for _, e := range append(m.Entities, m.CaptionEntities...) {
		if e.Type == "text_link" {
			hidden = append(hidden, e.URL)
		}
	}
	links := extractURLs(text+"\n"+strings.Join(hidden, "\n"), 10)
	if len(links) == 0 {
		_ = t.send(ctx, token, m.Chat.ID, m.ID, "Mesajda bağlantı yok. "+tgHelp)
		return
	}
	for i, l := range links { // share links → the video's own link, before "already downloaded?"
		links[i] = resolveShortLink(ctx, l)
	}
	var hosts []string
	queued := 0
	for _, l := range links {
		// The same video, still on disk: show it instead of downloading again.
		if !force {
			if ex, ok := FindExisting(t.q, t.lib, l); ok {
				t.reportExisting(ctx, token, m, l, ex)
				continue
			}
		}
		v, err := t.q.Add(NewJob{URL: l, Opts: JobOptions{Preset: "best", Embed: true, Mode: mode, Night: night},
			Source: "telegram", Reply: &ChatRef{Chat: m.Chat.ID, Msg: m.ID}})
		if err != nil {
			continue
		}
		queued++
		if !slices.Contains(hosts, v.Host) {
			hosts = append(hosts, v.Host)
		}
	}
	if queued == 0 {
		return
	}
	msg := "⏳ Kuyruğa alındı: " + strings.Join(hosts, ", ")
	if queued > 1 {
		msg = fmt.Sprintf("⏳ %d bağlantı kuyruğa alındı: %s", queued, strings.Join(hosts, ", "))
	}
	if night {
		qs := t.cfg.QueueSettings()
		msg = fmt.Sprintf("🌙 Gece indirilecek (%s–%s): %s", qs.NightStart, qs.NightEnd, strings.Join(hosts, ", "))
	}
	_ = t.send(ctx, token, m.Chat.ID, m.ID, msg)
}

func (t *Telegram) reportExisting(ctx context.Context, token string, m *tgMessage, link string, ex Existing) {
	title := clip(ex.Title, 150)
	if ex.Active {
		_ = t.send(ctx, token, m.Chat.ID, m.ID, "⏳ Bu zaten kuyrukta: "+title)
		return
	}
	var size int64
	for _, f := range ex.Files {
		if st, err := os.Stat(f); err == nil {
			size += st.Size()
		}
	}
	text := "✓ Zaten indirilmiş: " + title + " · " + humanBytes(size)
	if !ex.When.IsZero() {
		text += " · " + ex.When.Local().Format("02.01.2006")
	}
	text += "\nYeniden indirmek için: /yeniden " + link
	if err := t.send(ctx, token, m.Chat.ID, m.ID, text); err != nil {
		return
	}
	if !t.cfg.TelegramConf().NoFiles {
		t.sendFiles(ctx, token, m.Chat.ID, m.ID, ex.Files)
	}
}

func (t *Telegram) summary() string {
	running, queued := 0, 0
	var last []string
	for _, j := range t.q.List() { // newest first
		switch j.Status {
		case StRunning:
			running++
		case StQueued:
			queued++
		case StDone:
			if len(last) < 3 {
				last = append(last, "✓ "+clip(j.Title, 60))
			}
		}
	}
	s := fmt.Sprintf("İniyor: %d · Sırada: %d", running, queued)
	if len(last) > 0 {
		s += "\n\nSon bitenler:\n" + strings.Join(last, "\n")
	}
	return s
}

// ---- results -------------------------------------------------------------

// jobFinished reports a finished job that did not start in the web UI.
func (t *Telegram) jobFinished(j Job) {
	c := t.cfg.TelegramConf()
	if c.Token == "" || c.Quiet || len(c.Chats) == 0 || j.Status == StCanceled {
		return
	}
	switch j.Source {
	case "telegram", "api", "sub":
	default:
		return // started in the web UI: you are looking at it
	}
	if j.Source == "sub" && j.Status == StDone && len(j.Files) == 0 {
		return // a subscription check with nothing new stays quiet
	}
	t.mu.Lock()
	parent := t.parent
	t.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()

	title := clip(j.Title, 150)
	if j.Source == "sub" {
		title = "🔔 " + title
	}
	var size int64
	var files []string
	for _, f := range j.Deliverable() {
		if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
			size += st.Size()
			files = append(files, f)
		}
	}
	var text string
	switch {
	case j.Status == StError:
		text = "✕ " + title + "\n" + firstLine(j.Error)
	case len(files) == 0:
		text = "✓ " + title + "\nYeni bir şey yok."
	case len(files) == 1:
		text = "✓ " + title + " · " + humanBytes(size)
	default:
		text = fmt.Sprintf("✓ %s · %d dosya · %s", title, len(files), humanBytes(size))
	}

	targets := c.Chats
	replyTo := 0
	if j.Reply != nil && slices.Contains(c.Chats, j.Reply.Chat) {
		targets, replyTo = []int64{j.Reply.Chat}, j.Reply.Msg
	}
	sendBack := j.Source == "telegram" && !c.NoFiles && j.Status == StDone
	for _, chat := range targets {
		if err := t.send(ctx, c.Token, chat, replyTo, text); err != nil {
			slog.Warn("Telegram mesajı gönderilemedi", "hata", err)
			continue
		}
		if sendBack {
			t.sendFiles(ctx, c.Token, chat, replyTo, files)
		}
	}
}

// sendFiles uploads up to tgMaxFiles files that fit the Bot API limit and
// says what was left out.
func (t *Telegram) sendFiles(ctx context.Context, token string, chat int64, replyTo int, files []string) {
	tooBig, sent := 0, 0
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		if st.Size() > tgMaxUpload {
			tooBig++
			continue
		}
		if sent == tgMaxFiles {
			break
		}
		if err := t.sendFile(ctx, token, chat, replyTo, f); err != nil {
			slog.Warn("Telegram'a dosya gönderilemedi", "dosya", filepath.Base(f), "hata", err)
			_ = t.send(ctx, token, chat, replyTo, "⚠ "+filepath.Base(f)+" gönderilemedi: "+firstLine(err.Error()))
			continue
		}
		sent++
	}
	var notes []string
	if tooBig > 0 {
		notes = append(notes, fmt.Sprintf("%d dosya 50 MB'tan büyük, Telegram'a gönderilemiyor.", tooBig))
	}
	if rest := len(files) - sent - tooBig; rest > 0 {
		notes = append(notes, fmt.Sprintf("%d dosya daha var.", rest))
	}
	if len(notes) > 0 {
		_ = t.send(ctx, token, chat, replyTo, "ℹ "+strings.Join(notes, " ")+" Hepsi İndirgen arşivinde.")
	}
}

// Test sends a message to every paired chat.
func (t *Telegram) Test(ctx context.Context) error {
	c := t.cfg.TelegramConf()
	if c.Token == "" || len(c.Chats) == 0 {
		return errors.New("önce botu kur ve bir sohbet eşle")
	}
	for _, chat := range c.Chats {
		if err := t.send(ctx, c.Token, chat, 0, "İndirgen'den deneme mesajı ✓"); err != nil {
			return err
		}
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), 0
	units := []string{"KB", "MB", "GB", "TB"}
	for v /= unit; v >= unit && i < len(units)-1; i++ {
		v /= unit
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// diskLow warns the paired chats once when free space runs out: downloads
// wait until space is freed, which is easy to miss when away.
func (t *Telegram) diskLow(st QueueState) {
	c := t.cfg.TelegramConf()
	if c.Token == "" || c.Quiet || len(c.Chats) == 0 {
		return
	}
	msg := fmt.Sprintf("⚠ Disk dolmak üzere: %s boş (en az %s gerekli). Yeni indirmeler yer açılınca kendiliğinden başlar.",
		humanBytes(max(st.Free, 0)), humanBytes(st.MinFreeMB<<20))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, chat := range c.Chats {
		_ = t.send(ctx, c.Token, chat, 0, msg)
	}
}
