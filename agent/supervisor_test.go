package main

import (
	"testing"
	"time"
)

// testSupervisor builds a Supervisor wired to throwaway dirs so child processes
// can be (attempted to be) launched without touching a real seat/display.
func testSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	return NewSupervisor(&Config{
		Node:       "test",
		SeatUser:   "tester",
		Display:    ":0",
		RuntimeDir: t.TempDir(),
		Home:       t.TempDir(),
		Chromium:   "/usr/bin/chromium",
		CDPHost:    "127.0.0.1",
		CDPPort:    9222,
		NoPrivDrop: true,
	})
}

// badAppMode is an app mode whose argv[0] is a path-form binary that does not
// exist: buildCmd succeeds (no LookPath for path-form names) but cmd.Start()
// fails with ENOENT, so start() returns before the supervise loop is launched.
func badAppMode() Mode {
	m := Mode{Type: ModeApp, Params: map[string]any{
		"argv": []any{"/nonexistent/sideshow-no-such-binary"},
	}}
	m.normalize()
	return m
}

// webMode is an ordinary web kiosk on the compositor, used to build a runner
// whose state can be forced (without launching Chromium) for guard tests.
func webMode() Mode {
	m := Mode{Type: ModeWeb, Display: DisplayCompositor, Params: map[string]any{"url": "https://example.test/"}}
	m.normalize()
	return m
}

// When a runner's first launch fails, its run loop never starts — so nothing
// will ever close r.done. Stop() must not block forever waiting on it.
func TestStopAfterFailedLaunchDoesNotHang(t *testing.T) {
	r := newModeRunner(testSupervisor(t), badAppMode())

	if err := r.start(); err == nil {
		t.Fatal("start() should fail when argv[0] does not exist")
	}

	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung after a failed launch (deadlock waiting on r.done)")
	}

	select {
	case <-r.done:
	default:
		t.Error("r.done should be closed once the runner reaches a terminal state")
	}
}

// Regression for the latent deadlock: a failed runner that got installed as the
// active one used to wedge the next switch forever, because Switch() calls
// old.Stop() which blocked on an r.done that the never-started run loop would
// never close. The whole mode-switch path then hung with s.switching held.
func TestSwitchOutOfFailedRunnerDoesNotHang(t *testing.T) {
	sup := testSupervisor(t)

	// Reproduce the wedged state directly: a runner whose launch failed,
	// installed as the active runner (its supervise loop never ran).
	r := newModeRunner(sup, badAppMode())
	if err := r.start(); err == nil {
		t.Fatal("start() should fail when argv[0] does not exist")
	}
	sup.mu.Lock()
	sup.runner = r
	sup.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- sup.Switch(Mode{Type: ModeOff}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("switch out of a failed runner returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Switch hung in old.Stop() on a failed runner (deadlock on r.done)")
	}
}

// The public API path: switching into a mode that can't launch must fail the
// switch and leave the supervisor able to switch again (no stuck s.switching).
func TestSwitchIntoMissingBinaryThenSwitchAgain(t *testing.T) {
	sup := testSupervisor(t)

	if err := sup.Switch(badAppMode()); err == nil {
		t.Fatal("Switch into a non-existent binary should return an error")
	}

	done := make(chan error, 1)
	go func() { done <- sup.Switch(Mode{Type: ModeOff}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("switch to off after a failed switch: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Switch hung after a prior failed switch (s.switching wedged)")
	}
}

// ReattachWeb is the watchdog's cheap CDP recovery; it must be a strict no-op
// unless a LIVE web base is on screen with CDP detached — never relaunch, never
// hold the switch token after returning, never disturb a wrong-state surface —
// and it must 409 (not interleave) while a real switch owns the token.
func TestReattachWebNoOpGuards(t *testing.T) {
	sup := testSupervisor(t)

	tokenFree := func(label string) {
		t.Helper()
		sup.mu.Lock()
		held := sup.switching
		sup.mu.Unlock()
		if held {
			t.Fatalf("ReattachWeb leaked the switch token (%s)", label)
		}
	}

	// Nothing on screen → no-op, token released.
	if err := sup.ReattachWeb(); err != nil {
		t.Fatalf("ReattachWeb with no runner: %v", err)
	}
	tokenFree("no runner")

	// A non-web base (a failed app runner) → no-op, token released; must not try
	// to drive Chromium for a surface that isn't a web kiosk.
	r := newModeRunner(sup, badAppMode())
	_ = r.start() // fails (missing binary): modeType is app, not web, and not alive
	sup.mu.Lock()
	sup.runner = r
	sup.mu.Unlock()
	if err := sup.ReattachWeb(); err != nil {
		t.Fatalf("ReattachWeb on a non-web base: %v", err)
	}
	tokenFree("non-web base")

	// A web base that is NOT alive (Chromium failed/exited) → no-op, token
	// released. This case exists to exercise the !base.alive() guard SPECIFICALLY:
	// the non-web case above short-circuits on modeType() before alive() is ever
	// evaluated, so without this a regression dropping the alive() check would
	// still pass. A live (state==starting/running) web base with CDP detached
	// would instead try to drive Chromium, so the guard must reject a dead one.
	wr := newModeRunner(sup, webMode())
	wr.setState(stateFailed, "test: web runner not alive")
	sup.mu.Lock()
	sup.runner = wr
	sup.mu.Unlock()
	if err := sup.ReattachWeb(); err != nil {
		t.Fatalf("ReattachWeb on a dead web base: %v", err)
	}
	tokenFree("dead web base")

	// A real mode switch in progress → 409, and that other op's token is untouched.
	sup.mu.Lock()
	sup.switching = true
	sup.mu.Unlock()
	err := sup.ReattachWeb()
	if ae, ok := err.(*apiError); !ok || ae.code != 409 {
		t.Fatalf("ReattachWeb during a switch: want 409 apiError, got %v", err)
	}
	sup.mu.Lock()
	stillHeld := sup.switching
	sup.switching = false
	sup.mu.Unlock()
	if !stillHeld {
		t.Error("ReattachWeb cleared another operation's switch token")
	}
}
