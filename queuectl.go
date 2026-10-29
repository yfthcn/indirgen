package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// Queue controls: a total speed cap, a night window for jobs marked "night"
// and the low-disk guard. The scheduler applies them before starting a job;
// a job never fails for lack of space, it waits until space is freed.
// ---------------------------------------------------------------------------

type QueueSettings struct {
	RateLimitMB int    `json:"rate_limit_mb"`
	NightStart  string `json:"night_start"`
	NightEnd    string `json:"night_end"`
	MinFreeMB   int64  `json:"min_free_mb"`
}

// QueueState is shown above the job list.
type QueueState struct {
	QueueSettings
	DiskLow   bool      `json:"disk_low"`
	Free      int64     `json:"free"`       // bytes free where the next job writes, -1 unknown
	Night     bool      `json:"night"`      // inside the night window now
	NextNight time.Time `json:"next_night"` // next window start
	Waiting   int       `json:"waiting"`    // queued jobs held for the night
}

var hmRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// parseHM turns "01:30" into minutes after midnight.
func parseHM(s string) (int, bool) {
	m := hmRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h*60 + mi, true
}

// inWindow: t is inside [start, end), which may cross midnight (23:00–06:00).
func inWindow(t time.Time, start, end string) bool {
	s, ok1 := parseHM(start)
	e, ok2 := parseHM(end)
	if !ok1 || !ok2 || s == e {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if s < e {
		return m >= s && m < e
	}
	return m >= s || m < e
}

// nextStart is when the window opens next; while it is open, when the
// current one opened. It must stay the same between scheduler ticks: the
// UI is told about every change of it.
func nextStart(t time.Time, start, end string) time.Time {
	s, _ := parseHM(start)
	d := time.Date(t.Year(), t.Month(), t.Day(), s/60, s%60, 0, 0, t.Location())
	if inWindow(t, start, end) {
		if d.After(t) { // opened yesterday evening (23:00–06:00, now 03:00)
			d = d.AddDate(0, 0, -1)
		}
		return d
	}
	if !d.After(t) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

func (c *Config) QueueSettings() QueueSettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return QueueSettings{RateLimitMB: c.RateLimitMB, NightStart: c.NightStart, NightEnd: c.NightEnd, MinFreeMB: c.MinFreeMB}
}

func (c *Config) SetQueueSettings(s QueueSettings) error {
	if s.RateLimitMB < 0 || s.RateLimitMB > 1000 {
		return errors.New("hız sınırı 0–1000 MB/s olmalı")
	}
	if _, ok := parseHM(s.NightStart); !ok {
		return errors.New("gece başlangıcı SS:DD olmalı")
	}
	if _, ok := parseHM(s.NightEnd); !ok || s.NightEnd == s.NightStart {
		return errors.New("gece bitişi SS:DD olmalı ve başlangıçtan farklı olmalı")
	}
	if s.MinFreeMB < 100 || s.MinFreeMB > 1<<20 {
		return errors.New("en az boş alan 100 MB – 1 TB arası olmalı")
	}
	c.mu.Lock()
	c.RateLimitMB, c.NightStart, c.NightEnd, c.MinFreeMB = s.RateLimitMB, s.NightStart, s.NightEnd, s.MinFreeMB
	c.mu.Unlock()
	return c.Save()
}

// rateFor splits the total cap between the parallel downloads.
func rateFor(totalMB, parallel int) string {
	if totalMB <= 0 {
		return ""
	}
	return fmt.Sprintf("%dK", max(totalMB*1024/max(parallel, 1), 64))
}

var errDiskLow = errors.New("disk dolmak üzere")

// diskCheck reports free bytes for the library and the temp dir and whether
// a job of the given mode may start.
type diskCheck struct {
	lib, tmp, min int64
}

func (d diskCheck) low(mode string) bool {
	f := d.lib
	if mode == "temp" {
		f = d.tmp
	}
	return f >= 0 && f < d.min
}

func (q *Queue) State() QueueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state
}

// updateStateLocked stores the new state and tells the UI when something
// the user sees changed (not on every byte of free space).
func (q *Queue) updateStateLocked(st QueueState) {
	old := q.state
	q.state = st
	if old.DiskLow != st.DiskLow || old.Night != st.Night || old.Waiting != st.Waiting ||
		old.QueueSettings != st.QueueSettings || !old.NextNight.Equal(st.NextNight) {
		q.hub.Publish("queue", st)
	}
	if st.DiskLow && !old.DiskLow && q.onDiskLow != nil {
		go q.onDiskLow(st)
	}
}

// StartNow lifts the night hold of a queued job.
func (q *Queue) StartNow(id string) error {
	q.mu.Lock()
	j, ok := q.jobs[id]
	if !ok || j.Status != StQueued {
		q.mu.Unlock()
		return errors.New("kuyrukta bekleyen böyle bir iş yok")
	}
	j.Opts.Night = false
	q.publishLocked(j, true)
	q.mu.Unlock()
	q.requestSave()
	q.poke()
	return nil
}
