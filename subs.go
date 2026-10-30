package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Subscriptions: a channel, playlist, profile or gallery checked on a
// schedule. Each check queues an ordinary job with "only new" (download
// archive) and a limit of the newest N items, so a check costs a short
// listing and downloads only what is new. Checks run while İndirgen runs;
// one that came due while it was off runs at the next start.
// ---------------------------------------------------------------------------

var subIntervals = []int{1, 3, 6, 12, 24, 72, 168} // hours
var subLimits = []int{5, 10, 25, 50, 100}

type Subscription struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Host      string     `json:"host"`
	Engine    string     `json:"engine"`
	Title     string     `json:"title"`
	Thumb     string     `json:"thumb,omitempty"`
	Opts      JobOptions `json:"opts"`
	IntervalH int        `json:"interval_h"`
	Paused    bool       `json:"paused,omitempty"`
	Created   time.Time  `json:"created"`
	LastCheck time.Time  `json:"last_check,omitzero"`
	Next      time.Time  `json:"next,omitzero"`
	LastJob   string     `json:"last_job,omitempty"`
	LastNew   int        `json:"last_new"`
	LastError string     `json:"last_error,omitempty"`
	TotalNew  int        `json:"total_new"`
	// "Only what comes next": the next check records the newest N as seen
	// instead of downloading them. LastMarked is what that check recorded.
	MarkPending bool `json:"mark_pending,omitempty"`
	LastMarked  int  `json:"last_marked,omitempty"`
	Running     bool `json:"running,omitempty"` // a job of it is queued or running (not stored)
}

type NewSub struct {
	URL       string     `json:"url"`
	Engine    string     `json:"engine"`
	Title     string     `json:"title"`
	Thumb     string     `json:"thumb"`
	Opts      JobOptions `json:"opts"`
	IntervalH int        `json:"interval_h"`
	FromNow   bool       `json:"from_now"` // download nothing that is there now, only what comes next
}

type SubPatch struct {
	IntervalH *int  `json:"interval_h"`
	Limit     *int  `json:"limit"`
	Paused    *bool `json:"paused"`
}

type Subs struct {
	cfg   *Config
	q     *Queue
	hub   *Hub
	mu    sync.Mutex
	items map[string]*Subscription
	wake  chan struct{}
	now   func() time.Time // tests
}

func NewSubs(cfg *Config, q *Queue, hub *Hub) *Subs {
	s := &Subs{cfg: cfg, q: q, hub: hub, items: map[string]*Subscription{}, wake: make(chan struct{}, 1), now: time.Now}
	s.load()
	return s
}

func (s *Subs) file() string { return s.cfg.Dir("subscriptions.json") }

func (s *Subs) load() {
	raw, err := os.ReadFile(s.file())
	if err != nil {
		return
	}
	var list []*Subscription
	if err := json.Unmarshal(raw, &list); err != nil {
		slog.Warn("subscriptions.json okunamadı", "hata", err)
		return
	}
	for _, x := range list {
		x.Running = false
		s.items[x.ID] = x
	}
}

func (s *Subs) saveLocked() {
	raw, _ := json.MarshalIndent(s.sortedLocked(), "", "  ")
	if err := writeFileAtomic(s.file(), raw, 0o600); err != nil {
		slog.Warn("subscriptions.json yazılamadı", "hata", err)
	}
}

func (s *Subs) sortedLocked() []*Subscription {
	list := make([]*Subscription, 0, len(s.items))
	for _, x := range s.items {
		list = append(list, x)
	}
	slices.SortFunc(list, func(a, b *Subscription) int { return a.Created.Compare(b.Created) })
	return list
}

// List returns the subscriptions with their live "running" state.
func (s *Subs) List() []Subscription {
	active := s.q.ActiveSubs()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Subscription{}
	for _, x := range s.sortedLocked() {
		v := *x
		v.Running = active[x.ID]
		out = append(out, v)
	}
	return out
}

func (s *Subs) publish() { s.hub.Publish("subs", s.List()) }

func nearest(list []int, v, def int) int {
	if v <= 0 {
		return def
	}
	best := list[0]
	for _, x := range list {
		if abs(x-v) < abs(best-v) {
			best = x
		}
	}
	return best
}

func abs(x int) int { return max(x, -x) }

// Add subscribes to a link, or updates the subscription that already has it.
// The first check runs right away.
func (s *Subs) Add(n NewSub) (Subscription, error) {
	u, err := ValidateURL(n.URL)
	if err != nil {
		return Subscription{}, err
	}
	switch n.Engine {
	case "", EngYTDLP, EngGalleryDL:
	default:
		return Subscription{}, errors.New("bilinmeyen motor")
	}
	o := n.Opts
	o.NewOnly, o.Mode, o.Items = true, "library", nil
	o.Limit = nearest(subLimits, o.Limit, 10)
	o.normalize()
	if len(o.Sections) > 0 {
		n.Engine = EngGalleryDL
	}
	if n.Engine == EngYTDLP && !strings.Contains(u.Path, "/watch") {
		o.Playlist = true // channels and playlists: every entry, newest first
	}
	now := s.now()
	s.mu.Lock()
	var x *Subscription
	for _, it := range s.items {
		if it.URL == u.String() {
			x = it
		}
	}
	if x == nil {
		x = &Subscription{ID: newID(), URL: u.String(), Host: normHost(u), Created: now}
		s.items[x.ID] = x
	}
	x.Engine, x.Opts, x.Paused = n.Engine, o, false
	x.Title = clip(strings.TrimSpace(n.Title), 200)
	x.Title = cmp.Or(x.Title, x.URL)
	if strings.HasPrefix(n.Thumb, "https://") || strings.HasPrefix(n.Thumb, "http://") {
		x.Thumb = n.Thumb
	}
	x.IntervalH = nearest(subIntervals, n.IntervalH, 6)
	x.MarkPending = n.FromNow
	x.Next = now // check now
	s.saveLocked()
	v := *x
	s.mu.Unlock()
	s.publish()
	s.poke()
	return v, nil
}

func (s *Subs) Patch(id string, p SubPatch) error {
	s.mu.Lock()
	x, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return errors.New("abonelik bulunamadı")
	}
	if p.IntervalH != nil {
		x.IntervalH = nearest(subIntervals, *p.IntervalH, x.IntervalH)
		if !x.LastCheck.IsZero() {
			x.Next = x.LastCheck.Add(time.Duration(x.IntervalH) * time.Hour)
		}
	}
	if p.Limit != nil {
		x.Opts.Limit = nearest(subLimits, *p.Limit, x.Opts.Limit)
	}
	if p.Paused != nil {
		x.Paused = *p.Paused
		if !x.Paused && x.Next.Before(s.now()) {
			x.Next = s.now()
		}
	}
	s.saveLocked()
	s.mu.Unlock()
	s.publish()
	s.poke()
	return nil
}

func (s *Subs) Delete(id string) error {
	s.mu.Lock()
	_, ok := s.items[id]
	delete(s.items, id)
	if ok {
		s.saveLocked()
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("abonelik bulunamadı")
	}
	s.publish()
	return nil
}

// CheckNow queues a check regardless of the schedule.
func (s *Subs) CheckNow(id string) error {
	s.mu.Lock()
	x, ok := s.items[id]
	if ok {
		x.Next = s.now()
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("abonelik bulunamadı")
	}
	s.poke()
	return nil
}

func (s *Subs) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run queues due checks until ctx ends.
func (s *Subs) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

func (s *Subs) tick() {
	active := s.q.ActiveSubs()
	now := s.now()
	var due []Subscription
	s.mu.Lock()
	for _, x := range s.items {
		if x.Paused || active[x.ID] || now.Before(x.Next) {
			continue
		}
		x.LastCheck = now
		x.Next = now.Add(time.Duration(x.IntervalH) * time.Hour)
		due = append(due, *x)
	}
	if len(due) > 0 {
		s.saveLocked()
	}
	s.mu.Unlock()

	for _, x := range due {
		o := x.Opts
		o.Mark = x.MarkPending
		v, err := s.q.Add(NewJob{URL: x.URL, Engine: x.Engine, Title: x.Title, Thumb: x.Thumb, Opts: o, Sub: x.ID, Source: "sub"})
		s.mu.Lock()
		if it, ok := s.items[x.ID]; ok {
			if err != nil {
				it.LastError = err.Error()
			} else {
				it.LastJob = v.ID
			}
		}
		s.saveLocked()
		s.mu.Unlock()
	}
	if len(due) > 0 {
		s.publish()
	}
}

// jobFinished records the outcome of a subscription's check.
func (s *Subs) jobFinished(j Job) {
	if j.Sub == "" {
		return
	}
	s.mu.Lock()
	x, ok := s.items[j.Sub]
	if ok && x.LastJob == j.ID {
		// A marking check downloaded nothing: it counts as seen, not as new.
		x.LastNew, x.LastMarked = len(j.Files), 0
		if j.Opts.Mark {
			x.LastNew = 0
			if j.Status == StDone {
				x.MarkPending, x.LastMarked = false, j.Marked
			}
		}
		x.TotalNew += x.LastNew
		x.LastError = ""
		if j.Status == StError {
			x.LastError = j.Error
		}
		s.saveLocked()
	}
	s.mu.Unlock()
	if ok {
		s.publish()
	}
}
