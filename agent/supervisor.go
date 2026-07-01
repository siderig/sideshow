package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Child/mode states reported in the API.
const (
	stateStarting = "starting"
	stateRunning  = "running"
	stateFailed   = "failed"
	stateStopped  = "stopped"
)

// Restart policy for the supervised child (the "no-respawn gap" fix). These are
// vars, not consts, only so the interleaving tests can shrink the timings and
// lift the crash-loop cutoff; production never reassigns them.
var (
	restartBackoffMin = 1 * time.Second
	restartBackoffMax = 15 * time.Second
	// A child that stayed up at least healthyRun is considered to have run
	// cleanly: its crash resets the backoff and the failure burst, so one late
	// crash after hours of uptime doesn't inherit a long backoff or a stale
	// burst count.
	healthyRun = 30 * time.Second
	// If the child dies more than failBurst times within failWindow, give up
	// (mode → failed) instead of hot-looping. In the bulletproof tier this is
	// where the hardware watchdog takes over.
	failBurst  = 5
	failWindow = 30 * time.Second
	stopGrace  = 5 * time.Second // SIGTERM → SIGKILL grace on teardown
)

// Supervisor arbitrates the screen across two surfaces (docs/node-api.md §3):
//   - runner:   the persistent COMPOSITOR surface (web / app-on-X), drawn into
//     the compositor's VT. Kept alive across foreground excursions.
//   - fgRunner: an on-demand FOREGROUND surface (console/kms) on a dedicated VT,
//     layered over the compositor via VT switching (the DRM-master
//     handoff). nil when the compositor surface is the one on screen.
//
// Exactly one VT is active, so exactly one surface is visible. Switches are
// serialized (the switching flag → 409).
type Supervisor struct {
	cfg      *Config
	chrome   *Chrome   // CDP controller for the X/Wayland Chromium kiosk
	miracast *Miracast // wired post-construction; supplies the P2P interface to pin

	mu        sync.Mutex  // guards runner/fgRunner/switching
	runner    *modeRunner // compositor surface
	fgRunner  *modeRunner // foreground VT surface (console/kms), or nil
	switching bool
	startedAt time.Time
}

func NewSupervisor(cfg *Config) *Supervisor {
	return &Supervisor{cfg: cfg, chrome: NewChrome(cfg), startedAt: time.Now()}
}

// SetMiracast wires the miracast manager (post-construction) so childEnv can pin
// the P2P sink to the operator-chosen interface.
func (s *Supervisor) SetMiracast(m *Miracast) { s.miracast = m }

// cogBusAddr is the fixed address of the private D-Bus session bus the agent runs
// for the cog kiosk, so cog (the launch) and cogctl (control) share one bus
// deterministically — not relying on a root login session's /run/user/0/bus,
// which is absent on a headless boot. In /run (tmpfs), recreated each boot.
const cogBusAddr = "unix:path=/run/sideshow/cog-bus"

// ensureCogDBus makes sure the private session bus for cog is up (idempotent),
// starting a forking dbus-daemon at cogBusAddr if nothing is listening there.
// Called before launching the cog kiosk so cog can register its control actions.
func (s *Supervisor) ensureCogDBus() error {
	const sock = "/run/sideshow/cog-bus"
	if c, err := net.DialTimeout("unix", sock, 500*time.Millisecond); err == nil {
		c.Close()
		return nil // already up
	}
	if err := os.MkdirAll("/run/sideshow", 0o755); err != nil {
		return fmt.Errorf("cog bus dir: %w", err)
	}
	_ = os.Remove(sock) // stale socket from a prior boot/crash
	cmd := exec.Command("dbus-daemon", "--session", "--address="+cogBusAddr, "--fork", "--nopidfile")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start cog dbus: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cogControl runs a cogctl action (e.g. "open <url>", "reload") against the live
// cog kiosk over its D-Bus GAction interface. cog and cogctl share cogBusAddr
// (set in childEnv); cogctl finds cog by its default app-id (com.igalia.Cog).
// Runs as root — the bus socket is root-owned.
func (s *Supervisor) cogControl(args ...string) error {
	cmd := exec.Command(s.cfg.CogCtlCmd, args...)
	cmd.Env = []string{
		"DBUS_SESSION_BUS_ADDRESS=" + cogBusAddr,
		"HOME=/root",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cogctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Switch brings the requested mode on screen, blocking until its child is up (or
// it fails). Concurrent switches get 409. The switching flag is cleared via
// defer (crash-safe). Foreground (console/kms) modes layer over the compositor
// surface via VT switching; compositor modes return the screen to the
// compositor's VT (re-attaching CDP if a VT excursion dropped it).
func (s *Supervisor) Switch(m Mode) (retErr error) {
	m.normalize()
	if err := m.validate(); err != nil {
		return &apiError{code: 400, err: err}
	}

	s.mu.Lock()
	if s.switching {
		s.mu.Unlock()
		return &apiError{code: 409, err: fmt.Errorf("a mode switch is already in progress")}
	}
	s.switching = true
	base, fg := s.runner, s.fgRunner
	s.mu.Unlock()

	defer func() {
		if rec := recover(); rec != nil {
			retErr = &apiError{code: 500, err: fmt.Errorf("mode switch panicked: %v", rec)}
			log.Printf("PANIC in Switch(%s): %v", m.label(), rec)
		}
		s.mu.Lock()
		s.switching = false
		s.mu.Unlock()
	}()

	if m.foreground() {
		// Console/kms foreground modes layer over the compositor base on the mode
		// VT. That works over either X or Wayland — as long as the mode VT is
		// distinct from the base's VT. The only un-runnable case is a Wayland base
		// whose VT equals the mode VT (the historical default); reject that with a
		// fix-it hint rather than colliding on one VT.
		if base != nil && base.isWaylandMode() && s.cfg.ModeVT == s.cfg.WaylandVT {
			return &apiError{code: 409, err: fmt.Errorf("the console/kms mode VT (%d) collides with the Wayland primary's VT; start the agent with -mode-vt set to a free VT (≠ -wayland-vt) to layer a console mode over Wayland", s.cfg.ModeVT)}
		}
		return s.switchForeground(m, fg)
	}
	return s.switchCompositor(m, base, fg)
}

// switchForeground brings a console/kms mode onto its dedicated VT, keeping the
// compositor surface alive (suspended) behind it for an instant switch back.
func (s *Supervisor) switchForeground(m Mode, fg *modeRunner) error {
	if fg != nil && fg.runningMode(m) {
		// Already running this exact mode and drawing — just ensure it's visible.
		if err := s.activateVT(s.cfg.ModeVT); err != nil {
			return &apiError{code: 500, err: err}
		}
		return nil
	}
	// In-place URL change for the cog kiosk: re-navigate over cog's D-Bus control
	// (cogctl open) instead of restarting cog (mirrors the X kiosk's CDP re-nav).
	// Only the URL differs here — runningMode above already handled an exact match.
	if fg != nil && fg.alive() && m.isCogKiosk() && fg.modeType() == ModeWeb && fg.modeDisplay() == DisplayKMS {
		if err := s.activateVT(s.cfg.ModeVT); err != nil {
			return &apiError{code: 500, err: err}
		}
		if err := s.cogControl("open", m.str("url")); err != nil {
			return &apiError{code: 502, err: fmt.Errorf("cog navigate: %w", err)}
		}
		s.mu.Lock()
		if s.fgRunner == fg {
			fg.setMode(m)
		}
		s.mu.Unlock()
		return nil
	}
	if err := s.preflight(m); err != nil {
		return &apiError{code: 400, err: err}
	}
	// Stop any current foreground first — it shares the mode VT with the new one.
	if fg != nil {
		s.mu.Lock()
		s.fgRunner = nil
		s.mu.Unlock()
		fg.Stop()
	}
	r := newModeRunner(s, m)
	r.onTerminal = func() { s.recoverForeground(r) } // crash → return to compositor VT

	// cog (web+kms) must own DRM master, which only frees once the mode VT is the
	// active console — so switch the VT BEFORE launching cog. cog also needs its
	// D-Bus session bus up so it can register control actions. Other foreground
	// modes start first, then become visible (their child tolerates the late chvt).
	earlyVT := m.isCogKiosk()
	if earlyVT {
		if err := s.ensureCogDBus(); err != nil {
			log.Printf("[cog] dbus bus unavailable (navigate/reload won't work): %v", err)
		}
		if err := s.activateVT(s.cfg.ModeVT); err != nil {
			_ = s.activateVT(s.baseVT())
			return &apiError{code: 500, err: fmt.Errorf("activate mode VT: %w", err)}
		}
	}
	if err := r.start(); err != nil {
		_ = s.activateVT(s.baseVT()) // recover to the compositor surface (X or Wayland), not a blank VT
		return &apiError{code: 500, err: err}
	}
	s.mu.Lock()
	s.fgRunner = r
	s.mu.Unlock()

	if earlyVT {
		s.stopSuspendedBaseForKMS(m) // cog is a full-screen KMS takeover — free the X base
		return nil                   // VT was switched before the launch; cog renders its launch URL
	}
	// Make the mode VT visible. If chvt fails, the screen is still on the
	// compositor VT — so tear the foreground back down rather than reporting a
	// success that doesn't match what's on screen.
	if err := s.activateVT(s.cfg.ModeVT); err != nil {
		s.mu.Lock()
		if s.fgRunner == r {
			s.fgRunner = nil
		}
		s.mu.Unlock()
		r.Stop()
		return &apiError{code: 500, err: fmt.Errorf("switch to %s: %w", m.label(), err)}
	}
	s.stopSuspendedBaseForKMS(m) // a KMS receiver/player owns the whole screen — free the X base
	return nil
}

// stopSuspendedBaseForKMS tears down the compositor base after a direct-KMS
// foreground mode has taken the screen. A console overlay (htop) deliberately
// keeps the kiosk alive behind it for an instant switch-back — but a KMS
// receiver/player (airplay, media, cog) is a FULL-SCREEN takeover, so leaving the
// X Chromium base running just wastes RAM (a Pi 3B can't afford ~10 idle Chromium
// processes under uxplay) and makes "what's on screen" ambiguous. This mirrors
// what a compositor-backend receiver already does (it replaces the base), so the
// behaviour is now consistent: any full-screen mode stops the previous one; only
// a console overlay layers. Stopping an X client never touches DRM master (Xorg
// holds it, not Chromium), so it is safe alongside the VT/DRM handoff.
func (s *Supervisor) stopSuspendedBaseForKMS(m Mode) {
	if m.Display != DisplayKMS {
		return
	}
	s.mu.Lock()
	base := s.runner
	s.runner = nil
	s.mu.Unlock()
	if base != nil {
		s.chrome.Detach()
		base.Stop()
		log.Printf("[mode] stopped the compositor base behind KMS %s (full-screen takeover frees its RAM)", m.label())
	}
}

// recoverForeground returns the screen to the compositor VT after a foreground
// runner failed on its own, but only if it is still the installed foreground
// surface (a concurrent Switch may already own the screen).
func (s *Supervisor) recoverForeground(r *modeRunner) {
	s.mu.Lock()
	current := s.fgRunner == r
	if current {
		s.fgRunner = nil
	}
	s.mu.Unlock()
	if current {
		vt := s.baseVT()
		log.Printf("foreground mode failed; returning screen to compositor VT %d", vt)
		_ = s.activateVT(vt)
	}
}

// switchCompositor brings up a primary surface — the X11 web/app base, the
// labwc/Wayland web base, or off. There are two compositor backends now (X on
// XVT, Wayland on WaylandVT), each with its own VT and CDP port; the screen
// shows exactly one at a time. Any foreground VT mode yields first.
func (s *Supervisor) switchCompositor(m Mode, base, fg *modeRunner) error {
	cameFromForeground := fg != nil
	if fg != nil {
		s.mu.Lock()
		s.fgRunner = nil
		s.mu.Unlock()
		fg.Stop()
	}

	if m.Type == ModeOff {
		if base != nil {
			s.mu.Lock()
			s.runner = nil
			s.mu.Unlock()
			base.Stop()
		}
		s.chrome.Detach()
		_ = s.activateVT(s.cfg.XVT) // return the screen to X
		return nil
	}

	targetVT := s.primaryVT(m)

	// Same compositor backend already the base? (both X-web, or both Wayland-web,
	// or the same X app). Only then can we re-use the running surface.
	sameBackend := base != nil && base.alive() &&
		base.modeType() == m.Type && base.modeDisplay() == m.Display

	// In-place web→web on the SAME backend: Chromium is still alive, and after the
	// chvt the screen already shows it — correctness does NOT depend on CDP. A VT
	// excursion drops the CDP socket (leaving "attached" stale-true), so on return
	// we best-effort re-attach (short-bounded) for screenshots/URL control; if
	// that fails we LEAVE Chromium running. A plain web→web URL change with CDP
	// attached just re-navigates. Chromium death is handled by restart-on-exit.
	if sameBackend && m.Type == ModeWeb {
		if cameFromForeground {
			_ = s.activateVT(targetVT)
		}
		url, dark := m.str("url"), m.boolOr("dark", true)
		s.chrome.Target(s.cdpPort(m))
		if cameFromForeground || !s.chrome.Attached() {
			s.chrome.Detach()
			if err := s.chrome.Attach(url, dark, attachWaitReattach); err != nil {
				log.Printf("[web] CDP re-attach deferred (screen is correct, Chromium alive): %v", err)
			}
		} else if err := s.chrome.Navigate(url, dark); err != nil {
			return &apiError{code: 500, err: fmt.Errorf("re-navigate failed: %w", err)}
		}
		s.mu.Lock()
		if s.runner == base {
			base.setMode(m)
		}
		s.mu.Unlock()
		return nil
	}
	if sameBackend && base.sameMode(m) {
		if cameFromForeground {
			_ = s.activateVT(targetVT)
		}
		return nil // already this exact compositor mode
	}

	// Replace the base surface (different backend or type) with a fresh child.
	return s.startPrimary(m, base)
}

// startPrimary tears down prevBase (if any) and brings up m as a fresh primary
// surface, doing the VT/DRM handoff and CDP re-target. On a start failure it
// recovers the screen to X and, if it had destroyed a working base, restores the
// web kiosk so the screen never strands blank. The caller MUST hold the switch
// token (s.switching) — both switchCompositor and RestartMode do.
func (s *Supervisor) startPrimary(m Mode, prevBase *modeRunner) error {
	// Pre-flight before teardown so a bad request can't blank the screen (§3).
	if err := s.preflight(m); err != nil {
		return &apiError{code: 400, err: err}
	}
	hadBase := prevBase != nil
	if prevBase != nil {
		s.mu.Lock()
		if s.runner == prevBase {
			s.runner = nil
		}
		s.mu.Unlock()
		prevBase.Stop()
	}
	s.chrome.Detach()

	// DRM-master handoff via the VT switch BEFORE starting the new primary:
	//   - Wayland target: chvt to the Wayland VT so X drops DRM master, then labwc
	//     acquires it there.
	//   - X target (e.g. returning from Wayland): chvt to the X VT so Xorg
	//     reacquires master, then its Chromium client (re)starts.
	_ = s.activateVT(s.primaryVT(m))
	s.chrome.Target(s.cdpPort(m)) // postStart attaches CDP on this backend's port

	r := newModeRunner(s, m)
	// A Wayland primary that later crash-loops to give-up would strand the screen
	// on a dead Wayland VT (unlike X, where Xorg/lightdm survives underneath);
	// give it a terminal-recovery hook back to the X kiosk.
	if m.isWaylandPrimary() {
		url, dark := m.str("url"), m.boolOr("dark", true)
		r.onTerminal = func() { s.recoverPrimary(r, url, dark) }
	}
	if err := r.start(); err != nil {
		_ = s.activateVT(s.cfg.XVT)
		s.chrome.Target(s.cfg.CDPPort)
		if hadBase {
			s.restoreWebKiosk(m.str("url"), m.boolOr("dark", true))
		}
		return &apiError{code: 500, err: err}
	}
	s.mu.Lock()
	s.runner = r
	s.mu.Unlock()
	return nil
}

// recoverPrimary returns the screen to the X kiosk after a Wayland primary
// crash-looped to give-up on its own (NOT an intentional stop). Unlike
// recoverForeground it start()s a fallback child, so it must NOT race an
// operator Switch: it claims the switch token (under s.mu) so a concurrent
// Switch gets 409 instead of both installing a runner — otherwise the slow
// fb.start() could be clobbered/orphaned and leave two Chromiums on screen.
func (s *Supervisor) recoverPrimary(r *modeRunner, url string, dark bool) {
	s.mu.Lock()
	if s.switching || s.runner != r {
		s.mu.Unlock() // a switch owns (or will own) the screen — let it
		return
	}
	s.switching = true
	s.runner = nil
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.switching = false
		s.mu.Unlock()
	}()

	log.Printf("[recover] Wayland primary failed; returning the screen to X and restoring the web kiosk")
	s.chrome.Detach()
	s.chrome.Target(s.cfg.CDPPort)
	_ = s.activateVT(s.cfg.XVT)
	s.restoreWebKiosk(url, dark)
}

// restoreWebKiosk best-effort brings the default X11 web kiosk back after a
// failed primary switch tore down a working base — so the screen recovers
// rather than going blank. Falls back to the configured initial URL, preserving
// the requested dark/light appearance. The caller MUST hold the switch token
// (s.switching) so the unconditional s.runner install can't race another switch.
func (s *Supervisor) restoreWebKiosk(url string, dark bool) {
	if url == "" {
		url = s.cfg.InitialURL
	}
	fb := newModeRunner(s, Mode{Type: ModeWeb, Display: DisplayCompositor, Params: map[string]any{"url": url, "dark": dark}})
	if err := fb.start(); err != nil {
		log.Printf("[recover] could not restore X web kiosk: %v", err)
		return
	}
	s.mu.Lock()
	s.runner = fb
	s.mu.Unlock()
	log.Printf("[recover] restored X web kiosk after a failed primary switch")
}

// OnWaylandPrimary reports whether the labwc Wayland kiosk is the surface on
// screen (a Wayland base with no foreground VT mode over it). VNC uses this to
// avoid capturing/injecting into the now-hidden X session.
func (s *Supervisor) OnWaylandPrimary() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fgRunner == nil && s.runner != nil && s.runner.isWaylandMode()
}

// Foreground reports whether a console/kms foreground VT mode is on screen (the
// X compositor output is suspended behind it). Display.Rotate checks this so it
// doesn't rotate the hidden X output, matching SetTheme/SetZoom.
func (s *Supervisor) Foreground() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fgRunner != nil
}

// primaryVT is the VT a primary surface owns: the Wayland VT for the labwc
// kiosk, else the X VT.
func (s *Supervisor) primaryVT(m Mode) int {
	if m.isWaylandPrimary() {
		return s.cfg.WaylandVT
	}
	return s.cfg.XVT
}

// baseVT is the VT of the currently-installed compositor base — the Wayland VT
// when the labwc primary is up, else the X VT. A foreground (console) mode
// layers over it on the mode VT and returns here on teardown/crash, so recovery
// lands on whichever compositor actually owns the screen (not always X).
func (s *Supervisor) baseVT() int {
	s.mu.Lock()
	base := s.runner
	s.mu.Unlock()
	if base != nil && base.isWaylandMode() {
		return s.cfg.WaylandVT
	}
	return s.cfg.XVT
}

// cdpPort is the CDP port for a web primary: the Wayland Chromium's port for the
// labwc kiosk, else the X Chromium's port.
func (s *Supervisor) cdpPort(m Mode) int {
	if m.isWayland() {
		return s.cfg.WaylandCDPPort
	}
	return s.cfg.CDPPort
}

// Busy reports whether a mode switch is in progress or a foreground VT mode is
// on screen — i.e. the node is NOT in a steady single-kiosk state. The upgrade
// path checks this so a multi-minute apt run can't stack on a switch/cold-start.
func (s *Supervisor) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switching || s.fgRunner != nil
}

// CDPAttached reports whether the CDP controller currently holds a connection.
func (s *Supervisor) CDPAttached() bool { return s.chrome.Attached() }

// ShowMessage overlays a banner on the kiosk (web mode); secs>0 auto-clears.
// st controls appearance (size/position/colors); zero values use the defaults.
func (s *Supervisor) ShowMessage(text string, secs int, st MsgStyle) error {
	if !s.chrome.Attached() {
		return &apiError{code: 503, err: fmt.Errorf("CDP not attached; show a message in web mode")}
	}
	if err := s.chrome.ShowMessage(text, secs*1000, st); err != nil {
		return &apiError{code: 500, err: err}
	}
	return nil
}

// ClearMessage removes the kiosk banner.
func (s *Supervisor) ClearMessage() error {
	if !s.chrome.Attached() {
		return nil
	}
	_ = s.chrome.ClearMessage()
	return nil
}

// ReloadWeb re-navigates the current web page in place — used by the watchdog to
// recover a kiosk left on an error page after a network blip. Falls back to a
// hard restart if CDP isn't attached. No-op unless the web base is on screen.
func (s *Supervisor) ReloadWeb() error {
	s.mu.Lock()
	base, fg := s.runner, s.fgRunner
	s.mu.Unlock()
	// cog kiosk (a foreground mode): reload over its D-Bus control.
	if fg != nil && fg.alive() && fg.modeType() == ModeWeb && fg.modeDisplay() == DisplayKMS {
		return s.cogControl("reload")
	}
	if fg != nil || base == nil || base.modeType() != ModeWeb {
		return nil
	}
	m := base.currentMode()
	url := m.str("url")
	if url == "" {
		return nil
	}
	if s.chrome.Attached() {
		return s.chrome.Navigate(url, m.boolOr("dark", true))
	}
	return s.RestartMode()
}

// RestartMode relaunches the on-screen mode with a fresh child (a hard restart —
// recovers a wedged Chromium that a re-navigate can't). Atomic: it claims the
// switch token for the whole teardown+relaunch (like Switch), so a concurrent
// Switch gets 409 instead of interleaving and clobbering the screen. No-op if
// nothing is on screen.
func (s *Supervisor) RestartMode() (retErr error) {
	s.mu.Lock()
	if s.switching {
		s.mu.Unlock()
		return &apiError{code: 409, err: fmt.Errorf("a mode switch is already in progress")}
	}
	s.switching = true
	base, fg := s.runner, s.fgRunner
	s.mu.Unlock()
	defer func() {
		if rec := recover(); rec != nil {
			retErr = &apiError{code: 500, err: fmt.Errorf("restart panicked: %v", rec)}
			log.Printf("PANIC in RestartMode: %v", rec)
		}
		s.mu.Lock()
		s.switching = false
		s.mu.Unlock()
	}()

	switch {
	case fg != nil:
		return s.switchForeground(fg.currentMode(), fg) // stops fg + starts a fresh one
	case base != nil:
		return s.startPrimary(base.currentMode(), base) // tears the base down + fresh child
	default:
		return nil
	}
}

// ReattachWeb re-binds the CDP controller to the web kiosk that is ALREADY
// running, WITHOUT relaunching Chromium. It is the cheap recovery for a cold
// start whose post-start attach timed out while Chromium kept coming up — common
// on the Pi, where the debug port can open just after the 60s attach window, so
// the screen is already correct but the agent can't drive it (no screenshots /
// URL control). Claims the switch token like RestartMode so it can't interleave
// with a Switch. No-op (nil) unless a live web base is on screen with CDP
// currently detached and nothing layered on top; the watchdog falls back to the
// disruptive RestartMode only if a re-attach doesn't take.
func (s *Supervisor) ReattachWeb() (retErr error) {
	s.mu.Lock()
	if s.switching {
		s.mu.Unlock()
		return &apiError{code: 409, err: fmt.Errorf("a mode switch is already in progress")}
	}
	base, fg := s.runner, s.fgRunner
	if fg != nil || base == nil || base.modeType() != ModeWeb || !base.alive() || s.chrome.Attached() {
		s.mu.Unlock()
		return nil // nothing to re-attach to (or already attached)
	}
	s.switching = true
	s.mu.Unlock()
	defer func() {
		if rec := recover(); rec != nil {
			retErr = &apiError{code: 500, err: fmt.Errorf("reattach panicked: %v", rec)}
			log.Printf("PANIC in ReattachWeb: %v", rec)
		}
		s.mu.Lock()
		s.switching = false
		s.mu.Unlock()
	}()

	m := base.currentMode()
	s.chrome.Target(s.cdpPort(m)) // attach on this backend's CDP port (X vs Wayland)
	// Reattach (not Attach): the kiosk page is already on screen and correct, so
	// we re-bind CDP WITHOUT re-navigating — no page reload, no screen flash.
	return s.chrome.Reattach(m.boolOr("dark", true), attachWaitReattach)
}

// NavigateIfWeb re-points the kiosk at url in place — but ONLY if the running
// web base is the surface on screen, atomically under the switch token, so the
// signage rotation can never resurrect a kiosk the operator just switched away
// from (to off/console/standby). Returns false (skip, retry later) if the
// surface isn't a running web kiosk, a switch is in progress, or CDP isn't
// attached (a cold/wedged browser is the watchdog's job, not the playlist's).
func (s *Supervisor) NavigateIfWeb(url string) bool {
	s.mu.Lock()
	if s.switching || s.fgRunner != nil || s.runner == nil || s.runner.modeType() != ModeWeb || !s.runner.alive() {
		s.mu.Unlock()
		return false
	}
	s.switching = true
	base := s.runner
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.switching = false
		s.mu.Unlock()
	}()

	if !s.chrome.Attached() {
		return false
	}
	m := base.currentMode()
	if err := s.chrome.Navigate(url, m.boolOr("dark", true)); err != nil {
		return false
	}
	base.setURL(url)
	return true
}

// RunSlideshowIfWeb injects the on-page slideshow overlay — but ONLY if the
// running web base is the surface on screen, atomically under the switch token
// (mirroring NavigateIfWeb). Returns false (skip, retry later) when the surface
// isn't a running web kiosk, a switch is in progress, or CDP isn't attached.
func (s *Supervisor) RunSlideshowIfWeb(images []string, intervalMs int, fit, transition string) bool {
	s.mu.Lock()
	if s.switching || s.fgRunner != nil || s.runner == nil || s.runner.modeType() != ModeWeb || !s.runner.alive() {
		s.mu.Unlock()
		return false
	}
	s.switching = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.switching = false; s.mu.Unlock() }()
	if !s.chrome.Attached() {
		return false
	}
	if err := s.chrome.RunSlideshow(images, intervalMs, fit, transition); err != nil {
		log.Printf("[slideshow] inject: %v", err)
		return false
	}
	return true
}

// StopSlideshowIfWeb removes the slideshow overlay best-effort (no-op when CDP
// isn't attached). Safe to call regardless of the on-screen mode.
func (s *Supervisor) StopSlideshowIfWeb() {
	if !s.chrome.Attached() {
		return
	}
	_ = s.chrome.StopSlideshow()
}

// AdvanceDocument scrolls the kiosk page one viewport down (the document
// auto-advance) — only if a running web kiosk is on screen, under the switch
// token. Returns false to retry later.
func (s *Supervisor) AdvanceDocument() bool {
	s.mu.Lock()
	if s.switching || s.fgRunner != nil || s.runner == nil || s.runner.modeType() != ModeWeb || !s.runner.alive() {
		s.mu.Unlock()
		return false
	}
	s.switching = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.switching = false; s.mu.Unlock() }()
	if !s.chrome.Attached() {
		return false
	}
	return s.chrome.ScrollPage() == nil
}

// CaptureCompositor grabs the X compositor surface with scrot (the screenshot
// fallback for non-web compositor modes like media/app — CDP can only shoot the
// web kiosk). Runs as the seat user into the runtime dir so the priv-dropped
// child can write the temp file. scrot is X11; grim would be the Wayland tool.
func (s *Supervisor) CaptureCompositor() ([]byte, error) {
	dir := s.cfg.RuntimeDir
	if dir == "" {
		dir = os.TempDir()
	}
	tmp, err := os.CreateTemp(dir, "disp-shot-*.png")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)
	cmd := exec.Command("scrot", "-o", "-z", path)
	cmd.Env = s.childEnv(Mode{Type: ModeMedia, Display: DisplayCompositor})
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if s.cfg.cred != nil {
		cmd.SysProcAttr.Credential = s.cfg.cred
		// CreateTemp made the file root-owned 0600; the seat-user scrot must be
		// able to write it (-o overwrites in place), so hand it ownership.
		if err := os.Chown(path, int(s.cfg.cred.Uid), int(s.cfg.cred.Gid)); err != nil {
			return nil, fmt.Errorf("chown screenshot tmp: %w", err)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("scrot: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(path)
}

// SetTheme sets the node's light/dark appearance. It applies SYSTEM-WIDE so any
// app that follows desktop settings — a GTK/Qt app, the WebKitGTK webview inside a
// custom app mode, Chromium — renders in the chosen mode, not just the kiosk. The
// Chromium web kiosk additionally gets an immediate CDP prefers-color-scheme flip
// (which doesn't depend on the portal reaching Chromium) and persists the choice.
// Works in any mode; never errors on the surface not supporting CDP.
func (s *Supervisor) SetTheme(dark bool) error {
	// System-wide first — the only path that reaches a non-Chromium surface (an
	// app's webview, the cog kiosk, a GTK app). Best-effort; never blocks the toggle.
	s.SetSystemColorScheme(dark)

	s.mu.Lock()
	base, fg := s.runner, s.fgRunner
	s.mu.Unlock()

	// The Chromium kiosk (X11 or Wayland web) also gets the live CDP flip + the
	// choice persisted, so a re-navigate/restart keeps it. cog (web+kms), app, and
	// foreground modes have no CDP — the system color-scheme above is what reaches them.
	if fg == nil && base != nil && base.modeType() == ModeWeb && base.modeDisplay() != DisplayKMS && s.chrome.Attached() {
		if err := s.chrome.SetTheme(dark); err != nil {
			log.Printf("[theme] CDP flip failed (system color-scheme still applied): %v", err)
		} else {
			base.setDark(dark)
		}
	}

	// A GUI app's WebKitGTK webview reads the color-scheme at startup, not on a live
	// portal change — bounce it so the toggle actually reaches the page. No-op for
	// the Chromium kiosk (CDP handled it above), cog, and non-GUI modes.
	s.restartGUISurface()
	return nil
}

// SetZoom applies a page-zoom factor to the live web kiosk (in place, no
// restart). Web surface only — same gating as SetTheme.
func (s *Supervisor) SetZoom(factor float64) error {
	s.mu.Lock()
	base, fg := s.runner, s.fgRunner
	s.mu.Unlock()

	// The cog kiosk (web+kms) has no runtime zoom: cog's D-Bus control only does
	// navigate/reload, and there's no CDP. (cog's --scale sets a fixed boot-time
	// zoom.) Return a clear "unsupported" so a UI can hide the control.
	if fg != nil && fg.alive() && fg.modeType() == ModeWeb && fg.modeDisplay() == DisplayKMS {
		return &apiError{code: 501, err: fmt.Errorf("runtime zoom is not supported on the cog (WPE) kiosk; set a fixed zoom with cog --scale, or use the Chromium kiosk")}
	}
	if fg != nil {
		return &apiError{code: 409, err: fmt.Errorf("a foreground mode is on screen; zoom applies to the web kiosk")}
	}
	if base == nil || base.modeType() != ModeWeb {
		return &apiError{code: 400, err: fmt.Errorf("zoom only applies in web mode")}
	}
	if !s.chrome.Attached() {
		return &apiError{code: 503, err: fmt.Errorf("CDP not attached; cannot set zoom right now")}
	}
	if err := s.chrome.SetZoom(factor); err != nil {
		return &apiError{code: 500, err: err}
	}
	return nil
}

// activateVT switches the active virtual terminal (the DRM-master handoff:
// logind makes the outgoing session drop master and the incoming one acquire
// it). No-op for VT 0/unset.
func (s *Supervisor) activateVT(n int) error {
	if n <= 0 {
		return nil
	}
	out, err := exec.Command("chvt", strconv.Itoa(n)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chvt %d: %v: %s", n, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// preflight builds (but does not start) the child for a mode to catch a missing
// binary or invalid params before the current mode is stopped — so a bad switch
// returns 400 with the working mode intact rather than blanking the screen.
func (s *Supervisor) preflight(m Mode) error {
	name, _, err := modeCommand(s.cfg, m)
	if err != nil {
		return err
	}
	// Resolve the binary the same way exec.Command will: bare names via PATH,
	// path-form names by stat. Catches a typo'd argv before any teardown.
	if strings.ContainsRune(name, '/') {
		fi, err := os.Stat(name)
		if err != nil {
			return fmt.Errorf("%s: %w", m.label(), err)
		}
		if fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s: not executable: %s", m.label(), name)
		}
	} else if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s: %w", m.label(), err)
	}
	return nil
}

// Status returns a snapshot of the on-screen mode: the foreground (console/kms)
// surface if one is active, else the compositor surface. When a foreground mode
// is on screen, the suspended compositor mode is noted in Background.
func (s *Supervisor) Status() ModeStatus {
	s.mu.Lock()
	fg, base := s.fgRunner, s.runner
	s.mu.Unlock()

	if fg != nil {
		st := fg.status()
		if base != nil {
			st.Background = base.modeType() + " (suspended)"
		}
		return st
	}
	if base != nil {
		return base.status()
	}
	return ModeStatus{Type: ModeOff, Display: DisplayCompositor, State: stateStopped}
}

// Shutdown stops both surfaces (so we never orphan Chromium/htop on exit) and
// returns the screen to the compositor VT.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	base, fg := s.runner, s.fgRunner
	s.runner, s.fgRunner = nil, nil
	s.mu.Unlock()
	if fg != nil {
		fg.Stop()
	}
	if base != nil {
		base.Stop()
	}
	s.chrome.Detach()
	_ = s.activateVT(s.cfg.XVT)
}

// ---------------------------------------------------------------------------
// modeRunner: one running mode, with restart-on-exit.
//
// Teardown is driven by a context. The run loop re-checks ctx (atomically with
// recording the just-launched child) at the top of every iteration, so Stop()
// can always reach and kill whatever child is current — there is no window
// where a freshly-restarted child is invisible to Stop(). Every child started
// is Wait()ed exactly once.
// ---------------------------------------------------------------------------

type modeRunner struct {
	sup    *Supervisor
	ctx    context.Context
	cancel context.CancelFunc
	// done is closed once the runner reaches a terminal state — either the
	// supervise loop exited, or start() failed before it could launch that loop.
	// Stop() blocks on it, so it MUST be closed on every terminal path; closeDone
	// makes the close idempotent (and impossible to double-close) so a runner
	// whose first launch failed can never wedge Stop().
	done     chan struct{}
	doneOnce sync.Once

	// onTerminal, if set, runs when the runLoop exits in the failed state (an
	// unattended crash-loop give-up — NOT an intentional Stop). Foreground
	// runners use it to return the screen to the compositor VT so a console/kms
	// crash can't strand the only screen on a dead VT.
	onTerminal func()

	mu        sync.Mutex
	mode      Mode
	cmd       *exec.Cmd
	state     string
	restarts  int
	lastErr   string
	startedAt time.Time
}

func newModeRunner(s *Supervisor, m Mode) *modeRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &modeRunner{
		sup:    s,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		mode:   m,
		state:  stateStarting,
	}
}

// start launches the first child, attaches the controller (web), and hands off
// to the supervise loop. Blocks until the first child is up or the launch fails.
func (r *modeRunner) start() error {
	cmd, tty, err := r.sup.buildCmd(r.mode)
	if err != nil {
		r.setState(stateFailed, err.Error())
		r.closeDone() // no supervise loop will run → release any Stop() waiter
		return err
	}
	if err := cmd.Start(); err != nil {
		closeFile(tty)
		r.setState(stateFailed, err.Error())
		r.closeDone() // launch failed before runLoop → release any Stop() waiter
		return fmt.Errorf("start %s: %w", r.mode.label(), err)
	}
	closeFile(tty) // child holds its own dup of the controlling TTY now
	r.record(cmd)
	log.Printf("[mode %s] started pid=%d", r.mode.label(), cmd.Process.Pid)

	if err := r.sup.postStart(r.mode); err != nil {
		// Child is up but the controller (CDP) failed to attach. Keep the child
		// running and the mode usable, but surface the error.
		r.setState(stateRunning, "post-start: "+err.Error())
		log.Printf("[mode %s] post-start hook error: %v", r.mode.label(), err)
	} else {
		r.setState(stateRunning, "")
	}
	go r.runLoop()
	return nil
}

// runLoop waits on the child and restarts it on unexpected exit, until Stop()
// cancels the context or the failure burst is exceeded.
func (r *modeRunner) runLoop() {
	defer func() {
		r.closeDone()
		// On an unattended terminal failure (state==failed, never reached on the
		// intentional-stop paths), run the recovery hook — for foreground runners
		// this returns the screen to the compositor VT.
		r.mu.Lock()
		failed := r.state == stateFailed
		r.mu.Unlock()
		if failed && r.onTerminal != nil {
			r.onTerminal()
		}
	}()
	backoff := restartBackoffMin
	var burst []time.Time

	for {
		// Read the current child and check for teardown atomically: if Stop()
		// has cancelled, kill+reap whatever we hold and exit. Because we record
		// every restarted child before looping back here, Stop() can never miss
		// a live child.
		r.mu.Lock()
		cmd := r.cmd
		startedAt := r.startedAt
		stopping := r.ctx.Err() != nil
		r.mu.Unlock()

		if stopping {
			killGroup(cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			r.setState(stateStopped, "")
			return
		}

		err := cmd.Wait()                           // block until this child exits
		killGroup(cmd.Process.Pid, syscall.SIGKILL) // reap stragglers (Chromium forks)
		if r.mode.usesChrome() {
			r.sup.chrome.Detach() // CDP ctx is dead now; clears stale "attached"
		}

		if r.ctx.Err() != nil { // intentional stop — do not respawn
			r.setState(stateStopped, "")
			return
		}

		// Unexpected exit → restart-on-exit. A long, healthy run forgives prior
		// flapping (reset backoff + burst).
		reason := exitReason(err)
		if time.Since(startedAt) >= healthyRun {
			backoff = restartBackoffMin
			burst = nil
		}
		now := time.Now()
		burst = append(filterAfter(burst, now.Add(-failWindow)), now)
		r.bumpRestart(reason)
		r.setState(stateStarting, reason) // not "running" until the new child is up
		log.Printf("[mode %s] child exited (%s); restart #%d", r.mode.label(), reason, r.countRestarts())

		if len(burst) >= failBurst {
			r.setState(stateFailed, fmt.Sprintf("crash-looping (%d exits in %s); last: %s", len(burst), failWindow, reason))
			log.Printf("[mode %s] giving up: crash loop", r.mode.label())
			return
		}

		select {
		case <-r.ctx.Done():
			r.setState(stateStopped, "")
			return
		case <-time.After(backoff):
		}
		// A ready select picks uniformly, so the timer can win even when Stop()
		// already cancelled during the backoff. Re-check before spawning: the
		// top-of-loop guard would kill a child launched here on the next pass,
		// but launching it at all flashes a throwaway Chromium onto the screen
		// (briefly two owners). Stopping now skips that wasted spawn entirely.
		if r.ctx.Err() != nil {
			r.setState(stateStopped, "")
			return
		}
		backoff = min(backoff*2, restartBackoffMax)

		// A Wayland primary owns its own VT + DRM master; re-assert the VT (and
		// the CDP port the relaunched Chromium will expose) before respawning
		// labwc, so a restart lands on the right surface rather than a dead VT.
		if r.mode.isWaylandPrimary() {
			_ = r.sup.activateVT(r.sup.primaryVT(r.mode))
			r.sup.chrome.Target(r.sup.cdpPort(r.mode))
		}

		next, tty, err := r.sup.buildCmd(r.mode)
		if err != nil {
			r.setState(stateFailed, err.Error())
			return
		}
		if err := next.Start(); err != nil {
			closeFile(tty)
			r.setState(stateFailed, err.Error())
			return
		}
		closeFile(tty) // child holds its own dup of the controlling TTY now
		r.record(next) // top of loop re-checks ctx → Stop() reaches this child
		log.Printf("[mode %s] restarted pid=%d", r.mode.label(), next.Process.Pid)
		_ = r.sup.postStart(r.mode) // best-effort re-attach (web)
		r.setState(stateRunning, "")
	}
}

// Stop requests teardown (no respawn) and blocks until the run loop has reaped
// the child. Deadlock-free: it re-reads the current child after the grace
// period, so a child restarted during teardown is still killed — and a runner
// whose launch failed (no run loop, no child) returns at once because start()
// already closed r.done.
func (r *modeRunner) Stop() {
	r.cancel()

	if pid := r.currentPID(); pid > 0 {
		killGroup(pid, syscall.SIGTERM)
	}
	select {
	case <-r.done:
		return
	case <-time.After(stopGrace):
	}
	if pid := r.currentPID(); pid > 0 { // re-read: may be a restarted child
		killGroup(pid, syscall.SIGKILL)
	}
	<-r.done
}

// closeDone closes r.done at most once. The supervise loop closes it on exit;
// start() closes it when a launch fails before the loop ever runs. Routing both
// through one sync.Once keeps "done is closed on every terminal path" true
// without risking a double-close panic.
func (r *modeRunner) closeDone() {
	r.doneOnce.Do(func() { close(r.done) })
}

func (r *modeRunner) currentPID() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil {
		return r.cmd.Process.Pid
	}
	return 0
}

func (r *modeRunner) record(cmd *exec.Cmd) {
	r.mu.Lock()
	r.cmd = cmd
	r.startedAt = time.Now()
	r.mu.Unlock()
}

func (r *modeRunner) alive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == stateRunning || r.state == stateStarting
}

func (r *modeRunner) modeType() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode.Type
}

func (r *modeRunner) modeDisplay() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode.Display
}

func (r *modeRunner) isWaylandMode() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode.isWaylandPrimary()
}

// currentMode returns a copy of the runner's mode (params deep-copied) safe to
// hand back to Switch.
func (r *modeRunner) currentMode() Mode {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.mode
	m.Params = copyParams(r.mode.Params)
	return m
}

func (r *modeRunner) sameMode(m Mode) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode.equivalent(m)
}

// runningMode reports whether this runner is up (drawing) and is the given mode.
func (r *modeRunner) runningMode(m Mode) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == stateRunning && r.mode.equivalent(m)
}

func (r *modeRunner) setMode(m Mode) {
	r.mu.Lock()
	r.mode = m
	r.mu.Unlock()
}

// setURL records the current URL on the mode's params (so a later restart/reload
// uses it).
func (r *modeRunner) setURL(url string) {
	r.mu.Lock()
	if r.mode.Params == nil {
		r.mode.Params = map[string]any{}
	}
	r.mode.Params["url"] = url
	r.mu.Unlock()
}

// setDark records the kiosk's current light/dark choice on the mode's params so
// it survives a later re-navigate or Chromium restart.
func (r *modeRunner) setDark(dark bool) {
	r.mu.Lock()
	if r.mode.Params == nil {
		r.mode.Params = map[string]any{}
	}
	r.mode.Params["dark"] = dark
	r.mu.Unlock()
}

func (r *modeRunner) setState(state, errMsg string) {
	r.mu.Lock()
	r.state = state
	if errMsg != "" {
		r.lastErr = errMsg
	}
	r.mu.Unlock()
}

func (r *modeRunner) bumpRestart(reason string) {
	r.mu.Lock()
	r.restarts++
	r.lastErr = reason
	r.mu.Unlock()
}

func (r *modeRunner) countRestarts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restarts
}

func (r *modeRunner) status() ModeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := ModeStatus{
		Type:     r.mode.Type,
		Params:   copyParams(r.mode.Params), // deep-copy: the live map is mutated under r.mu (setDark) but JSON-encoded lock-free
		Display:  r.mode.Display,
		State:    r.state,
		Restarts: r.restarts,
		LastErr:  r.lastErr,
	}
	if !r.startedAt.IsZero() {
		st.Since = r.startedAt.UTC().Format(time.RFC3339)
	}
	// Only report a live PID when the child is actually the running one (state
	// transitions to "starting" the instant Wait() returns, so we never report
	// a dead pid as Running).
	if r.cmd != nil && r.cmd.Process != nil && r.state == stateRunning {
		st.PID = r.cmd.Process.Pid
		st.Running = true
	}
	return st
}

// ---------------------------------------------------------------------------
// process-group helpers
// ---------------------------------------------------------------------------

// killGroup sends sig to the whole process group led by pid. Chromium spawns a
// tree of helpers; killing the group reaps them all. Best-effort.
func killGroup(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	// Negative pid → the process group. We set Setpgid so leader pid == pgid.
	if err := syscall.Kill(-pid, sig); err != nil {
		// Group may already be gone, or leader changed pgid; fall back to pid.
		_ = syscall.Kill(pid, sig)
	}
}

func closeFile(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

func exitReason(err error) string {
	if err == nil {
		return "exit status 0"
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.String()
	}
	return err.Error()
}

func filterAfter(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}
