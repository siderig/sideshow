package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Timezone reads and sets the node's system time zone via timedatectl. The
// current zone is cached fork-free (read once at construction, updated on Set)
// so the snapshot poll never forks; the zone list forks timedatectl on demand,
// and detection makes a single opt-in outbound request the operator triggers.
//
// Note: `timedatectl set-timezone` changes the system zone immediately, but the
// agent's own process keeps the zone it started with (Go caches time.Local),
// so the agent's local-time scheduler (weekly.go) only picks up the new zone
// after the agent's next restart. The system clock, kiosk content, and logs
// follow the new zone right away.
type Timezone struct {
	mu      sync.Mutex
	current string   // cached IANA name, e.g. "Europe/Helsinki" ("" if unknown)
	canSet  bool     // timedatectl is present
	zones   []string // cached list-timezones output (lazily filled)
}

// TimezoneInfo is the cheap, fork-free snapshot block (also the core of GET
// /api/timezone).
type TimezoneInfo struct {
	Current string `json:"current"` // IANA name, "" if unknown
	CanSet  bool   `json:"can_set"` // timedatectl available → the zone is settable
}

// timezoneFull is the on-demand GET /api/timezone payload: the snapshot block
// plus the full picker list (kept out of the snapshot — the list forks).
type timezoneFull struct {
	TimezoneInfo
	Zones []string `json:"zones"`
}

// geoTZEndpoint is the public IP-geolocation service Detect() queries. It
// returns the caller's IANA zone as text/plain (e.g. "Europe/Helsinki"). Only
// hit on an explicit operator action; the result is validated and merely
// suggested, never auto-applied.
const geoTZEndpoint = "https://ipapi.co/timezone/"

func NewTimezone() *Timezone {
	t := &Timezone{current: readSystemZone(), canSet: lookPath("timedatectl")}
	return t
}

// Info is the fork-free snapshot block.
func (t *Timezone) Info() TimezoneInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TimezoneInfo{Current: t.current, CanSet: t.canSet}
}

// List returns the sorted set of IANA zones (`timedatectl list-timezones`),
// cached after the first fork. Best-effort: on a node without timedatectl it
// returns an empty list (the UI then falls back to a free-text zone entry).
func (t *Timezone) List() []string {
	t.mu.Lock()
	if t.zones != nil {
		z := t.zones
		t.mu.Unlock()
		return z
	}
	canSet := t.canSet
	t.mu.Unlock()

	if !canSet {
		return []string{}
	}
	out, err := runShort(10*time.Second, "timedatectl", "list-timezones")
	if err != nil {
		log.Printf("[tz] list-timezones: %v", err)
		return []string{}
	}
	zs := []string{}
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			zs = append(zs, s)
		}
	}
	sort.Strings(zs)
	t.mu.Lock()
	t.zones = zs
	t.mu.Unlock()
	return zs
}

// Full is the on-demand GET /api/timezone payload (snapshot block + picker list).
func (t *Timezone) Full() timezoneFull {
	return timezoneFull{TimezoneInfo: t.Info(), Zones: t.List()}
}

// Set validates and applies a new system time zone (`timedatectl set-timezone`),
// then updates the cached value. The name is checked against a strict allowlist
// and the local zoneinfo database before it ever reaches timedatectl.
func (t *Timezone) Set(zone string) (TimezoneInfo, error) {
	zone = strings.TrimSpace(zone)
	if err := validateZone(zone); err != nil {
		return TimezoneInfo{}, err
	}
	t.mu.Lock()
	canSet := t.canSet
	cur := t.current
	t.mu.Unlock()
	if !canSet {
		return TimezoneInfo{}, fmt.Errorf("timedatectl not available on this node")
	}
	if zone == cur {
		return t.Info(), nil
	}
	if out, err := runShort(10*time.Second, "timedatectl", "set-timezone", zone); err != nil {
		return TimezoneInfo{}, fmt.Errorf("timedatectl: %v: %s", err, out)
	}
	t.mu.Lock()
	t.current = zone
	t.mu.Unlock()
	log.Printf("[tz] set to %q", zone)
	return t.Info(), nil
}

// Detect asks a public IP-geolocation service for the zone the node's public IP
// maps to. Opt-in (the operator clicks "Detect"); a single bounded outbound
// request. The returned zone is validated but NOT applied — the caller offers
// it as a suggestion the operator confirms.
func (t *Timezone) Detect(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoTZEndpoint, nil)
	if err != nil {
		return "", fmt.Errorf("detect: %w", err)
	}
	req.Header.Set("User-Agent", "sideshow-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach the location service: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("location service returned HTTP %d", resp.StatusCode)
	}
	zone, err := parseDetectedZone(string(body))
	if err != nil {
		return "", err
	}
	return zone, nil
}

// parseDetectedZone extracts and validates an IANA zone from a geolocation
// response body (text/plain, one zone). It rejects error pages / rate-limit
// blurbs by requiring the trimmed body to be a valid, installed zone.
func parseDetectedZone(body string) (string, error) {
	zone := strings.TrimSpace(body)
	if err := validateZone(zone); err != nil {
		return "", fmt.Errorf("could not determine a time zone from the network")
	}
	return zone, nil
}

// validateZone gates a zone name: a strict syntactic allowlist (defense in depth
// before exec — no shell is used, but a bogus value never reaches timedatectl)
// plus a check that the zone actually exists in the local zoneinfo database.
func validateZone(zone string) error {
	if !looksLikeZone(zone) || zone == "Local" {
		return fmt.Errorf("invalid time-zone name %q — expected an IANA name like Europe/Helsinki", zone)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return fmt.Errorf("unknown time zone %q", zone)
	}
	return nil
}

// looksLikeZone reports whether s is a syntactically plausible IANA zone name:
// slash-separated components of ASCII letters/digits/±/_/-, no empty or
// dot/dot-dot parts, no leading/trailing slash. Accepts single-component names
// (UTC), signed Etc zones (Etc/GMT+5), and nested names (America/Argentina/…).
func looksLikeZone(s string) bool {
	if s == "" || len(s) > 64 || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '_' || r == '-' || r == '+':
			default:
				return false
			}
		}
	}
	return true
}

// readSystemZone reads the node's current IANA zone without forking: first the
// Debian /etc/timezone file, then the /etc/localtime → …/zoneinfo/<zone>
// symlink. Returns "" if neither yields a name.
func readSystemZone() string {
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if z := strings.TrimSpace(string(b)); z != "" && looksLikeZone(z) {
			return z
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if z := zoneFromLocaltimeLink(target); z != "" {
			return z
		}
	}
	return ""
}

// zoneFromLocaltimeLink extracts the IANA zone from a /etc/localtime symlink
// target such as "/usr/share/zoneinfo/Europe/Helsinki" or a relative
// "../usr/share/zoneinfo/Europe/Helsinki". Returns "" if the target doesn't
// point into a zoneinfo tree.
func zoneFromLocaltimeLink(target string) string {
	const marker = "/zoneinfo/"
	if i := strings.LastIndex(target, marker); i >= 0 {
		z := target[i+len(marker):]
		if looksLikeZone(z) {
			return z
		}
	}
	return ""
}
