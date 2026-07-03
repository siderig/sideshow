package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The weekly scheduler: a per-weekday timeline of time→action transitions, with
// date-specific exceptions. Instead of only powering the screen, each transition
// fires a saved Action (by slug), so a node can show a lobby page at 08:00, a video
// playlist at 13:00, photos at 16:00, and sleep at 18:00. "Screen off" is the
// reserved action "@sleep"; "screen on (keep the current mode)" is "@wake".
//
// It is the ONE scheduler that owns the display timeline. The older nightly
// sleep/wake window (once a second, independent ticker in displaystate.go) is
// folded in here as an optional NightlyWindow: when enabled it contributes a daily
// {sleep→@sleep, wake→@wake} pair that is MERGED into each day's effective timeline
// (see effectiveEntriesLocked). Because resolution is "the last transition at-or-
// before now wins", an explicit per-day entry and the nightly window compose without
// conflict — and a node can never run two schedulers fighting over screen power.
//
// It uses a 30s edge-triggered tick (first tick delayed past the cold-boot window)
// that computes the action which SHOULD be on screen now and fires it once, only
// when the resolved transition changes — so a manual mode switch between transitions
// sticks until the next slot.

// scheduleSleepAction powers the display off; scheduleWakeAction powers it back on
// without changing the mode (content keeps running behind a slept screen). Any other
// action value is a saved Action slug. Both are the reserved vocabulary the nightly
// window synthesizes and that operators can use directly in per-day entries.
const (
	scheduleSleepAction = "@sleep"
	scheduleWakeAction  = "@wake"
)

// ScheduleEntry is one transition: at this node-local time, put this action on.
type ScheduleEntry struct {
	At     string `json:"at"`     // "HH:MM" (24h, node-local)
	Action string `json:"action"` // a saved Action slug, or "@sleep" (display off)
}

// ScheduleException replaces a single date's entries (e.g. a holiday). Empty
// Entries means "nothing scheduled that day" → the previous day's last entry
// carries over; to blank a day, add one {"00:00","@sleep"} entry.
type ScheduleException struct {
	Date    string          `json:"date"` // "YYYY-MM-DD" (node-local)
	Entries []ScheduleEntry `json:"entries"`
}

// NightlyWindow is the simple "every day, off at sleep and back on at wake" cycle.
// It is enforced by the weekly scheduler (not a second ticker): when Enabled it
// contributes a daily {Sleep→@sleep, Wake→@wake} pair merged into each day.
type NightlyWindow struct {
	Enabled bool   `json:"enabled"`
	Sleep   string `json:"sleep,omitempty"` // "HH:MM" node-local: screen off
	Wake    string `json:"wake,omitempty"`  // "HH:MM" node-local: screen back on
}

// WeeklyScheduleInfo is the JSON for GET/POST /api/schedule/week and the snapshot.
// Days is indexed by time.Weekday (0=Sunday … 6=Saturday). Nightly is the folded-in
// nightly sleep/wake window (its own Enabled flag, independent of the weekly Enabled).
type WeeklyScheduleInfo struct {
	Enabled    bool                `json:"enabled"`
	Days       [7][]ScheduleEntry  `json:"days"`
	Exceptions []ScheduleException `json:"exceptions"`
	Nightly    NightlyWindow       `json:"nightly"`
}

type persistedWeekly struct {
	Enabled    bool                `json:"enabled"`
	Days       [7][]ScheduleEntry  `json:"days"`
	Exceptions []ScheduleException `json:"exceptions,omitempty"`
	// Nightly is a pointer so a nil (absent) block is distinguishable from a
	// present-but-disabled one — that gates the one-time migration from display.json.
	Nightly *NightlyWindow `json:"nightly,omitempty"`
}

// maxScheduleEntries bounds the entries in any one day (or exception) so a runaway
// submission can't bloat the file / the tick's per-day sort.
const maxScheduleEntries = 48

// Scheduler owns the weekly schedule, persists it (hardened atomic write, like
// state.go/playlist.go), and enforces it via fire(). fire resolves an entry's
// action (a slug or "@sleep") and is supplied by the server.
type Scheduler struct {
	path string
	fire func(action string) error

	mu         sync.Mutex
	enabled    bool
	days       [7][]ScheduleEntry
	exceptions []ScheduleException
	lastKey    string // edge trigger: the last fired transition's key

	// nightly is the folded-in nightly sleep/wake window. nightlyLoaded records
	// whether schedule.json carried a nightly block, so the display.json migration
	// runs at most once (see MigrateNightly).
	nightlyEnabled bool
	nightlySleep   string
	nightlyWake    string
	nightlyLoaded  bool

	saveMu sync.Mutex
}

func NewScheduler(cfg *Config, fire func(action string) error) *Scheduler {
	s := &Scheduler{fire: fire}
	if cfg.StateFile != "" {
		s.path = filepath.Join(filepath.Dir(cfg.StateFile), "schedule.json")
	}
	s.load()
	return s
}

func (s *Scheduler) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var p persistedWeekly
	if json.Unmarshal(b, &p) != nil {
		return
	}
	s.mu.Lock()
	s.enabled = p.Enabled
	for d := 0; d < 7; d++ {
		s.days[d] = sanitizeEntries(p.Days[d])
	}
	s.exceptions = sanitizeExceptions(p.Exceptions)
	if p.Nightly != nil {
		s.nightlyLoaded = true
		en, sl, wk := sanitizeNightly(*p.Nightly)
		s.nightlyEnabled, s.nightlySleep, s.nightlyWake = en, sl, wk
	}
	s.mu.Unlock()
}

func (s *Scheduler) save() {
	if s.path == "" {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	p := persistedWeekly{Enabled: s.enabled, Exceptions: append([]ScheduleException(nil), s.exceptions...)}
	for d := 0; d < 7; d++ {
		p.Days[d] = append([]ScheduleEntry(nil), s.days[d]...)
	}
	if s.nightlyLoaded {
		p.Nightly = &NightlyWindow{Enabled: s.nightlyEnabled, Sleep: s.nightlySleep, Wake: s.nightlyWake}
	}
	s.mu.Unlock()
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("[schedule] state dir: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[schedule] save: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[schedule] save rename: %v", err)
	}
}

// sanitizeEntries drops malformed entries (bad time / empty action) and returns
// them sorted by time-of-day, capped.
func sanitizeEntries(in []ScheduleEntry) []ScheduleEntry {
	out := make([]ScheduleEntry, 0, len(in))
	for _, e := range in {
		e.At = strings.TrimSpace(e.At)
		e.Action = strings.TrimSpace(e.Action)
		if e.Action == "" {
			continue
		}
		if _, ok := parseHHMM(e.At); !ok {
			continue
		}
		out = append(out, e)
		if len(out) >= maxScheduleEntries {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := parseHHMM(out[i].At)
		b, _ := parseHHMM(out[j].At)
		return a < b
	})
	return out
}

func sanitizeExceptions(in []ScheduleException) []ScheduleException {
	out := make([]ScheduleException, 0, len(in))
	for _, ex := range in {
		if !validScheduleDate(ex.Date) {
			continue
		}
		out = append(out, ScheduleException{Date: ex.Date, Entries: sanitizeEntries(ex.Entries)})
	}
	return out
}

// sanitizeNightly trims + validates a nightly window: a time that isn't HH:MM is
// dropped, and enabled is forced off unless both times are valid and differ.
func sanitizeNightly(n NightlyWindow) (enabled bool, sleep, wake string) {
	sleep = strings.TrimSpace(n.Sleep)
	wake = strings.TrimSpace(n.Wake)
	if _, ok := parseHHMM(sleep); !ok {
		sleep = ""
	}
	if _, ok := parseHHMM(wake); !ok {
		wake = ""
	}
	enabled = n.Enabled && sleep != "" && wake != "" && sleep != wake
	return enabled, sleep, wake
}

// validScheduleDate reports whether s is a "YYYY-MM-DD" calendar date.
func validScheduleDate(s string) bool {
	_, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	return err == nil
}

// Set validates + persists a new weekly schedule and re-arms the engine (clears
// the edge-trigger so the next tick re-evaluates immediately).
func (s *Scheduler) Set(info WeeklyScheduleInfo) (WeeklyScheduleInfo, error) {
	var days [7][]ScheduleEntry
	for d := 0; d < 7; d++ {
		for _, e := range info.Days[d] {
			if _, ok := parseHHMM(strings.TrimSpace(e.At)); !ok {
				return WeeklyScheduleInfo{}, &apiError{code: 400, err: fmt.Errorf("bad time %q (use HH:MM)", e.At)}
			}
			if strings.TrimSpace(e.Action) == "" {
				return WeeklyScheduleInfo{}, &apiError{code: 400, err: fmt.Errorf("a schedule entry needs an action")}
			}
		}
		days[d] = sanitizeEntries(info.Days[d])
	}
	for _, ex := range info.Exceptions {
		if !validScheduleDate(ex.Date) {
			return WeeklyScheduleInfo{}, &apiError{code: 400, err: fmt.Errorf("bad exception date %q (use YYYY-MM-DD)", ex.Date)}
		}
	}
	exc := sanitizeExceptions(info.Exceptions)

	if info.Nightly.Enabled {
		if _, ok := parseHHMM(strings.TrimSpace(info.Nightly.Sleep)); !ok {
			return WeeklyScheduleInfo{}, &apiError{code: 400, err: fmt.Errorf("nightly sleep time must be HH:MM (24h)")}
		}
		if _, ok := parseHHMM(strings.TrimSpace(info.Nightly.Wake)); !ok {
			return WeeklyScheduleInfo{}, &apiError{code: 400, err: fmt.Errorf("nightly wake time must be HH:MM (24h)")}
		}
		if strings.TrimSpace(info.Nightly.Sleep) == strings.TrimSpace(info.Nightly.Wake) {
			return WeeklyScheduleInfo{}, &apiError{code: 400, err: fmt.Errorf("nightly sleep and wake times cannot be equal")}
		}
	}
	nEn, nSleep, nWake := sanitizeNightly(info.Nightly)

	s.mu.Lock()
	s.enabled = info.Enabled
	s.days = days
	s.exceptions = exc
	s.nightlyEnabled, s.nightlySleep, s.nightlyWake = nEn, nSleep, nWake
	s.nightlyLoaded = true // schedule.json now owns the nightly window
	s.lastKey = ""         // force re-evaluation on the next tick
	s.mu.Unlock()
	s.save()
	return s.Info(), nil
}

// Info returns the current schedule (inner day slices are non-nil so JSON is [] not
// null).
func (s *Scheduler) Info() WeeklyScheduleInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := WeeklyScheduleInfo{
		Enabled: s.enabled,
		Nightly: NightlyWindow{Enabled: s.nightlyEnabled, Sleep: s.nightlySleep, Wake: s.nightlyWake},
	}
	for d := 0; d < 7; d++ {
		info.Days[d] = append([]ScheduleEntry{}, s.days[d]...)
	}
	info.Exceptions = append([]ScheduleException{}, s.exceptions...)
	return info
}

// dayEntriesLocked returns the explicit per-day entries for t's local date (a
// matching exception's entries, else that weekday's) — already sanitized (sorted) on
// load/set. Caller holds s.mu. Not gated on s.enabled; effectiveEntriesLocked gates.
func (s *Scheduler) dayEntriesLocked(t time.Time) []ScheduleEntry {
	ds := t.Format("2006-01-02")
	for _, ex := range s.exceptions {
		if ex.Date == ds {
			return ex.Entries
		}
	}
	return s.days[int(t.Weekday())]
}

// effectiveEntriesLocked returns the sorted transitions that apply on t's local date:
// the explicit per-day entries (when the weekly schedule is enabled) MERGED with the
// nightly window's synthetic {sleep→@sleep, wake→@wake} pair (when the nightly window
// is enabled). Returns a fresh slice (never the stored one) so the caller can't mutate
// state. Nightly transitions are placed before the explicit entries so that, at an
// identical minute, the explicit entry wins ("last transition at-or-before now"
// resolves ties to whichever is later in the sorted slice). Caller holds s.mu.
func (s *Scheduler) effectiveEntriesLocked(t time.Time) []ScheduleEntry {
	var out []ScheduleEntry
	if s.nightlyEnabled {
		out = append(out,
			ScheduleEntry{At: s.nightlySleep, Action: scheduleSleepAction},
			ScheduleEntry{At: s.nightlyWake, Action: scheduleWakeAction})
	}
	if s.enabled {
		out = append(out, s.dayEntriesLocked(t)...)
	}
	if len(out) == 0 {
		return nil
	}
	sortEntriesByTime(out)
	return out
}

// sortEntriesByTime stably sorts entries ascending by time-of-day (no validation or
// capping — inputs are already sanitized).
func sortEntriesByTime(e []ScheduleEntry) {
	sort.SliceStable(e, func(i, j int) bool {
		a, _ := parseHHMM(e[i].At)
		b, _ := parseHHMM(e[j].At)
		return a < b
	})
}

// desiredLocked computes the transition that should be active at t: the last entry
// at-or-before now today, else (before today's first entry) the previous day's last
// entry carried over past midnight. The returned key uniquely identifies the
// transition (date+time+action) so the tick fires each transition exactly once.
func (s *Scheduler) desiredLocked(t time.Time) (key, action string, ok bool) {
	nowMin := t.Hour()*60 + t.Minute()
	today := s.effectiveEntriesLocked(t)
	var pick *ScheduleEntry
	for i := range today {
		m, _ := parseHHMM(today[i].At)
		if m <= nowMin {
			pick = &today[i]
		} else {
			break // entries are sorted ascending
		}
	}
	if pick != nil {
		return t.Format("2006-01-02") + "T" + pick.At + "|" + pick.Action, pick.Action, true
	}
	// Before the first entry today → carry over the previous day's last entry.
	y := t.AddDate(0, 0, -1)
	if yEntries := s.effectiveEntriesLocked(y); len(yEntries) > 0 {
		last := yEntries[len(yEntries)-1]
		return y.Format("2006-01-02") + "T" + last.At + "|" + last.Action, last.Action, true
	}
	return "", "", false
}

// tick fires the desired transition when it changes. Edge-triggered on the resolved
// key (not wall-clock), so a manual switch between transitions is left alone until
// the next boundary. A fire error is logged; lastKey is still advanced so a broken
// action isn't retried every 30s — the next transition recovers.
func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	if !s.enabled && !s.nightlyEnabled {
		s.mu.Unlock()
		return
	}
	key, action, ok := s.desiredLocked(now)
	if !ok || key == s.lastKey {
		s.mu.Unlock()
		return
	}
	s.lastKey = key
	s.mu.Unlock()

	if s.fire == nil {
		return
	}
	if err := s.fire(action); err != nil {
		log.Printf("[schedule] %s → %s failed: %v", now.Format("15:04"), action, err)
		return
	}
	log.Printf("[schedule] %s → %s", now.Format("15:04"), action)
}

// MigrateNightly folds a legacy nightly window (persisted in display.json by the
// retired displaystate scheduler) into this scheduler — once. It is a no-op if
// schedule.json already carried a nightly block (nightlyLoaded) or there is nothing
// to migrate. Call once at startup, before Start(). Times are pre-validated by the
// display loader; sanitizeNightly re-checks defensively.
func (s *Scheduler) MigrateNightly(enabled bool, sleep, wake string) {
	s.mu.Lock()
	if s.nightlyLoaded || (sleep == "" && wake == "") {
		s.mu.Unlock()
		return
	}
	en, sl, wk := sanitizeNightly(NightlyWindow{Enabled: enabled, Sleep: sleep, Wake: wake})
	s.nightlyEnabled, s.nightlySleep, s.nightlyWake = en, sl, wk
	s.nightlyLoaded = true
	s.lastKey = ""
	s.mu.Unlock()
	s.save()
	log.Printf("[schedule] migrated nightly window %s→%s (enabled=%v) from legacy display state", sl, wk, en)
}

// Start runs the enforcement loop: a first tick after the cold-boot window, then
// every 30s (so a scheduled switch can't stack onto Chromium's fragile cold start).
func (s *Scheduler) Start() {
	go func() {
		time.Sleep(60 * time.Second)
		s.tick(time.Now())
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			s.tick(time.Now())
		}
	}()
}
