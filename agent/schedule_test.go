package main

import "testing"

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
