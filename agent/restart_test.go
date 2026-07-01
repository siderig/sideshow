package main

import (
	"testing"
	"time"
)

// Regression for the batch review: RestartMode and NavigateIfWeb must claim the
// switch token (like Switch) so concurrent calls serialize instead of racing
// s.runner / s.switching or clobbering each other. Drive all three against each
// other and assert no deadlock; run under -race to catch a reintroduced race.
func TestRestartNavigateSwitchNoRace(t *testing.T) {
	sup := testSupervisor(t)
	sup.cfg.Chromium = "/nonexistent/sideshow-chromium" // launches fail; no real browser

	r := newModeRunner(sup, Mode{
		Type: ModeWeb, Display: DisplayCompositor,
		Params: map[string]any{"url": "https://example.org"},
	})
	_ = r.start() // fails (no chromium) → state failed, done closed
	sup.mu.Lock()
	sup.runner = r
	sup.mu.Unlock()

	done := make(chan struct{}, 4)
	go func() { _ = sup.RestartMode(); done <- struct{}{} }()
	go func() { _ = sup.Switch(Mode{Type: ModeOff}); done <- struct{}{} }()
	go func() { sup.NavigateIfWeb("https://example.org/2"); done <- struct{}{} }()
	go func() { _ = sup.RestartMode(); done <- struct{}{} }()

	timeout := time.After(5 * time.Second)
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("deadlock: RestartMode/NavigateIfWeb/Switch contended")
		}
	}
}
