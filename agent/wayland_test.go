package main

import (
	"testing"
	"time"
)

// Regression for the focused-review finding: recoverPrimary (a Wayland base's
// onTerminal crash-recovery) used to start a fallback Chromium and install it
// with no coordination against a concurrent Switch — racing the runner pointer
// and able to orphan a second Chromium. recoverPrimary now claims the switch
// token. This drives recoverPrimary against a concurrent Switch and asserts no
// deadlock; run under -race to catch a reintroduced data race.
func TestRecoverPrimaryVsSwitchNoRace(t *testing.T) {
	sup := testSupervisor(t)
	// Make the fallback child fail to launch (no real Chromium spawned in CI).
	sup.cfg.Chromium = "/nonexistent/sideshow-chromium"

	// A Wayland base runner already in a terminal state: done closed so Stop()
	// returns at once, no real child.
	r := newModeRunner(sup, Mode{
		Type: ModeWeb, Display: DisplayWayland,
		Params: map[string]any{"url": "https://example.org"},
	})
	r.setState(stateFailed, "test")
	r.closeDone()
	sup.mu.Lock()
	sup.runner = r
	sup.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() { sup.recoverPrimary(r, "https://example.org", true); done <- struct{}{} }()
	go func() { _ = sup.Switch(Mode{Type: ModeOff}); done <- struct{}{} }()

	timeout := time.After(5 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("deadlock: recoverPrimary raced/blocked against Switch")
		}
	}
}
