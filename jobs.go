package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Sites that block aggressively: one job at a time and paced requests.
var sensitiveHosts = []string{"instagram.com", "facebook.com", "threads.net", "threads.com",
	"tiktok.com", "x.com", "twitter.com"}

const (
	StQueued   = "queued"
	StRunning  = "running"
	StDone     = "done"
	StError    = "error"
	StCanceled = "canceled"

	maxAttempts = 3
	maxJobs     = 1000
)

type Job struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Host      string     `json:"host"`
	Engine    string     `json:"engine"`
	Title     string     `json:"title"`
	Thumb     string     `json:"thumb,omitempty"`
	Opts      JobOptions `json:"opts"`
	Expected  int        `json:"expected,omitempty"` // gallery item count from probe
	Status    string     `json:"status"`
	Phase     string     `json:"phase,omitempty"` // downloading | processing
	Progress  float64    `json:"progress"`        // 0..1, -1 unknown
	Speed     float64    `json:"speed,omitempty"`
	ETA       int        `json:"eta,omitempty"`
	Bytes     int64      `json:"bytes,omitempty"`
	Total     int64      `json:"total,omitempty"`
	Item      int        `json:"item,omitempty"`
	Items     int        `json:"items,omitempty"`
	Files     []string   `json:"files,omitempty"`
	Existing  []string   `json:"existing,omitempty"` // already on disk before this run (served, never deleted or counted as new)
	Marked    int        `json:"marked,omitempty"`   // entries a marking run recorded as seen
	Crashes   int        `json:"crashes,omitempty"`  // runs cut short by İndirgen itself stopping
	Error     string     `json:"error,omitempty"`
	Note      string     `json:"note,omitempty"` // UI message key, e.g. "no_new"
	Attempts  int        `json:"attempts"`
	NotBefore time.Time  `json:"not_before,omitzero"`
	Created   time.Time  `json:"created"`
	Started   time.Time  `json:"started,omitzero"`
	Finished  time.Time  `json:"finished,omitzero"`
	Expired   bool       `json:"expired,omitempty"`
	Sub       string     `json:"sub,omitempty"`    // subscription that queued it
	Source    string     `json:"source,omitempty"` // "" web UI | api (Shortcut, extension) | telegram | sub
	Reply     *ChatRef   `json:"reply,omitempty"`  // Telegram message to answer when it ends
	Rev       int64      `json:"rev,omitempty"`    // +1 per published change: the UI keeps the newest when a reply and an event cross

	cancel   context.CancelFunc
	lastPub  time.Time
	userStop bool
}

// finished: the job will not run again unless retried.
func (j *Job) finished() bool {
	return j.Status == StDone || j.Status == StError || j.Status == StCanceled
}

// JobView is the public representation (file paths reduced to names).
type JobView struct {
	Job
	FileNames []string `json:"file_names,omitempty"`
}

type Queue struct {
	cfg     *Config
	eng     *EngineManager
	router  *Router
	cookies *CookieStore
	hub     *Hub

	mu      sync.Mutex
	jobs    map[string]*Job
	wake    chan struct{}
	saveReq chan struct{}
	ctx     context.Context
	wg      sync.WaitGroup

	onLibraryChange func()           // files were added to or removed from the library
	onFinish        func(Job)        // a run ended (done, error or canceled)
	onDiskLow       func(QueueState) // free space fell below the minimum
	state           QueueState
}

func (q *Queue) libraryChanged() {
	if q.onLibraryChange != nil {
		q.onLibraryChange()
	}
}

// ActiveSubs reports which subscriptions have a job queued or running.
func (q *Queue) ActiveSubs() map[string]bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := map[string]bool{}
	for _, j := range q.jobs {
		if j.Sub != "" && (j.Status == StQueued || j.Status == StRunning) {
			out[j.Sub] = true
		}
	}
	return out
}

// ForgetFiles drops deleted library paths from job records: abs itself, and
// everything below it when it was a folder.
func (q *Queue) ForgetFiles(abs string) {
	abs, _ = filepath.Abs(abs)
	changed := false
	q.mu.Lock()
	for _, j := range q.jobs {
		kept := slices.DeleteFunc(slices.Clone(j.Files), func(f string) bool {
			fa, err := filepath.Abs(f)
			return err == nil && isWithin(abs, fa)
		})
		if len(kept) != len(j.Files) {
			j.Files = kept
			q.publishLocked(j, true)
			changed = true
		}
	}
	q.mu.Unlock()
	if changed {
		q.requestSave()
	}
}

func NewQueue(cfg *Config, eng *EngineManager, router *Router, cookies *CookieStore, hub *Hub) *Queue {
	q := &Queue{cfg: cfg, eng: eng, router: router, cookies: cookies, hub: hub,
		jobs: map[string]*Job{}, wake: make(chan struct{}, 1), saveReq: make(chan struct{}, 1)}
	q.load()
	return q
}

func (q *Queue) file() string { return q.cfg.Dir("jobs.json") }

func (q *Queue) load() {
	raw, err := os.ReadFile(q.file())
	if err != nil {
		return
	}
	var list []*Job
	if err := json.Unmarshal(raw, &list); err != nil {
		slog.Warn("jobs.json okunamadı", "hata", err)
		return
	}
	for _, j := range list {
		if j.Status == StRunning { // interrupted by a crash or power loss → resume
			j.Status, j.Phase, j.Progress = StQueued, "", 0
			j.Crashes++
			if j.Crashes >= 3 { // it may be the cause (out of memory…): stop retrying it
				j.Status, j.Finished = StError, time.Now()
				j.Error = "İndirgen bu iş sırasında üç kez kapandı; iş durduruldu (tekrar denemek için: Tekrar dene)"
			} else {
				j.Attempts = max(j.Attempts-1, 0) // not the job's fault: keep its retries
			}
		}
		q.jobs[j.ID] = j
	}
}

func (q *Queue) saveNow() {
	q.mu.Lock()
	list := q.sortedLocked()
	raw, _ := json.Marshal(list)
	q.mu.Unlock()
	if err := writeFileAtomic(q.file(), raw, 0o600); err != nil {
		slog.Warn("jobs.json yazılamadı", "hata", err)
	}
}

func (q *Queue) requestSave() {
	select {
	case q.saveReq <- struct{}{}:
	default:
	}
}

func (q *Queue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) sortedLocked() []*Job {
	list := make([]*Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		list = append(list, j)
	}
	slices.SortFunc(list, func(a, b *Job) int { return a.Created.Compare(b.Created) })
	return list
}

func (q *Queue) view(j *Job) JobView {
	v := JobView{Job: *j}
	v.Files = nil
	for _, f := range j.Files {
		v.FileNames = append(v.FileNames, filepath.Base(f))
	}
	return v
}

func (q *Queue) List() []JobView {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.sortedLocked()
	out := make([]JobView, 0, len(list))
	for _, l := range slices.Backward(list) {
		out = append(out, q.view(l))
	}
	return out
}

func (q *Queue) Get(id string) (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// publish sends a job update; progress-only updates are throttled.
func (q *Queue) publishLocked(j *Job, force bool) {
	if !force && time.Since(j.lastPub) < 250*time.Millisecond {
		return
	}
	j.lastPub = time.Now()
	j.Rev++
	q.hub.Publish("job", q.view(j))
}

type NewJob struct {
	URL      string     `json:"url"`
	Engine   string     `json:"engine"`
	Title    string     `json:"title"`
	Thumb    string     `json:"thumb"`
	Expected int        `json:"expected"`
	Opts     JobOptions `json:"opts"`
	Sub      string     `json:"-"` // set by the subscription scheduler only
	Source   string     `json:"-"`
	Reply    *ChatRef   `json:"-"`
}

// ChatRef points at a Telegram message.
type ChatRef struct {
	Chat int64 `json:"chat"`
	Msg  int   `json:"msg"`
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (q *Queue) Add(n NewJob) (JobView, error) {
	u, err := ValidateURL(n.URL)
	if err != nil {
		return JobView{}, err
	}
	switch n.Engine {
	case "", EngYTDLP, EngGalleryDL:
	default:
		return JobView{}, errors.New("bilinmeyen motor")
	}
	n.Opts.normalize()
	if len(n.Opts.Items) > 0 || len(n.Opts.Sections) > 0 {
		n.Engine = EngGalleryDL
	}
	j := &Job{
		ID: newID(), URL: u.String(), Host: normHost(u), Engine: n.Engine,
		Title: clip(strings.TrimSpace(n.Title), 300), Thumb: n.Thumb, Expected: n.Expected,
		Opts: n.Opts, Status: StQueued, Created: time.Now(), Progress: 0, Sub: n.Sub, Source: n.Source, Reply: n.Reply,
	}
	j.Title = cmp.Or(j.Title, j.URL)
	if !strings.HasPrefix(j.Thumb, "http://") && !strings.HasPrefix(j.Thumb, "https://") {
		j.Thumb = ""
	}
	q.mu.Lock()
	q.jobs[j.ID] = j
	q.pruneLocked()
	q.publishLocked(j, true)
	v := q.view(j)
	q.mu.Unlock()
	q.requestSave()
	q.poke()
	return v, nil
}

// pruneLocked keeps the job history bounded (oldest finished first).
func (q *Queue) pruneLocked() {
	if len(q.jobs) <= maxJobs {
		return
	}
	for _, j := range q.sortedLocked() {
		if len(q.jobs) <= maxJobs {
			return
		}
		if j.finished() {
			delete(q.jobs, j.ID)
			q.hub.Publish("removed", map[string]string{"id": j.ID})
		}
	}
}

func (q *Queue) Cancel(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return errors.New("iş bulunamadı")
	}
	switch j.Status {
	case StRunning:
		j.userStop = true
		if j.cancel != nil {
			j.cancel()
		}
	case StQueued:
		j.Status, j.Finished = StCanceled, time.Now()
		q.publishLocked(j, true)
		q.requestSave()
	}
	return nil
}

func (q *Queue) Retry(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return errors.New("iş bulunamadı")
	}
	if j.Status == StRunning || j.Status == StQueued {
		return nil
	}
	j.Status, j.Error, j.Note, j.Attempts, j.Progress, j.Phase = StQueued, "", "", 0, 0, ""
	j.NotBefore, j.Finished, j.Expired = time.Time{}, time.Time{}, false
	j.Files, j.Existing, j.Crashes = nil, nil, 0
	q.publishLocked(j, true)
	q.requestSave()
	q.poke()
	return nil
}

// Remove deletes a job record; withFiles also deletes the downloaded files.
func (q *Queue) Remove(id string, withFiles bool) error {
	q.mu.Lock()
	j, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		return errors.New("iş bulunamadı")
	}
	if j.Status == StRunning {
		j.userStop = true
		if j.cancel != nil {
			j.cancel()
		}
	}
	delete(q.jobs, id)
	files := append([]string(nil), j.Files...)
	temp := j.Opts.Mode == "temp"
	// A file another job also lists (the same video downloaded twice, a
	// profile run again) belongs to that one too: keep it.
	shared := map[string]bool{}
	for _, o := range q.jobs {
		for _, f := range o.Files {
			shared[f] = true
		}
	}
	files = slices.DeleteFunc(files, func(f string) bool { return shared[f] })
	q.mu.Unlock()

	if temp {
		_ = os.RemoveAll(q.tempDir(id))
	} else if withFiles {
		for _, f := range files {
			if q.allowedPath(f) {
				_ = os.Remove(f)
				removeEmptyParents(filepath.Dir(f), q.cfg.Library)
			}
		}
		q.libraryChanged()
	}
	q.hub.Publish("removed", map[string]string{"id": id})
	q.requestSave()
	return nil
}

func (q *Queue) ClearFinished() {
	q.mu.Lock()
	var ids []string
	for id, j := range q.jobs {
		if j.finished() {
			ids = append(ids, id)
		}
	}
	q.mu.Unlock()
	for _, id := range ids {
		_ = q.Remove(id, false)
	}
}

func (q *Queue) tempDir(id string) string { return q.cfg.Dir("temp", id) }
func (q *Queue) partDir(id string) string { return q.cfg.Dir("parts", id) }

// allowedPath ensures we only ever serve or delete files inside our roots.
func (q *Queue) allowedPath(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	for _, root := range []string{q.cfg.Library, q.cfg.Dir("temp")} {
		r, err := filepath.Abs(root)
		if err == nil && abs != r && isWithin(r, abs) {
			return true
		}
	}
	return false
}

// applyProgress puts a yt-dlp progress line on the job. In a playlist the
// progress covers all items: item 3 of 4 at half is 62.5 %.
func applyProgress(x *Job, p progressLine) {
	x.Bytes, x.Total, x.Speed, x.ETA = p.Downloaded, p.Total, p.Speed, p.ETA
	x.Item, x.Items = p.Index, p.N
	frac := -1.0
	if p.Total > 0 {
		frac = float64(p.Downloaded) / float64(p.Total)
	}
	if p.Status == "finished" {
		frac, x.Phase = 1, "processing"
	} else {
		x.Phase = "downloading"
	}
	if p.N > 1 && p.Index > 0 {
		x.Progress = (float64(p.Index-1) + max(frac, 0)) / float64(p.N)
	} else {
		x.Progress = frac
	}
	if x.Title == x.URL && p.Title != "" && p.Title != "NA" {
		x.Title = p.Title
	}
}

// dirClaim is a folder a download is about to create: dir and its parents up
// to (not including) stop did not exist yet.
type dirClaim struct{ dir, stop string }

// claimNewDir checks the folder of file, a path yt-dlp is about to write
// below root. ok is false when that folder already exists: it may be one the
// user made, and it is never removed. yt-dlp creates the folder right after
// printing the path; if it wins that race the folder counts as existing,
// which only leaves an empty folder behind.
func claimNewDir(root, file string) (dirClaim, bool) {
	root, _ = filepath.Abs(root)
	d, err := filepath.Abs(filepath.Dir(file))
	if err != nil || d == root || !isWithin(root, d) || exists(d) {
		return dirClaim{}, false
	}
	stop := filepath.Dir(d)
	for stop != root && !exists(stop) {
		stop = filepath.Dir(stop)
	}
	return dirClaim{dir: d, stop: stop}, true
}

func removeEmptyParents(dir, stop string) {
	stop, _ = filepath.Abs(stop)
	for range 4 {
		d, _ := filepath.Abs(dir)
		if d == stop || !isWithin(stop, d) {
			return
		}
		if os.Remove(d) != nil {
			return
		}
		dir = filepath.Dir(d)
	}
}

// ---- scheduler ------------------------------------------------------------

func (q *Queue) Run(ctx context.Context) {
	q.ctx = ctx
	go q.saver(ctx)
	go q.janitor(ctx)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		q.schedule()
		select {
		case <-ctx.Done():
			q.shutdown()
			return
		case <-q.wake:
		case <-tick.C:
		}
	}
}

func (q *Queue) schedule() {
	qs := q.cfg.QueueSettings()
	disk := diskCheck{lib: diskFree(q.cfg.Library), tmp: diskFree(q.cfg.Dir("temp")), min: qs.MinFreeMB << 20}
	now := time.Now()
	st := QueueState{QueueSettings: qs, Free: disk.lib, Night: inWindow(now, qs.NightStart, qs.NightEnd),
		NextNight: nextStart(now, qs.NightStart, qs.NightEnd)}
	st.DiskLow = disk.low("library") || disk.low("temp")
	if !disk.low("library") && disk.low("temp") {
		st.Free = disk.tmp
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.Status == StQueued && j.Opts.Night && !st.Night {
			st.Waiting++
		}
	}
	q.updateStateLocked(st)
	running, perHost := 0, map[string]int{}
	for _, j := range q.jobs {
		if j.Status == StRunning {
			running++
			perHost[j.Host]++
		}
	}
	for _, j := range q.sortedLocked() {
		if running >= q.cfg.Concurrency {
			return
		}
		limit := q.cfg.PerHost
		if hostMatch(j.Host, sensitiveHosts) {
			limit = 1 // never hammer bot-sensitive sites in parallel
		}
		if j.Status != StQueued || now.Before(j.NotBefore) || perHost[j.Host] >= limit {
			continue
		}
		if (j.Opts.Night && !st.Night) || disk.low(j.Opts.Mode) {
			continue // the night window opens / space is freed: the next tick starts it
		}
		if j.Engine != "" && !q.eng.Ready(j.Engine) {
			continue // wait for the engine manager
		}
		if j.Engine == "" && !q.eng.Ready(EngYTDLP) && !q.eng.Ready(EngGalleryDL) {
			continue
		}
		running++
		perHost[j.Host]++
		jctx, cancel := context.WithCancel(q.ctx)
		j.Status, j.Phase, j.cancel, j.Started, j.Error = StRunning, "starting", cancel, now, ""
		j.userStop = false
		j.Attempts++
		q.publishLocked(j, true)
		slog.Info("indiriliyor", jobLogArgs(j)...)
		q.wg.Go(func() { q.execute(jctx, j.ID) })
	}
}

func (q *Queue) shutdown() {
	q.mu.Lock()
	for _, j := range q.jobs {
		if j.Status == StRunning && j.cancel != nil {
			j.cancel()
		}
	}
	q.mu.Unlock()
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	q.mu.Lock()
	for _, j := range q.jobs {
		if j.Status == StRunning {
			j.Status, j.Phase = StQueued, ""
			j.Attempts--
		}
	}
	q.mu.Unlock()
	q.saveNow()
}

func (q *Queue) saver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.saveReq:
			time.Sleep(500 * time.Millisecond)
			q.saveNow()
		}
	}
}

// janitor deletes browser-mode (temp) files after the retention window and
// cleans leftover partial-download dirs.
func (q *Queue) janitor(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		q.cleanup()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (q *Queue) cleanup() {
	ret := time.Duration(q.cfg.TempRetentionHours) * time.Hour
	q.mu.Lock()
	active := map[string]bool{}
	var expire []*Job
	for id, j := range q.jobs {
		if j.Status == StRunning || j.Status == StQueued {
			active[id] = true
		}
		if j.Opts.Mode == "temp" && !j.Expired && !j.Finished.IsZero() && time.Since(j.Finished) > ret {
			expire = append(expire, j)
		}
	}
	for _, j := range expire {
		_ = os.RemoveAll(q.tempDir(j.ID))
		j.Expired, j.Files = true, nil
		q.publishLocked(j, true)
	}
	q.mu.Unlock()
	if len(expire) > 0 {
		q.requestSave()
	}
	for _, sub := range []string{"parts", "temp"} {
		entries, _ := os.ReadDir(q.cfg.Dir(sub))
		for _, e := range entries {
			id := e.Name()
			q.mu.Lock()
			j, known := q.jobs[id]
			keep := active[id] || (sub == "temp" && known && !j.Expired)
			q.mu.Unlock()
			if !keep {
				_ = os.RemoveAll(q.cfg.Dir(sub, id))
			}
		}
	}
}

// ---- execution ------------------------------------------------------------

func (q *Queue) update(id string, force bool, fn func(j *Job)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j, ok := q.jobs[id]; ok {
		fn(j)
		q.publishLocked(j, force)
	}
}

func (q *Queue) execute(ctx context.Context, id string) {
	defer q.poke()

	q.mu.Lock()
	j, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		return
	}
	snap := *j
	q.mu.Unlock()

	q.finish(ctx, id, q.runJob(ctx, snap))
}

// finish records the outcome of a run.
func (q *Queue) finish(ctx context.Context, id string, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return
	}
	j.cancel = nil
	j.Speed, j.ETA, j.Phase = 0, 0, ""
	if ctx.Err() == nil {
		j.Crashes = 0
	}
	defer func() { logOutcome(*j, err) }() // after the switch: the final state
	switch {
	case j.userStop:
		j.Status, j.Finished = StCanceled, time.Now()
	case ctx.Err() != nil: // shutdown: Queue.shutdown re-queues it
	case errors.Is(err, errDiskLow):
		// Filled up between the scheduler's check and the start: wait again.
		j.Status, j.Error, j.Progress = StQueued, "", 0
		j.Attempts--
	case err == nil:
		j.Status, j.Progress, j.Finished = StDone, 1, time.Now()
		switch {
		case j.Opts.Mark:
			j.Note = "marked" // the newest N recorded as seen, nothing downloaded
		case len(j.Files) > 0:
		case len(j.Existing) > 0:
			j.Note = "had_all" // everything was already downloaded
		case j.Opts.NewOnly:
			// "Only new" with nothing new is a successful, empty run.
			j.Note = "no_new" // i18n key, shown by the UI
		default:
			j.Status, j.Error = StError, "indirme bitti ama dosya bulunamadı"
		}
	default:
		j.Error = err.Error()
		if j.Attempts < maxAttempts && transient(err.Error()) {
			j.Status = StQueued
			j.NotBefore = time.Now().Add(time.Duration(j.Attempts*j.Attempts) * 15 * time.Second)
		} else {
			j.Status, j.Finished = StError, time.Now()
		}
	}
	q.publishLocked(j, true)
	q.requestSave()
	if j.Status == StDone && j.Opts.Mode != "temp" && len(j.Files) > 0 {
		go q.libraryChanged() // not under q.mu
	}
	if j.finished() && q.onFinish != nil {
		go q.onFinish(*j)
	}
}

// transient: worth an automatic retry (network hiccups, throttling).
func transient(msg string) bool {
	m := strings.ToLower(msg)
	if strings.Contains(m, "service unavailable") { // HTTP 503: "unavailable" below means the video
		return true
	}
	return !containsAny(m, "unsupported url", "private", "sign in", "login", "not available",
		"removed", "copyright", "404", "403", "no video formats", "disk", "geçersiz", "bulamadı", "hazır değil",
		"unavailable", "incomplete youtube id", "does not exist", "has been terminated", "members-only",
		// gallery-dl logs "<exception class>: <message>" (job.py)
		"notfounderror", "could not be found", "authorizationerror", "authrequired")
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(x string) bool { return strings.Contains(s, x) })
}

func (q *Queue) runJob(ctx context.Context, j Job) error {
	// Browser-mode files live in the data dir, which may be on another disk.
	target := q.cfg.Library
	if j.Opts.Mode == "temp" {
		target = q.cfg.Dir("temp")
	}
	if free := diskFree(target); free >= 0 && free < q.cfg.QueueSettings().MinFreeMB<<20 {
		return errDiskLow
	}

	// Engine not chosen yet (e.g. API client added a raw URL): probe now.
	if j.Engine == "" {
		q.update(j.ID, true, func(x *Job) { x.Phase = "probing" })
		res, err := q.router.Probe(ctx, j.URL, "")
		if err != nil {
			return err
		}
		j.Engine = res.Engine
		q.update(j.ID, true, func(x *Job) {
			x.Engine, x.Phase = res.Engine, "starting"
			if x.Title == x.URL || x.Title == "" {
				x.Title = res.Title
			}
			x.Thumb = cmp.Or(x.Thumb, res.Thumbnail)
			if res.Engine == EngGalleryDL {
				x.Expected = res.Count
			}
			if res.IsPlaylist {
				x.Opts.Playlist = true
			}
			// A profile sent from outside (extension, Shortcut): its default
			// sections, only what is not downloaded yet.
			if res.Profile != nil && len(x.Opts.Sections) == 0 && len(x.Opts.Items) == 0 {
				x.Opts.Sections, x.Opts.NewOnly = res.Profile.Preselect, true
			}
		})
		j, _ = q.Get(j.ID)
	}

	if err := os.MkdirAll(q.cfg.Library, 0o755); err != nil {
		return fmt.Errorf("kütüphane klasörü oluşturulamadı: %w", err)
	}
	home := q.cfg.Library
	if j.Opts.Mode == "temp" {
		home = q.tempDir(j.ID)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}

	err := q.runEngine(ctx, j, home)
	if err == nil || ctx.Err() != nil {
		return err
	}
	// The router's guess can be wrong (e.g. an Instagram image post that
	// yt-dlp probes fine but cannot download). Switch engine once, in place.
	alt := fallbackEngine(j.Engine, err.Error())
	if alt != "" && !q.eng.Ready(alt) {
		return fmt.Errorf("%s gerekiyordu ama hazır değil (Ayarlar → Motorlar). Asıl hata: %s", alt, firstLine(err.Error()))
	}
	if alt != "" {
		slog.Info("diğer motorla deneniyor", jobLogArgs(&j, "eski", j.Engine, "yeni", alt, "sebep", firstLine(err.Error()))...)
		j.Engine = alt
		q.update(j.ID, true, func(x *Job) {
			x.Engine, x.Phase, x.Progress, x.Files = alt, "starting", 0, nil
			if alt == EngGalleryDL {
				x.Expected, x.Items, x.Item = 0, 0, 0
			}
		})
		err2 := q.runEngine(ctx, j, home)
		if err2 == nil || ctx.Err() != nil {
			return err2
		}
		err = err2
	}
	return withLoginHint(j.Host, err)
}

func (q *Queue) runEngine(ctx context.Context, j Job, home string) error {
	switch j.Engine {
	case EngYTDLP:
		return q.runYTDLP(ctx, j, home)
	case EngGalleryDL:
		return q.runGalleryDL(ctx, j, home)
	}
	return errors.New("bilinmeyen motor")
}

// fallbackEngine decides whether a failure means "wrong engine" rather than
// "this content is unavailable".
func fallbackEngine(engine, msg string) string {
	m := strings.ToLower(msg)
	has := func(s ...string) bool { return containsAny(m, s...) }
	switch engine {
	case EngYTDLP:
		if has("no video formats", "there is no video", "no video in this", "unsupported url",
			"requested format is not available", "no media found", "is not a video") {
			return EngGalleryDL
		}
	case EngGalleryDL:
		if has("no suitable extractor", "unsupported url", "no results for") {
			return EngYTDLP
		}
	}
	return ""
}

// isLoginError: the site wants a (fresh) login, or is throttling anonymous use.
func isLoginError(err error) bool {
	return containsAny(strings.ToLower(err.Error()), "login", "log in", "sign in", "cookies", "authenticat", "401",
		"accounts/login", "rate-limit", "rate limit", "checkpoint", "private")
}

// withLoginHint turns authentication failures into an actionable message.
func withLoginHint(host string, err error) error {
	if isLoginError(err) {
		return fmt.Errorf("%s giriş istiyor: Ayarlar → Çerezler'e %s için cookies.txt ekle. (%s)",
			host, siteOf(host), firstLine(err.Error()))
	}
	return err
}

func (q *Queue) startCmd(ctx context.Context, bin string, args []string) (*exec.Cmd, io.Reader, *tailBuffer, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = q.eng.Env()
	setProcAttrs(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	tail := &tailBuffer{max: 40}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdout, tail, nil
}

func (q *Queue) runYTDLP(ctx context.Context, j Job, home string) error {
	bin, err := q.eng.Need(EngYTDLP)
	if err != nil {
		return err
	}
	part := q.partDir(j.ID)
	_ = os.MkdirAll(part, 0o755)
	_ = os.MkdirAll(q.cfg.Dir("archive"), 0o755)
	ff := ""
	if p := q.eng.Path("ffmpeg"); p != "" {
		ff = filepath.Dir(p)
	}
	ck, done := q.cookies.Use(j.Host)
	defer done()
	args := ytdlpArgs(j.Opts, ytdlpPaths{
		Home: home, Temp: part, FFmpegDir: ff,
		Cookies: ck, SubLangs: q.cfg.SubLangs,
		Pace: q.cfg.Pacing(), Sensitive: hostMatch(j.Host, sensitiveHosts),
		Archive:   q.cfg.Dir("archive", "yt-dlp.txt"),
		RateLimit: rateFor(q.cfg.QueueSettings().RateLimitMB, q.cfg.Concurrency),
		Flat:      q.cfg.Storage().Layout == "flat",
	}, j.URL)

	cmd, stdout, tail, err := q.startCmd(ctx, bin, args)
	if err != nil {
		return err
	}
	var created []dirClaim // folders this run makes (yt-dlp creates them before downloading)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if f, ok := strings.CutPrefix(line, dirPrefix); ok {
			if c, ok := claimNewDir(home, strings.TrimSpace(f)); ok {
				created = append(created, c)
			}
			continue
		}
		if p, ok := parseProgress(line); ok {
			q.update(j.ID, p.Status == "finished", func(x *Job) { applyProgress(x, p) })
			continue
		}
		if f, ok := strings.CutPrefix(line, filePrefix); ok {
			f = strings.TrimSpace(f)
			if f != "" && f != "NA" {
				q.update(j.ID, true, func(x *Job) { x.Files = appendUnique(x.Files, f) })
			}
			continue
		}
		if strings.HasPrefix(line, markPrefix) {
			q.update(j.ID, false, func(x *Job) { x.Marked++ })
		}
	}
	drain(stdout)
	err = cmd.Wait()
	_ = os.RemoveAll(part)
	// A failed or canceled run leaves its folders empty: remove them. Only
	// empty ones go, so a successful run keeps its files' folders.
	for _, c := range created {
		removeEmptyParents(c.dir, c.stop)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		cur, _ := q.Get(j.ID)
		// Playlists may partially fail; keep what succeeded.
		if j.Opts.Playlist && len(cur.Files)+cur.Marked > 0 {
			return nil
		}
		return engineError(tail.String(), err)
	}
	return nil
}

func (q *Queue) runGalleryDL(ctx context.Context, j Job, home string) error {
	bin, err := q.eng.Need(EngGalleryDL)
	if err != nil {
		return err
	}
	urls := []string{j.URL}
	if len(j.Opts.Items) > 0 {
		urls = j.Opts.Items
	} else if u, err := url.Parse(j.URL); err == nil && len(j.Opts.Sections) > 0 {
		if user, _, ok := parseInstagramProfile(u); ok {
			urls = nil
			for _, sec := range j.Opts.Sections {
				urls = append(urls, igSectionURL(user, sec))
			}
		}
	}
	_ = os.MkdirAll(q.cfg.Dir("archive"), 0o755)
	ck, done := q.cookies.Use(j.Host)
	defer done()
	args := gallerydlArgs(j.Opts, gallerydlRun{
		Dir: home, Flat: j.Opts.Mode == "temp" || q.cfg.Storage().Layout == "flat", Cookies: ck,
		Pace: q.cfg.Pacing(), Archive: q.cfg.Dir("archive", "gallery-dl.sqlite3"), URLs: urls,
		RateLimit: rateFor(q.cfg.QueueSettings().RateLimitMB, q.cfg.Concurrency),
	})
	cmd, stdout, tail, err := q.startCmd(ctx, bin, args)
	if err != nil {
		return err
	}
	q.update(j.ID, true, func(x *Job) {
		x.Phase, x.Items = "downloading", x.Expected
		if x.Expected <= 0 {
			x.Progress = -1
		}
	})
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		p, skipped := parseGalleryLine(sc.Text())
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(home, p)
		}
		if skipped && j.Opts.Mark { // "# name": listed and recorded, not downloaded
			q.update(j.ID, false, func(x *Job) { x.Marked++ })
			continue
		}
		if skipped { // "# path": downloaded earlier, by another job
			q.update(j.ID, false, func(x *Job) { x.Existing = appendUnique(x.Existing, p) })
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		q.update(j.ID, false, func(x *Job) {
			x.Files = appendUnique(x.Files, p)
			x.Item = len(x.Files)
			if x.Items > 0 && x.Item > x.Items {
				// The probe only counts the first probeLimit items: the
				// real total is unknown, so stop showing "75/60".
				x.Items, x.Progress = 0, -1
			}
			if x.Items > 0 {
				x.Progress = min(float64(x.Item)/float64(x.Items), 0.99)
			}
		})
	}
	drain(stdout)
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cur, _ := q.Get(j.ID)
	if err != nil && len(cur.Files)+cur.Marked == 0 {
		return engineError(tail.String(), err)
	}
	return nil
}

func appendUnique(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// drain empties a pipe the scanner gave up on (a line over its buffer):
// otherwise the engine blocks on a full pipe and Wait never returns.
func drain(r io.Reader) { _, _ = io.Copy(io.Discard, r) }

// tailBuffer keeps the last N lines written to it (stderr capture).
type tailBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
	part  string
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.part + string(p)
	parts := strings.Split(s, "\n")
	t.part = parts[len(parts)-1]
	if len(t.part) > 4096 {
		t.part = t.part[len(t.part)-4096:]
	}
	for _, l := range parts[:len(parts)-1] {
		t.lines = append(t.lines, strings.TrimRight(l, "\r"))
	}
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(append(append([]string{}, t.lines...), t.part), "\n")
}

// jobLogArgs names a job in the log: its title once known, else the link.
func jobLogArgs(j *Job, more ...any) []any {
	args := []any{"site", j.Host}
	if j.Title != "" && j.Title != j.URL {
		args = append(args, "başlık", clip(j.Title, 70))
	} else {
		args = append(args, "adres", clip(j.URL, 90))
	}
	return append(args, more...)
}

// logOutcome writes one line when a run ends (deferred in finish, so it
// sees the final state).
func logOutcome(j Job, err error) {
	switch j.Status {
	case StDone:
		var size int64
		for _, f := range j.Files {
			if st, err := os.Stat(f); err == nil {
				size += st.Size()
			}
		}
		more := []any{"dosya", len(j.Files), "boyut", humanBytes(size)}
		if !j.Started.IsZero() {
			more = append(more, "süre", durationText(j.Finished.Sub(j.Started)))
		}
		if j.Note == "no_new" {
			more = []any{"sonuç", "yeni öğe yok"}
		}
		slog.Info("indirildi", jobLogArgs(&j, more...)...)
	case StError:
		slog.Warn("indirilemedi", jobLogArgs(&j, "hata", strings.TrimPrefix(firstLine(j.Error), "ERROR: "))...)
	case StCanceled:
		slog.Info("durduruldu", jobLogArgs(&j)...)
	case StQueued:
		switch {
		case errors.Is(err, errDiskLow):
			slog.Warn("disk dolu, indirme yer açılınca başlayacak", jobLogArgs(&j)...)
		case err != nil:
			slog.Info("yeniden denenecek", jobLogArgs(&j, "deneme", j.Attempts+1, "hata", firstLine(err.Error()))...)
		}
	}
}

// Deliverable: the files to hand over (phone, Telegram): this run's, or the
// ones that were already there when it found nothing new to download.
func (j Job) Deliverable() []string {
	if len(j.Files) > 0 {
		return j.Files
	}
	return j.Existing
}
