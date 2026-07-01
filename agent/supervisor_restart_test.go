package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary re-exec itself as a throwaway "display child":
// when SUPERVISOR_TEST_CHILD is set it sleeps for that duration then exits 0, so
// the supervisor manages a real OS process (own pgid, real SIGTERM + Wait reap)
// without depending on a system binary or fractional-sleep support. It also
// mutes the supervisor's per-restart logging, which the interleaving test would
// otherwise spew by the hundred.
func TestMain(m *testing.M) {
	if d := os.Getenv("SUPERVISOR_TEST_CHILD"); d != "" {
		dur, err := time.ParseDuration(d)
		if err != nil {
			dur = 50 * time.Millisecond
		}
		time.Sleep(dur)
		os.Exit(0)
	}
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// childMode returns an app mode that re-execs this test binary as a child that
// lives for `life` then exits 0. params.env is the one channel buildCmd lets an
// app-mode caller inject, so we smuggle the lifetime through it.
func childMode(t *testing.T, life time.Duration) Mode {
	t.Helper()
	m := Mode{
		Type: ModeApp,
		Params: map[string]any{
			"argv": []string{os.Args[0]},
			"env":  map[string]any{"SUPERVISOR_TEST_CHILD": life.String()},
		},
	}
	m.normalize()
	if err := m.validate(); err != nil {
		t.Fatalf("childMode invalid: %v", err)
	}
	return m
}

// fastRestart shrinks the restart timings and lifts the crash-loop cutoff so a
// test can spin the supervise loop through many restarts in milliseconds, and so
// that Stop() (not the failBurst give-up) is what ends the loop. Returns a
// restore func; callers must defer it. Not safe with t.Parallel.
func fastRestart() func() {
	oMin, oMax, oGrace := restartBackoffMin, restartBackoffMax, stopGrace
	oBurst, oHealthy := failBurst, healthyRun
	restartBackoffMin = 1 * time.Millisecond
	restartBackoffMax = 2 * time.Millisecond
	stopGrace = 20 * time.Millisecond
	failBurst = 1 << 30    // never give up: keep restarting so teardown is Stop()'s job
	healthyRun = time.Hour // short-lived children never reset the (irrelevant) burst
	return func() {
		restartBackoffMin, restartBackoffMax, stopGrace = oMin, oMax, oGrace
		failBurst, healthyRun = oBurst, oHealthy
	}
}

// alive reports whether pid names a process we can still signal. Signal 0 only
// probes existence: nil → alive; ESRCH/EPERM → gone (or no longer ours).
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// TestStopKillsRunningChild is the straight-line teardown: a live child must be
// signaled, reaped, and reported stopped — never left orphaned owning the
// screen.
func TestStopKillsRunningChild(t *testing.T) {
	r := newModeRunner(testSupervisor(t), childMode(t, 5*time.Second))
	if err := r.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := r.currentPID()
	if !alive(pid) {
		t.Fatalf("child not running after start (pid=%d)", pid)
	}

	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s")
	}

	if alive(pid) {
		t.Fatalf("child pid %d still alive after Stop()", pid)
	}
	if st := r.status().State; st != stateStopped {
		t.Fatalf("state after Stop() = %q, want %q", st, stateStopped)
	}
}

// TestStopInterleavedWithRestart drives the exact race the supervisor must
// survive: a child exits unexpectedly, the loop relaunches a fresh one, and
// Stop() arrives right in that window. The children here are long-lived (they
// never self-exit during the test), so a relaunched child that Stop() fails to
// signal would hang the loop in cmd.Wait() forever — the deadlock the prompt
// describes, where Stop() blocks on r.done and an orphaned child keeps owning
// the screen. Each iteration nudges the timing so Stop() lands before, during,
// and after the relaunch. Every iteration must see Stop() return within the
// deadline and leave no child alive. Run with -race to also surface the cmd/ctx
// interleaving directly.
//
// (If the children self-exited quickly, an orphan would die on its own and mask
// the deadlock — hence the long lifetime and the explicit crash-to-restart.)
func TestStopInterleavedWithRestart(t *testing.T) {
	defer fastRestart()()

	// Far longer than the 3s deadline: an orphaned child stays alive long enough
	// to actually wedge cmd.Wait(), so the bug would surface as a timeout rather
	// than being papered over by a self-exit.
	const (
		iters     = 60
		childLife = 30 * time.Second
	)

	for i := 0; i < iters; i++ {
		r := newModeRunner(testSupervisor(t), childMode(t, childLife))
		if err := r.start(); err != nil {
			t.Fatalf("iter %d: start: %v", i, err)
		}

		// Record every pid the loop launches so we can prove none survive Stop().
		// The poller exits when the run loop does (r.done closes), so on the happy
		// path it never outlives the runner.
		var (
			mu   sync.Mutex
			pids = map[int]struct{}{}
		)
		go func() {
			for {
				if pid := r.currentPID(); pid > 0 {
					mu.Lock()
					pids[pid] = struct{}{}
					mu.Unlock()
				}
				select {
				case <-r.done:
					return
				case <-time.After(200 * time.Microsecond):
				}
			}
		}()

		// Simulate an unexpected crash so the loop relaunches: kill the running
		// child's group out from under it. With backoff shrunk to ~1ms the loop
		// respawns almost immediately, so a Stop() fired a hair later collides
		// with the relaunch — the window where the kill target can go stale.
		if pid := r.currentPID(); pid > 0 {
			killGroup(pid, syscall.SIGKILL)
		}
		// Walk the gap across the relaunch: 0 lands in the backoff (cancel wins),
		// the rest land at/after the respawn (the stale-target window).
		if d := time.Duration(i%6) * time.Millisecond; d > 0 {
			time.Sleep(d)
		}

		done := make(chan struct{})
		go func() { r.Stop(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("iter %d (gap %dms): Stop() did not return within 3s — deadlock (stale kill target)", i, i%6)
		}

		if st := r.status().State; st != stateStopped {
			t.Fatalf("iter %d: state after Stop() = %q, want %q", i, st, stateStopped)
		}
		mu.Lock()
		for pid := range pids {
			if alive(pid) {
				mu.Unlock()
				t.Fatalf("iter %d (gap %dms): child pid %d still alive after Stop() — orphaned screen owner", i, i%6, pid)
			}
		}
		mu.Unlock()
	}
}

// TestConsoleModeRetiresOnSelfExit is the regression for the "htop won't return
// to the kiosk" bug: a foreground console app that exits on its own (the operator
// quit it) must NOT be respawned — respawning would pin the screen to the mode VT
// forever. Instead the runner retires and fires onTerminal so the screen returns
// to the compositor base.
//
// A real console mode opens the mode VT (absent on the test host), so we hand-
// start the child and drive runLoop directly. That is faithful: the retire path
// returns before any respawn, so buildCmd/openModeTTY is never reached — exactly
// the property under test. If the bug regressed, the loop would instead bump
// Restarts and (failing to reopen the VT) end in stateFailed.
func TestConsoleModeRetiresOnSelfExit(t *testing.T) {
	m := Mode{Type: ModeApp, Display: DisplayConsole, Params: map[string]any{"argv": []any{os.Args[0]}}}
	m.normalize()
	r := newModeRunner(testSupervisor(t), m)

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SUPERVISOR_TEST_CHILD=30ms")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start console child: %v", err)
	}
	pid := cmd.Process.Pid
	r.record(cmd)
	r.setState(stateRunning, "")

	returned := make(chan struct{}) // closed when onTerminal (return-to-base) runs
	r.onTerminal = func() { close(returned) }
	go r.runLoop()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("console self-exit did not trigger onTerminal (return-to-base) within 2s")
	}
	<-r.done // run loop has finished

	if got := r.status().Restarts; got != 0 {
		t.Fatalf("console mode respawned after self-exit: Restarts=%d, want 0", got)
	}
	if st := r.status().State; st != stateStopped {
		t.Fatalf("state after console self-exit = %q, want %q", st, stateStopped)
	}
	if alive(pid) {
		t.Fatalf("console child pid %d still alive after retire", pid)
	}
}

// TestForegroundRetiresWhenDoneClassification locks the receiver-vs-player
// boundary the KMS retire fix keys on: a finite/interactive mode (app, media)
// retires when done, while a surface meant to stay up (a streaming receiver, the
// cog web kiosk) keeps restart-on-exit. A refactor that mislabels one — making a
// receiver stop listening, or a player replay forever — is caught here.
func TestForegroundRetiresWhenDoneClassification(t *testing.T) {
	cases := []struct {
		m    Mode
		want bool
	}{
		{Mode{Type: ModeApp, Display: DisplayConsole}, true},    // htop / a shell
		{Mode{Type: ModeApp, Display: DisplayKMS}, true},        // a one-shot KMS program
		{Mode{Type: ModeMedia, Display: DisplayKMS}, true},      // mpv playing a file to the end
		{Mode{Type: ModeWeb, Display: DisplayKMS}, false},       // cog kiosk — a persistent URL surface
		{Mode{Type: ModeAirplay, Display: DisplayKMS}, false},   // receiver — stays listening
		{Mode{Type: ModeMoonlight, Display: DisplayKMS}, false}, // receiver
		{Mode{Type: ModeSteamlink, Display: DisplayKMS}, false}, // receiver
		{Mode{Type: ModeMiracast, Display: DisplayKMS}, false},  // receiver
		// retiresWhenDone is Type-only, so compositor app/media report true too —
		// they are a base (not foreground), saved from a wrongful retire by the
		// leading foreground() gate in runLoop, NOT by this predicate. Pin that so a
		// refactor can't drop the gate and silently retire a compositor base.
		{Mode{Type: ModeApp, Display: DisplayCompositor}, true},
		{Mode{Type: ModeMedia, Display: DisplayCompositor}, true},
		{Mode{Type: ModeWeb, Display: DisplayCompositor}, false},
	}
	for _, c := range cases {
		if got := c.m.retiresWhenDone(); got != c.want {
			t.Errorf("%s+%s retiresWhenDone()=%v, want %v", c.m.Type, c.m.Display, got, c.want)
		}
	}
}

// TestKMSPlayerRetiresOnCleanExit is the KMS-twin fix (case 1): a one-shot KMS
// player (mpv of a finite file with loop:false) exits 0 when the file ends — it
// must retire and return to the base, NOT respawn and replay forever pinning the
// screen to the mode VT. We drive runLoop with a clean-exiting child and assert
// it retired (Restarts==0, onTerminal fired), mirroring the console retire test.
func TestKMSPlayerRetiresOnCleanExit(t *testing.T) {
	m := Mode{Type: ModeMedia, Display: DisplayKMS, Params: map[string]any{"url": "/media/clip.mp4"}}
	m.normalize()
	r := newModeRunner(testSupervisor(t), m)

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SUPERVISOR_TEST_CHILD=30ms") // exits 0
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start KMS player child: %v", err)
	}
	r.record(cmd)
	r.setState(stateRunning, "")

	returned := make(chan struct{})
	r.onTerminal = func() { close(returned) }
	go r.runLoop()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("KMS player clean exit did not trigger onTerminal (return-to-base) within 2s")
	}
	<-r.done
	if got := r.status().Restarts; got != 0 {
		t.Fatalf("KMS player replayed after a clean exit: Restarts=%d, want 0", got)
	}
	if st := r.status().State; st != stateStopped {
		t.Fatalf("state after KMS player clean exit = %q, want %q", st, stateStopped)
	}
}

// TestKMSPlayerCrashStillRestarts is the other half of the clean-vs-crash split:
// a KMS player that exits NON-zero (a crash, not "finished") must still hit
// restart-on-exit so a transient failure retries — only a clean exit retires. We
// SIGKILL the running child (non-zero wait status); the respawn then execs a
// nonexistent binary and fails, so the loop ends having bumped Restarts>=1.
func TestKMSPlayerCrashStillRestarts(t *testing.T) {
	defer fastRestart()()

	sup := testSupervisor(t)
	sup.cfg.AllowCustomRoot = true // let a display=kms app mode build (root-gated otherwise)
	m := Mode{Type: ModeApp, Display: DisplayKMS, Params: map[string]any{"argv": []any{"/nonexistent/sideshow-kms-none"}}}
	m.normalize()
	r := newModeRunner(sup, m)

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SUPERVISOR_TEST_CHILD=5s") // long-lived; we crash it
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start KMS child: %v", err)
	}
	pid := cmd.Process.Pid
	r.record(cmd)
	r.setState(stateRunning, "")
	go r.runLoop()

	killGroup(pid, syscall.SIGKILL) // crash the child (non-zero wait status)

	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("KMS runLoop did not settle within 2s after a crash")
	}
	if got := r.status().Restarts; got < 1 {
		t.Fatalf("KMS player did not restart after a crash: Restarts=%d, want >=1 (only a clean exit retires)", got)
	}
}

// TestRecoverForegroundReturnsToBaseAndReRecords exercises the REAL onTerminal
// hook that the retire path fires — TestConsoleModeRetiresOnSelfExit stubs it, so
// it never covers the VT hand-back or the state re-record. recoverForeground must
// clear the foreground surface, release the switch token it claims, and fire
// onSettle so the persisted "active" mode is re-synced to the restored base (else
// a reboot relaunches the retired console mode). activateVT's chvt is best-effort
// (no VT on the test host) and its error is ignored, so this runs hostless.
func TestRecoverForegroundReturnsToBaseAndReRecords(t *testing.T) {
	sup := testSupervisor(t)
	var settled int
	sup.SetOnSettle(func() { settled++ })

	base := newModeRunner(sup, webMode())
	base.setState(stateRunning, "")
	fg := newModeRunner(sup, Mode{Type: ModeApp, Display: DisplayConsole, Params: map[string]any{"argv": []any{"htop"}}})
	fg.setState(stateRunning, "")
	sup.mu.Lock()
	sup.runner = base
	sup.fgRunner = fg
	sup.mu.Unlock()

	sup.recoverForeground(fg, Mode{}) // base is alive (console layers over it) → chvt, no relaunch

	sup.mu.Lock()
	gotFg, switching := sup.fgRunner, sup.switching
	sup.mu.Unlock()
	if gotFg != nil {
		t.Fatalf("fgRunner not cleared after recoverForeground")
	}
	if switching {
		t.Fatalf("switch token not released after recoverForeground")
	}
	if settled != 1 {
		t.Fatalf("onSettle fired %d times after return-to-base, want 1", settled)
	}
}

// TestRecoverForegroundYieldsToConcurrentSwitch is the race guard: if an operator
// Switch already owns the screen (switching set), a foreground mode retiring at
// the same instant must NOT clear fgRunner or chvt/settle underneath it — else
// its chvt could strand the screen on the wrong VT. recoverForeground must bail.
func TestRecoverForegroundYieldsToConcurrentSwitch(t *testing.T) {
	sup := testSupervisor(t)
	var settled int
	sup.SetOnSettle(func() { settled++ })

	fg := newModeRunner(sup, Mode{Type: ModeApp, Display: DisplayConsole, Params: map[string]any{"argv": []any{"htop"}}})
	sup.mu.Lock()
	sup.fgRunner = fg
	sup.switching = true // a Switch owns (or is about to own) the screen
	sup.mu.Unlock()

	sup.recoverForeground(fg, Mode{})

	sup.mu.Lock()
	gotFg, switching := sup.fgRunner, sup.switching
	sup.mu.Unlock()
	if gotFg != fg {
		t.Fatalf("recoverForeground cleared fgRunner despite a switch in progress")
	}
	if !switching {
		t.Fatalf("recoverForeground released a switch token it did not own")
	}
	if settled != 0 {
		t.Fatalf("onSettle fired during a concurrent switch: got %d, want 0", settled)
	}
}

// TestRecoverForegroundRestoresTornDownBase is the KMS-twin fix (case 2): a KMS
// foreground mode is a full-screen takeover that tore the compositor base down
// (runner==nil), so recoverForeground must RELAUNCH the base — not chvt to a dead
// VT with no compositor (a blank screen). The base here is a launchable app-mode
// surface (this test binary re-exec'd) so startPrimary actually installs a runner
// on the test host, where a web base would need a real Chromium.
func TestRecoverForegroundRestoresTornDownBase(t *testing.T) {
	sup := testSupervisor(t)
	var settled int
	sup.SetOnSettle(func() { settled++ })

	fg := newModeRunner(sup, Mode{Type: ModeMedia, Display: DisplayKMS, Params: map[string]any{"url": "/media/clip.mp4"}})
	fg.setState(stateRunning, "")
	sup.mu.Lock()
	sup.fgRunner = fg
	sup.runner = nil // the KMS takeover already freed the base
	sup.mu.Unlock()

	baseMode := Mode{Type: ModeApp, Params: map[string]any{
		"argv": []any{os.Args[0]},
		"env":  map[string]any{"SUPERVISOR_TEST_CHILD": "5s"},
	}}
	baseMode.normalize()

	sup.recoverForeground(fg, baseMode)

	sup.mu.Lock()
	base, gotFg, switching := sup.runner, sup.fgRunner, sup.switching
	sup.mu.Unlock()
	if base == nil {
		t.Fatal("recoverForeground did not relaunch the torn-down base — the screen would be blank")
	}
	t.Cleanup(base.Stop)
	if gotFg != nil {
		t.Fatalf("fgRunner not cleared after base restore")
	}
	if switching {
		t.Fatalf("switch token not released after base restore")
	}
	if settled != 1 {
		t.Fatalf("onSettle fired %d times after base restore, want 1", settled)
	}
}

// TestRecoverForegroundBaselessNodeStaysIdle guards the base-less-node fix: on a
// node configured console/KMS/off-only (no compositor base — start-mode off/none,
// runner==nil), a retiring foreground mode must return the screen to idle (off),
// NOT conjure a Chromium web kiosk it was never meant to show (and then persist
// it). We point cfg.Chromium at a harmless real binary so that IF the buggy kiosk
// fallback fired it WOULD install a web runner — making the regression observable
// on a host with no real Chromium; the fix installs no runner and Status reports off.
func TestRecoverForegroundBaselessNodeStaysIdle(t *testing.T) {
	defer fastRestart()()

	sup := testSupervisor(t)
	sup.cfg.Chromium = "/bin/echo" // a spurious kiosk launch would install a web runner off this
	var settled int
	sup.SetOnSettle(func() { settled++ })

	fg := newModeRunner(sup, Mode{Type: ModeApp, Display: DisplayConsole, Params: map[string]any{"argv": []any{"htop"}}})
	fg.setState(stateRunning, "")
	sup.mu.Lock()
	sup.fgRunner = fg
	sup.runner = nil // base-less node: nothing was ever hidden
	sup.mu.Unlock()

	sup.recoverForeground(fg, Mode{}) // no base mode captured (there was none)

	sup.mu.Lock()
	base, gotFg, switching := sup.runner, sup.fgRunner, sup.switching
	sup.mu.Unlock()
	if base != nil {
		base.Stop() // a runaway kiosk was spawned — reap it before failing
		t.Fatalf("base-less retire spawned a %s base; want idle/off (no kiosk the node was never configured to show)", base.modeType())
	}
	if gotFg != nil {
		t.Fatalf("fgRunner not cleared")
	}
	if switching {
		t.Fatalf("switch token not released")
	}
	if got := sup.Status().Type; got != ModeOff {
		t.Fatalf("Status after base-less retire = %q, want %q", got, ModeOff)
	}
	if settled != 1 {
		t.Fatalf("onSettle fired %d times, want 1", settled)
	}
}
