package main

import (
	"testing"
	"time"
)

// on 2026-07-06 (any weekday — tests derive the weekday from the date, so they're
// robust regardless of what day that actually is).
func schedDay(h, m int) time.Time { return time.Date(2026, 7, 6, h, m, 0, 0, time.Local) }

func TestSanitizeEntriesDropsAndSorts(t *testing.T) {
	got := sanitizeEntries([]ScheduleEntry{{At: "13:00", Action: "b"}, {At: "25:00", Action: "bad"}, {At: "08:00", Action: "a"}, {At: "09:00", Action: ""}})
	if len(got) != 2 || got[0].Action != "a" || got[1].Action != "b" {
		t.Errorf("sanitizeEntries = %+v, want [a@08:00, b@13:00]", got)
	}
}

func TestScheduleDesiredResolution(t *testing.T) {
	s := &Scheduler{enabled: true}
	wd := int(schedDay(0, 0).Weekday())
	s.days[wd] = sanitizeEntries([]ScheduleEntry{{At: "18:00", Action: "@sleep"}, {At: "08:00", Action: "lobby"}, {At: "13:00", Action: "news"}})

	// Before the first entry with no previous-day schedule → nothing desired yet.
	if _, _, ok := s.desiredLocked(schedDay(7, 0)); ok {
		t.Error("07:00 with an empty previous day should resolve to not-ok")
	}
	check := func(h, m int, want string) {
		t.Helper()
		_, a, ok := s.desiredLocked(schedDay(h, m))
		if !ok || a != want {
			t.Errorf("%02d:%02d → %q (ok=%v), want %q", h, m, a, ok, want)
		}
	}
	check(8, 0, "lobby")
	check(12, 30, "lobby")
	check(13, 0, "news")
	check(17, 59, "news")
	check(18, 0, "@sleep")
	check(23, 30, "@sleep")

	// Carry-over past midnight: the previous day's last entry applies at 02:00.
	pd := int(schedDay(0, 0).AddDate(0, 0, -1).Weekday())
	s.days[pd] = sanitizeEntries([]ScheduleEntry{{At: "20:00", Action: "evening"}})
	check(2, 0, "evening")
}

func TestScheduleTickEdgeTrigger(t *testing.T) {
	var fired []string
	s := &Scheduler{enabled: true, fire: func(a string) error { fired = append(fired, a); return nil }}
	wd := int(schedDay(0, 0).Weekday())
	s.days[wd] = sanitizeEntries([]ScheduleEntry{{At: "08:00", Action: "A"}, {At: "13:00", Action: "B"}})

	s.tick(schedDay(7, 59))  // before first, previous day empty → no fire
	s.tick(schedDay(8, 0))   // → A
	s.tick(schedDay(10, 0))  // same transition (a manual override window) → NO re-fire
	s.tick(schedDay(12, 59)) // still A
	s.tick(schedDay(13, 0))  // → B
	s.tick(schedDay(15, 0))  // still B

	if len(fired) != 2 || fired[0] != "A" || fired[1] != "B" {
		t.Errorf("fired = %v, want [A B] (each transition once)", fired)
	}
}

func TestScheduleException(t *testing.T) {
	s := &Scheduler{enabled: true}
	wd := int(schedDay(0, 0).Weekday())
	s.days[wd] = sanitizeEntries([]ScheduleEntry{{At: "08:00", Action: "weekday"}})
	s.exceptions = sanitizeExceptions([]ScheduleException{{Date: "2026-07-06", Entries: []ScheduleEntry{{At: "08:00", Action: "holiday"}}}})

	if _, a, ok := s.desiredLocked(schedDay(10, 0)); !ok || a != "holiday" {
		t.Errorf("exception date → %q (ok=%v), want holiday", a, ok)
	}
	// The same weekday one week later has no exception → the weekday entry applies.
	other := schedDay(10, 0).AddDate(0, 0, 7)
	if _, a, ok := s.desiredLocked(other); !ok || a != "weekday" {
		t.Errorf("non-exception same weekday → %q (ok=%v), want weekday", a, ok)
	}
}

func TestSanitizeNightly(t *testing.T) {
	cases := []struct {
		in                NightlyWindow
		wantEn            bool
		wantSleep, wantWk string
	}{
		{NightlyWindow{true, "22:00", "07:00"}, true, "22:00", "07:00"},
		{NightlyWindow{true, "22:00", "22:00"}, false, "22:00", "22:00"}, // equal → off
		{NightlyWindow{true, "bad", "07:00"}, false, "", "07:00"},        // bad time dropped
		{NightlyWindow{false, "22:00", "07:00"}, false, "22:00", "07:00"}, // disabled keeps times
	}
	for _, c := range cases {
		en, sl, wk := sanitizeNightly(c.in)
		if en != c.wantEn || sl != c.wantSleep || wk != c.wantWk {
			t.Errorf("sanitizeNightly(%+v) = %v,%q,%q; want %v,%q,%q", c.in, en, sl, wk, c.wantEn, c.wantSleep, c.wantWk)
		}
	}
}

// A nightly-only schedule (weekly disabled) still resolves via the synthesized
// {sleep→@sleep, wake→@wake} pair, including carry-over past midnight.
func TestNightlyWindowOnly(t *testing.T) {
	s := &Scheduler{enabled: false, nightlyEnabled: true, nightlySleep: "22:00", nightlyWake: "07:00"}
	check := func(h, m int, want string) {
		t.Helper()
		if _, a, ok := s.desiredLocked(schedDay(h, m)); !ok || a != want {
			t.Errorf("%02d:%02d → %q (ok=%v), want %q", h, m, a, ok, want)
		}
	}
	check(7, 0, scheduleWakeAction)   // wake
	check(12, 0, scheduleWakeAction)  // still awake
	check(22, 0, scheduleSleepAction) // sleep
	check(23, 30, scheduleSleepAction)
	check(3, 0, scheduleSleepAction) // carry the previous night's @sleep past midnight
	check(6, 59, scheduleSleepAction)
}

// The nightly pair MERGES with explicit per-day entries; "last transition ≤ now"
// resolves the blend, and an explicit entry wins over the nightly pair at an equal minute.
func TestNightlyMergedWithWeekly(t *testing.T) {
	s := &Scheduler{enabled: true, nightlyEnabled: true, nightlySleep: "22:00", nightlyWake: "07:00"}
	wd := int(schedDay(0, 0).Weekday())
	s.days[wd] = sanitizeEntries([]ScheduleEntry{{At: "08:00", Action: "lobby"}, {At: "13:00", Action: "news"}})
	check := func(h, m int, want string) {
		t.Helper()
		if _, a, ok := s.desiredLocked(schedDay(h, m)); !ok || a != want {
			t.Errorf("%02d:%02d → %q (ok=%v), want %q", h, m, a, ok, want)
		}
	}
	check(7, 30, scheduleWakeAction) // nightly wake before the first content entry
	check(8, 0, "lobby")
	check(13, 0, "news")
	check(21, 0, "news")              // content holds until the nightly sleep
	check(22, 0, scheduleSleepAction) // nightly sleep
	check(23, 30, scheduleSleepAction)

	// Tie-break: an explicit entry at the exact nightly-sleep minute wins.
	s2 := &Scheduler{enabled: true, nightlyEnabled: true, nightlySleep: "22:00", nightlyWake: "07:00"}
	s2.days[wd] = sanitizeEntries([]ScheduleEntry{{At: "22:00", Action: "movie"}})
	if _, a, ok := s2.desiredLocked(schedDay(22, 30)); !ok || a != "movie" {
		t.Errorf("explicit entry at the nightly-sleep minute → %q (ok=%v), want movie", a, ok)
	}
}

// effectiveEntriesLocked must return a fresh slice, never mutate the stored day.
func TestEffectiveEntriesNoMutation(t *testing.T) {
	s := &Scheduler{enabled: true, nightlyEnabled: true, nightlySleep: "23:00", nightlyWake: "06:00"}
	wd := int(schedDay(0, 0).Weekday())
	s.days[wd] = sanitizeEntries([]ScheduleEntry{{At: "08:00", Action: "a"}, {At: "13:00", Action: "b"}})
	_ = s.effectiveEntriesLocked(schedDay(12, 0))
	if len(s.days[wd]) != 2 || s.days[wd][0].Action != "a" || s.days[wd][1].Action != "b" {
		t.Errorf("stored day was mutated: %+v", s.days[wd])
	}
}

// Neither portion enabled → nothing to resolve, tick fires nothing.
func TestScheduleAllDisabled(t *testing.T) {
	var fired []string
	s := &Scheduler{fire: func(a string) error { fired = append(fired, a); return nil }}
	s.tick(schedDay(22, 0))
	s.tick(schedDay(7, 0))
	if len(fired) != 0 {
		t.Errorf("fired = %v, want none when both weekly and nightly are off", fired)
	}
}

func TestMigrateNightly(t *testing.T) {
	// Fresh scheduler (no schedule.json nightly): migrate the legacy window in.
	s := &Scheduler{}
	s.MigrateNightly(true, "22:00", "07:00")
	if !s.nightlyEnabled || s.nightlySleep != "22:00" || s.nightlyWake != "07:00" || !s.nightlyLoaded {
		t.Fatalf("after migrate: en=%v sleep=%q wake=%q loaded=%v", s.nightlyEnabled, s.nightlySleep, s.nightlyWake, s.nightlyLoaded)
	}
	// A second migrate is ignored (nightlyLoaded gates it).
	s.MigrateNightly(true, "01:00", "02:00")
	if s.nightlySleep != "22:00" {
		t.Errorf("second migrate overwrote nightly (%q); should be ignored", s.nightlySleep)
	}
	// schedule.json already owned nightly → no migration.
	s2 := &Scheduler{nightlyLoaded: true}
	s2.MigrateNightly(true, "22:00", "07:00")
	if s2.nightlyEnabled {
		t.Errorf("migrate ran despite nightlyLoaded=true")
	}
	// Nothing to migrate (no legacy window) → stays unset.
	s3 := &Scheduler{}
	s3.MigrateNightly(false, "", "")
	if s3.nightlyLoaded {
		t.Errorf("migrate marked loaded for an empty legacy window")
	}
}
