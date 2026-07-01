package main

import (
	"path/filepath"
	"testing"
)

func newTestMiracast(t *testing.T) (*Miracast, *State, *Config) {
	t.Helper()
	cfg := &Config{StateFile: filepath.Join(t.TempDir(), "display.json"), MiracastMaxMinutes: 30, MiracastAbortAfterS: 30}
	st := NewState(cfg)
	return NewMiracast(cfg, NewSupervisor(cfg), st), st, cfg
}

func TestMiracastSetAndPersist(t *testing.T) {
	m, _, cfg := newTestMiracast(t)
	info := m.Set("wlan1", 15, 45)
	if info.Iface != "wlan1" || info.MaxMinutes != 15 || info.AbortAfterS != 45 {
		t.Fatalf("set mismatch: %+v", info)
	}
	if info.Allowed {
		t.Fatal("allowed should reflect cfg.AllowMiracast=false")
	}
	// reload from disk
	m2 := NewMiracast(cfg, NewSupervisor(cfg), NewState(cfg))
	got := m2.Info()
	if got.Iface != "wlan1" || got.MaxMinutes != 15 || got.AbortAfterS != 45 {
		t.Fatalf("did not persist: %+v", got)
	}
	if m2.Iface() != "wlan1" {
		t.Fatalf("Iface() = %q", m2.Iface())
	}
}

func TestMiracastNegativeClamp(t *testing.T) {
	m, _, _ := newTestMiracast(t)
	if info := m.Set("", -5, -1); info.MaxMinutes != 0 || info.AbortAfterS != 0 {
		t.Fatalf("negatives should clamp to 0: %+v", info)
	}
}

func TestMiracastPreviousModeRestoresLastRealMode(t *testing.T) {
	m, st, _ := newTestMiracast(t)
	if pm := m.previousMode(); pm.Type != ModeOff {
		t.Fatalf("empty history should restore off, got %s", pm.Type)
	}
	st.RecordMode(Mode{Type: ModeWeb, Params: map[string]any{"url": "https://a"}})
	st.RecordMode(Mode{Type: ModeMiracast})
	pm := m.previousMode()
	if pm.Type != ModeWeb || pm.str("url") != "https://a" {
		t.Fatalf("previous should be the web mode before miracast, got %+v", pm)
	}
}

func TestMiracastAllowedReflectsFlag(t *testing.T) {
	cfg := &Config{StateFile: filepath.Join(t.TempDir(), "display.json"), AllowMiracast: true}
	m := NewMiracast(cfg, NewSupervisor(cfg), NewState(cfg))
	if !m.Info().Allowed {
		t.Fatal("allowed should be true when -allow-miracast is set")
	}
}
