package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Miracast holds the safety configuration + guard for the experimental Miracast
// sink. Miracast is Wi-Fi Direct (P2P): on a single-radio node it can knock the
// box off its own uplink and leave a headless node unreachable (see
// docs/miracast.md). The hard -allow-miracast deploy-time gate stays; this
// manager bounds the residual risk with three operator-tunable mitigations:
//
//   - iface: exported to the launcher as SIDESHOW_MIRACAST_IFACE, so a wrapper
//     can pin the P2P sink to a DEDICATED second wireless adapter (no contention
//     with the uplink radio);
//   - session time-box: auto-stop after N minutes so it can't hold the radio;
//   - uplink auto-abort: if connectivity is lost for N seconds WHILE miracast is
//     on screen, stop it and restore the last real mode — the node self-heals
//     instead of needing an on-site power-cycle.
//
// Config persists in miracast.json (seeded from the -miracast-* flags); the guard
// goroutine runs whenever miracast is the active mode.
type Miracast struct {
	cfg   *Config
	sup   *Supervisor
	state *State
	path  string

	mu          sync.Mutex
	iface       string
	maxMinutes  int
	abortAfterS int
	saveMu      sync.Mutex
}

// MiracastInfo is the JSON for GET /api/miracast + the snapshot block.
type MiracastInfo struct {
	Allowed     bool   `json:"allowed"`       // -allow-miracast (hard deploy-time gate)
	Iface       string `json:"iface"`         // pin the P2P sink here (2nd adapter)
	MaxMinutes  int    `json:"max_minutes"`   // auto-stop after N min (0 = unlimited)
	AbortAfterS int    `json:"abort_after_s"` // auto-abort on uplink loss (0 = off)
	Active      bool   `json:"active"`        // miracast is on screen now
}

type persistedMiracast struct {
	Iface       string `json:"iface"`
	MaxMinutes  int    `json:"max_minutes"`
	AbortAfterS int    `json:"abort_after_s"`
}

func NewMiracast(cfg *Config, sup *Supervisor, state *State) *Miracast {
	m := &Miracast{
		cfg: cfg, sup: sup, state: state,
		iface: cfg.MiracastIface, maxMinutes: cfg.MiracastMaxMinutes, abortAfterS: cfg.MiracastAbortAfterS,
	}
	if cfg.StateFile != "" {
		m.path = filepath.Join(filepath.Dir(cfg.StateFile), "miracast.json")
	}
	m.load()
	return m
}

func (m *Miracast) load() {
	if m.path == "" {
		return
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var p persistedMiracast
	if json.Unmarshal(b, &p) != nil {
		return
	}
	m.iface = p.Iface
	if p.MaxMinutes >= 0 {
		m.maxMinutes = p.MaxMinutes
	}
	if p.AbortAfterS >= 0 {
		m.abortAfterS = p.AbortAfterS
	}
}

func (m *Miracast) save() {
	if m.path == "" {
		return
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	p := persistedMiracast{Iface: m.iface, MaxMinutes: m.maxMinutes, AbortAfterS: m.abortAfterS}
	m.mu.Unlock()
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		log.Printf("[miracast] state dir: %v", err)
		return
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[miracast] save: %v", err)
		return
	}
	if err := os.Rename(tmp, m.path); err != nil {
		log.Printf("[miracast] save rename: %v", err)
	}
}

// Iface is the interface the supervisor exports to the miracast child (or "").
func (m *Miracast) Iface() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.iface
}

// Set updates + persists the mitigation config. Negative values clamp to 0.
func (m *Miracast) Set(iface string, maxMinutes, abortAfterS int) MiracastInfo {
	if maxMinutes < 0 {
		maxMinutes = 0
	}
	if abortAfterS < 0 {
		abortAfterS = 0
	}
	m.mu.Lock()
	m.iface = strings.TrimSpace(iface)
	m.maxMinutes = maxMinutes
	m.abortAfterS = abortAfterS
	m.mu.Unlock()
	m.save()
	return m.Info()
}

func (m *Miracast) Info() MiracastInfo {
	m.mu.Lock()
	info := MiracastInfo{Allowed: m.cfg.AllowMiracast, Iface: m.iface, MaxMinutes: m.maxMinutes, AbortAfterS: m.abortAfterS}
	m.mu.Unlock()
	info.Active = m.sup.Status().Type == ModeMiracast
	return info
}

// Start runs the guard loop: while miracast is the active running mode it enforces
// the session time-box and the uplink auto-abort, restoring the previous mode if
// either fires.
func (m *Miracast) Start() {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		var activeSince, downSince time.Time
		for range t.C {
			st := m.sup.Status()
			if st.Type != ModeMiracast || st.State != stateRunning {
				activeSince, downSince = time.Time{}, time.Time{}
				continue
			}
			now := time.Now()
			if activeSince.IsZero() {
				activeSince, downSince = now, time.Time{}
			}
			m.mu.Lock()
			maxMin, abortS := m.maxMinutes, m.abortAfterS
			m.mu.Unlock()

			if maxMin > 0 && now.Sub(activeSince) >= time.Duration(maxMin)*time.Minute {
				if m.abort(fmt.Sprintf("session time-box (%d min) reached", maxMin)) {
					activeSince, downSince = time.Time{}, time.Time{}
				}
				continue // if deferred (a switch was in progress), retry next tick
			}
			if abortS > 0 {
				if m.uplinkUp() {
					downSince = time.Time{}
				} else if downSince.IsZero() {
					downSince = now
				} else if now.Sub(downSince) >= time.Duration(abortS)*time.Second {
					if m.abort(fmt.Sprintf("uplink lost for %ds", abortS)) {
						activeSince, downSince = time.Time{}, time.Time{}
					}
					continue
				}
			}
		}
	}()
}

// uplinkUp reports whether the node can still reach the network, reusing the
// watchdog's probe target (a failed dial while miracast is up is the P2P sink
// having taken the radio off the uplink channel).
func (m *Miracast) uplinkUp() bool {
	probe := m.cfg.WatchdogProbe
	if probe == "" {
		probe = "1.1.1.1:53"
	}
	c, err := net.DialTimeout("tcp", probe, 4*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// abort stops miracast and restores the last non-miracast mode. Returns false if
// the restore was deferred (a switch was already in progress → Switch returns a
// 409/error) so the guard retries on the next tick; true if it restored, or if
// miracast is no longer on screen (an operator/watchdog already switched away —
// respect that rather than overriding it). Only persists the mode that actually
// took the screen, so state.json never diverges from reality.
func (m *Miracast) abort(reason string) bool {
	if m.sup.Status().Type != ModeMiracast {
		return true
	}
	restore := m.previousMode()
	log.Printf("[miracast] aborting (%s) → restoring %s", reason, restore.label())
	if err := m.sup.Switch(restore); err != nil {
		log.Printf("[miracast] restore to %s deferred (%v); will retry", restore.label(), err)
		return false
	}
	m.state.RecordMode(restore)
	return true
}

// previousMode returns the most recent non-miracast mode from history, else off.
func (m *Miracast) previousMode() Mode {
	for _, h := range m.state.Info().History {
		if h.Type != ModeMiracast && h.Type != ModeOff {
			return Mode{Type: h.Mode.Type, Display: h.Mode.Display, Params: copyParams(h.Mode.Params)}
		}
	}
	return Mode{Type: ModeOff}
}
