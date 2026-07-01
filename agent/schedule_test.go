package main

import (
	"testing"
	"time"
)

func at(h, m int) time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.Local) }

func TestParseHHMM(t *testing.T) {
	ok := map[string]int{"22:00": 1320, "7:05": 425, "00:00": 0, "23:59": 1439}
	for s, want := range ok {
		if got, valid := parseHHMM(s); !valid || got != want {
			t.Errorf("parseHHMM(%q) = %d,%v; want %d,true", s, got, valid, want)
		}
	}
	for _, s := range []string{"", "24:00", "12:60", "ab", "-1:00", "99"} {
		if _, valid := parseHHMM(s); valid {
			t.Errorf("parseHHMM(%q) should be invalid", s)
		}
	}
}

func TestInSleepWindow(t *testing.T) {
	cases := []struct {
		sleep, wake string
		now         time.Time
		want        bool
	}{
		// wraps midnight: 22:00 → 07:00
		{"22:00", "07:00", at(23, 0), true},
		{"22:00", "07:00", at(3, 0), true},
		{"22:00", "07:00", at(6, 59), true},
		{"22:00", "07:00", at(22, 0), true},
		{"22:00", "07:00", at(7, 0), false},
		{"22:00", "07:00", at(12, 0), false},
		{"22:00", "07:00", at(21, 59), false},
		// same day: 01:00 → 06:00
		{"01:00", "06:00", at(3, 0), true},
		{"01:00", "06:00", at(0, 30), false},
		{"01:00", "06:00", at(6, 0), false},
		{"01:00", "06:00", at(5, 59), true},
		// degenerate / invalid → never asleep
		{"08:00", "08:00", at(8, 0), false},
		{"", "07:00", at(3, 0), false},
		{"bad", "07:00", at(3, 0), false},
	}
	for _, c := range cases {
		if got := inSleepWindow(c.now, c.sleep, c.wake); got != c.want {
			t.Errorf("inSleepWindow(%s, sleep=%s, wake=%s) = %v; want %v",
				c.now.Format("15:04"), c.sleep, c.wake, got, c.want)
		}
	}
}
