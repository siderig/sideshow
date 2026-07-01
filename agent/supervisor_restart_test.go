package main

import (
	"io"
	"log"
	"os"
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
