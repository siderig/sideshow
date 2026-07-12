package main

import (
	"strings"
	"testing"
)

func TestLooksLikeZone(t *testing.T) {
	valid := []string{
		"UTC",
		"Europe/Helsinki",
		"America/New_York",
		"America/Argentina/Buenos_Aires",
		"Etc/GMT+5",
		"Etc/GMT-14",
	}
	for _, z := range valid {
		if !looksLikeZone(z) {
			t.Errorf("looksLikeZone(%q) = false, want true", z)
		}
	}
	invalid := []string{
		"",                                   // empty
		"/Europe/Helsinki",                   // leading slash
		"Europe/Helsinki/",                   // trailing slash
		"Europe//Helsinki",                   // empty component
		"../../etc/passwd",                   // traversal
		"Europe/Helsinki; reboot",            // shell metachars + space
		"Europe/Hel sinki",                   // space
		"Zone\t",                             // control char
		"America/" + strings.Repeat("x", 80), // over the 64-char cap
	}
	for _, z := range invalid {
		if looksLikeZone(z) {
			t.Errorf("looksLikeZone(%q) = true, want false", z)
		}
	}
}

func TestValidateZone(t *testing.T) {
	// UTC always loads (no tzdata needed), so it is a deterministic positive.
	if err := validateZone("UTC"); err != nil {
		t.Errorf("validateZone(UTC) = %v, want nil", err)
	}
	bad := []string{
		"",            // empty
		"Local",       // pseudo-zone, explicitly rejected
		"Mars/Phobos", // well-formed but not an installed zone
		"bad name",    // fails the syntactic gate
		"../etc/passwd",
	}
	for _, z := range bad {
		if err := validateZone(z); err == nil {
			t.Errorf("validateZone(%q) = nil, want error", z)
		}
	}
}

func TestZoneFromLocaltimeLink(t *testing.T) {
	cases := map[string]string{
		"/usr/share/zoneinfo/Europe/Helsinki":    "Europe/Helsinki",
		"../usr/share/zoneinfo/America/New_York": "America/New_York",
		"/usr/share/zoneinfo/UTC":                "UTC",
		"/etc/localtime":                         "", // no zoneinfo marker
		"/some/random/path":                      "",
		"":                                       "",
	}
	for target, want := range cases {
		if got := zoneFromLocaltimeLink(target); got != want {
			t.Errorf("zoneFromLocaltimeLink(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestParseDetectedZone(t *testing.T) {
	// A clean, valid response (with trailing newline, as ipapi returns).
	if z, err := parseDetectedZone("UTC\n"); err != nil || z != "UTC" {
		t.Errorf("parseDetectedZone(UTC) = %q, %v; want UTC, nil", z, err)
	}
	// Error pages / rate-limit blurbs / junk must be rejected, not returned.
	bad := []string{
		"",
		"   ",
		"<!DOCTYPE html><html>error</html>",
		"RateLimited: too many requests",
		"Undefined",
	}
	for _, body := range bad {
		if z, err := parseDetectedZone(body); err == nil {
			t.Errorf("parseDetectedZone(%q) = %q, nil; want error", body, z)
		}
	}
}
