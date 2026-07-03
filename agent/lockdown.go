package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kiosk input lockdown (-lock-input). A kiosk should own the screen the way Cage
// does: the operator cannot switch away from, close, or pop a menu over the active
// mode with the local keyboard/mouse. The agent already keeps X11 VT-locked
// (-novtswitch); the two remaining local escapes are the compositor's own
// window-management shortcuts. This strips them on both bases:
//
//   - labwc (Wayland): a config dir with an rc.xml that defines a few inert
//     keybinds/mousebinds. labwc loads its FULL set of default binds only when no
//     <keybind>/<mousebind> entries exist at all (labwc-config(5)); having any
//     entry suppresses every default, so Alt+Tab window-cycling, Super-key tiling,
//     Alt+F4 close, and the right-click root menu all vanish. The launchers point
//     labwc at this dir via `-C` (see run-wayland.sh / waylandAppLauncherSh), so
//     the node's own ~/.config/labwc is never touched.
//   - matchbox (X11): an empty ~/.matchbox/kbdconfig in the seat user's home.
//     matchbox-window-manager has NO command-line config option (its flags are
//     just -display/-theme/-use_*); it reads keyboard shortcuts from that fixed
//     path, and a user file OVERRIDES the built-in set — so an empty one removes
//     matchbox's shortcuts (Alt+Tab next, Alt+F4 close, Alt+space taskmenu, …).
//     Harmless (a no-op) on a build without keyboard support.
//
// NOT covered here: VT switching under Wayland (Ctrl+Alt+Fn). wlroots handles that
// below labwc and exposes no knob to disable it — close that path at the seat with
// logind/getty (mask the login gettys; see docs/node-api.md). X11 stays covered by
// -novtswitch, enforced in Config.resolve when -lock-input is set.

// labwcLockedRC is the rc.xml the agent writes under -lock-input. The six inert
// keybinds both (a) guarantee ≥1 <keybind> entry so labwc suppresses ALL of its
// default keybinds, and (b) name the common escape shortcuts explicitly, so the
// intent is legible and they are neutralized even if a future labwc changes how
// the "no entries → load defaults" fallback counts a bare None. The single Root
// mousebind does the same for mousebinds, killing the right-click root menu.
const labwcLockedRC = `<!--
  sideshow kiosk input lockdown (-lock-input). AGENT-MANAGED — regenerated at boot,
  do not edit. labwc loads its default keybinds/mousebinds only when this file
  defines none (labwc-config(5)); the inert binds below suppress every default, so
  the kiosk client cannot be switched away from, closed, or covered by a menu via
  the keyboard/mouse. This does NOT disable VT switching (Ctrl+Alt+Fn) — wlroots
  owns that below labwc; mask the login gettys to close it (see docs/node-api.md).
-->
<labwc_config>
  <keyboard>
    <keybind key="A-Tab"><action name="None"/></keybind>
    <keybind key="A-S-Tab"><action name="None"/></keybind>
    <keybind key="A-Escape"><action name="None"/></keybind>
    <keybind key="A-space"><action name="None"/></keybind>
    <keybind key="A-F4"><action name="None"/></keybind>
    <keybind key="W-Tab"><action name="None"/></keybind>
  </keyboard>
  <mouse>
    <context name="Root">
      <mousebind button="Right" action="Press"><action name="None"/></mousebind>
    </context>
  </mouse>
</labwc_config>
`

// matchboxLockedKbd is the empty ~/.matchbox/kbdconfig the agent writes under
// -lock-input: comment lines only (matchbox skips '#' lines and blanks), i.e. zero
// bindings. A user kbdconfig replaces matchbox's built-in shortcut set, so an empty
// one leaves nothing to switch or close the kiosk with.
const matchboxLockedKbd = `# sideshow kiosk input lockdown (-lock-input). AGENT-MANAGED — regenerated at boot,
# do not edit. Empty binding set: this file overrides matchbox's built-in
# shortcuts (Alt+Tab next, Alt+Shift+Tab prev, Alt+F4 close, Alt+space taskmenu,
# Alt+Escape menu) with nothing, so the kiosk client cannot be switched away from
# or closed via the keyboard.
`

// labwcConfigDir is the agent-owned labwc config directory the launchers pass to
// `labwc -C`. A sibling of the state file (/var/lib/sideshow/labwc by default), so
// it inherits the same root-owned, world-readable location every other agent state
// dir uses — labwc reads it as the seat user.
func labwcConfigDir(cfg *Config) string {
	return filepath.Join(filepath.Dir(cfg.StateFile), "labwc")
}

// matchboxKbdConfigPath is the fixed path matchbox reads a user keyboard config
// from: <seat-home>/.matchbox/kbdconfig.
func matchboxKbdConfigPath(cfg *Config) string {
	return filepath.Join(cfg.Home, ".matchbox", "kbdconfig")
}

// labwcLockActive reports whether the labwc lockdown is both requested
// (-lock-input) and placeable — it needs a state dir to hold its `labwc -C` config
// dir. childEnv (which sets the SIDESHOW_LABWC_CONFIG the launcher turns into -C)
// and EnsureKioskLockdown (which writes the rc.xml there) MUST agree on this: if
// childEnv points labwc at a dir the writer skipped, labwc finds no binds and
// silently loads its full defaults — the exact un-lock this feature prevents.
func labwcLockActive(cfg *Config) bool {
	return cfg.LockInput && cfg.StateFile != ""
}

// EnsureKioskLockdown reconciles the compositor lockdown config with -lock-input.
// It must run on EVERY start (not just when locking), because the two bases undo
// differently:
//   - labwc (Wayland): the config is consumed only via `labwc -C`, and childEnv
//     emits SIDESHOW_LABWC_CONFIG only when labwcLockActive — so with the flag off
//     the launcher just drops -C and labwc reverts. Nothing to clean up; a leftover
//     rc.xml is never read.
//   - matchbox (X11): it reads ~/.matchbox/kbdconfig UNCONDITIONALLY (it has no
//     config flag to drop), so a file left from a prior locked boot keeps the kiosk
//     locked after the operator unset -lock-input. Turning the flag off must
//     actively REMOVE the file we wrote.
//
// Idempotent (rewrites only on change) and best-effort: errors log and continue.
// Call after resolve() (cfg.cred/cfg.Home set) and before the initial mode switch.
func EnsureKioskLockdown(cfg *Config) {
	// matchbox (X11 nodes only): reconcile both directions — write the empty
	// kbdconfig when locking, remove our file when not — so the off state truly
	// restores matchbox's built-in shortcuts.
	if cfg.StartX && cfg.Home != "" {
		reconcileManagedFile(cfg, matchboxKbdConfigPath(cfg), matchboxLockedKbd, cfg.LockInput, "matchbox kbdconfig")
	}

	if !cfg.LockInput {
		return
	}
	// labwc (Wayland): write the agent-owned `-C` config dir. Gated identically to
	// childEnv's env var (labwcLockActive) so the two can never disagree.
	if !labwcLockActive(cfg) {
		log.Printf("[lockdown] no -state-file: labwc keybind lockdown inactive")
		return
	}
	dir := labwcConfigDir(cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[lockdown] mkdir %s: %v (labwc keybind lockdown not applied)", dir, err)
		return
	}
	writeIfChanged(filepath.Join(dir, "rc.xml"), labwcLockedRC, "labwc rc.xml")
}

// reconcileManagedFile drives an agent-managed file to the desired lock state.
// want==true: ensure the parent dir exists (handed to the seat user) and the file
// holds body. want==false: remove the file, but ONLY when its content is exactly
// body — i.e. a file WE wrote — so a user's own kbdconfig is never deleted.
func reconcileManagedFile(cfg *Config, path, body string, want bool, what string) {
	if want {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Printf("[lockdown] mkdir %s: %v (%s lockdown not applied)", filepath.Dir(path), err, what)
			return
		}
		chownToSeat(cfg, filepath.Dir(path))
		writeIfChanged(path, body, what)
		chownToSeat(cfg, path)
		return
	}
	if cur, err := os.ReadFile(path); err != nil || string(cur) != body {
		return // absent, unreadable, or a user's own file → leave it
	}
	if err := os.Remove(path); err != nil {
		log.Printf("[lockdown] remove stale %s (%s): %v", path, what, err)
		return
	}
	log.Printf("[lockdown] -lock-input off: removed agent %s (%s), restored built-in shortcuts", path, what)
}

// writeIfChanged writes want to path (0644) only when the current contents differ,
// logging the outcome. The parent dir must already exist.
func writeIfChanged(path, want, what string) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
		return // already current
	}
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		log.Printf("[lockdown] write %s (%s): %v", path, what, err)
		return
	}
	log.Printf("[lockdown] wrote %s (%s)", path, what)
}

// chownToSeat hands an agent-written file/dir under the seat user's home to the
// seat user, so a root-run agent doesn't leave root-owned droppings in it. No-op
// when not dropping privileges (cfg.cred nil — already running as the seat user).
func chownToSeat(cfg *Config, path string) {
	if cfg.cred == nil {
		return
	}
	_ = os.Chown(path, int(cfg.cred.Uid), int(cfg.cred.Gid))
}

// enforceNoVTSwitch appends -novtswitch to the X server args when -lock-input is
// set and it is not already present, so an operator who overrode -x-server-args
// can't accidentally re-enable Ctrl+Alt+Fn on a locked-down node.
func enforceNoVTSwitch(args string) string {
	if strings.Contains(args, "-novtswitch") {
		return args
	}
	return strings.TrimSpace(args + " -novtswitch")
}

// --- Seat-level VT-switch lockdown (the Wayland/non-X counterpart of -novtswitch) ---
//
// An agent-owned Xorg blocks Ctrl+Alt+Fn with -novtswitch, but wlroots/labwc has no
// such knob. So on a NON-X -lock-input node the escape is closed at the seat: stop
// logind auto-spawning login gettys (NAutoVTs=0/ReserveVT=0) and mask the static
// console gettys, so a VT switch lands on a dead console with no login/shell. At the
// defaults the agent's own VTs (X on -x-vt=7, labwc on -wayland-vt=8, foreground
// modes on -mode-vt=9) sit outside the masked tty1-6 range — and even a compositor
// deliberately placed on tty1-6 is unaffected, since it owns the VT directly (via
// XDG_VTNR), not through a getty.
//
// The drop-in doubles as the "agent applied the VT lock" marker: it is written
// BEFORE any getty is masked, and removed AFTER unmasking on the way out, so an
// interrupted apply/revert is always finished on the next boot — masks can never
// get stuck on with no marker to trigger the unmask. Trade-off: the off path
// unmasks the whole tty1-6 range (not a saved list), so a getty the operator masked
// by hand is re-enabled; re-mask it if you need it kept.

const vtLockDropin = "/etc/systemd/logind.conf.d/10-sideshow-kiosk-novt.conf"

const vtLockDropinBody = `# sideshow kiosk hardening (AGENT-MANAGED under -lock-input; removed when the flag
# is off). wlroots/labwc cannot disable Ctrl+Alt+Fn VT switching, so make every VT
# switch land on a DEAD console: no autologin getty is spawned. Paired with masking
# getty@tty1..6. See docs/node-api.md.
[Login]
NAutoVTs=0
ReserveVT=0
`

// vtLockGettys is the console-getty autovt range the lockdown masks (tty1-6).
func vtLockGettys() []string {
	return []string{
		"getty@tty1.service", "getty@tty2.service", "getty@tty3.service",
		"getty@tty4.service", "getty@tty5.service", "getty@tty6.service",
	}
}

// vtLockActive reports whether the seat-level VT lockdown applies: only under
// -lock-input, and only on a non-X node (an agent-owned Xorg uses -novtswitch
// instead, enforced in resolve()).
func vtLockActive(cfg *Config) bool {
	return cfg.LockInput && !cfg.StartX
}

// ReconcileVTLockdown applies or reverts the seat-level VT-switch lockdown to match
// -lock-input on a non-X node. Root-only (it masks system units) and best-effort: a
// failure logs and continues — the kiosk still runs. Idempotent: mask over a fixed
// range is a no-op when already masked, so it is safe to run on every boot. Run in
// a goroutine at boot — it shells out to systemctl.
func ReconcileVTLockdown(cfg *Config) {
	if !vtLockActive(cfg) {
		revertVTLockdown()
		return
	}
	if os.Geteuid() != 0 {
		log.Printf("[lockdown] not root: seat VT lockdown (getty mask) skipped")
		return
	}
	// Write the logind override FIRST — it is also the marker the off path keys on,
	// so an apply interrupted mid-mask can still be reverted (masks can't get stuck
	// on with no marker). Bail if the marker can't be written, so we never mask
	// gettys the off path wouldn't then know to unmask.
	if err := writeVTLockDropin(); err != nil {
		log.Printf("[lockdown] write %s: %v (seat VT lockdown not applied)", vtLockDropin, err)
		return
	}
	failed := 0
	for _, u := range vtLockGettys() {
		if out, err := runShort(10*time.Second, "systemctl", "mask", u); err != nil {
			log.Printf("[lockdown] mask %s: %v: %s", u, err, out)
			failed++
			continue
		}
		_, _ = runShort(10*time.Second, "systemctl", "stop", u) // drop any running login
	}
	_, _ = runShort(10*time.Second, "systemctl", "reload", "systemd-logind")
	// Report honestly: don't claim "active" if a mask failed. NAutoVTs=0 still
	// backstops (logind won't auto-spawn a getty on VT switch even on an unmasked
	// unit), but the operator should know a mask didn't take.
	if failed == 0 {
		log.Printf("[lockdown] seat VT lockdown active: getty@tty1..6 masked, logind NAutoVTs=0 (Ctrl+Alt+Fn → no login)")
	} else {
		log.Printf("[lockdown] seat VT lockdown PARTIAL: %d/%d getty mask(s) failed; logind NAutoVTs=0 still blocks autovt logins", failed, len(vtLockGettys()))
	}
}

// revertVTLockdown lifts a prior VT lockdown, keyed on the marker drop-in: unmask
// the whole tty1-6 range and drop the override. A no-op when the marker is absent
// (the agent never applied it) — cheap on X11 / fresh nodes. Unmasking the fixed
// range (rather than a saved list) means it can never leave a getty stuck masked;
// the marker is removed LAST, so an interrupted revert is retried on the next boot.
func revertVTLockdown() {
	if _, err := os.Stat(vtLockDropin); err != nil {
		return // no marker → the agent never applied the VT lock
	}
	if os.Geteuid() != 0 {
		log.Printf("[lockdown] not root: cannot revert seat VT lockdown (getty unmask needs root); marker + masks left for a root run")
		return
	}
	for _, u := range vtLockGettys() {
		if out, err := runShort(10*time.Second, "systemctl", "unmask", u); err != nil {
			log.Printf("[lockdown] unmask %s: %v: %s", u, err, out)
		}
	}
	_, _ = runShort(10*time.Second, "systemctl", "reload", "systemd-logind")
	// Marker LAST: if the unmask loop is interrupted, the marker survives so the
	// next boot finishes the revert (masks never stay stuck on).
	if err := os.Remove(vtLockDropin); err != nil && !os.IsNotExist(err) {
		log.Printf("[lockdown] remove %s: %v", vtLockDropin, err)
	}
	log.Printf("[lockdown] -lock-input off (or X node): seat VT lockdown reverted (getty@tty1..6 unmasked, override dropped)")
}

// writeVTLockDropin writes the agent's logind override (NAutoVTs=0/ReserveVT=0).
func writeVTLockDropin() error {
	if err := os.MkdirAll(filepath.Dir(vtLockDropin), 0o755); err != nil {
		return err
	}
	return os.WriteFile(vtLockDropin, []byte(vtLockDropinBody), 0o644)
}

// --- No local input (pure display) ---
//
// -lock-input strips the compositor's escape shortcuts but still lets the local
// keyboard/mouse drive the kiosk client (Chromium's own Ctrl+N / click / type). For
// a display that must take NO local input at all, -no-local-input makes the input
// stack skip the physical devices entirely, by the means each stack honors (the
// libinput property alone is not enough under X — see its docs):
//   - Wayland/libinput: a udev rule tagging keyboards/pointers with
//     LIBINPUT_IGNORE_DEVICE, so wlroots never binds them.
//   - X11: an xorg.conf.d InputClass with Option "Ignore" "on" (the Xorg-native
//     way), written additionally on -start-x nodes.
// Remote control (wayvnc / the panel) injects through wlroots virtual-input, created
// in-compositor and unaffected by either rule. Power/lid/sleep buttons are not
// matched. Both files are agent-managed and removed when the flag is off; the change
// takes effect when the compositor (re)starts (the agent restart on deploy).

const noInputUdevRule = "/etc/udev/rules.d/99-sideshow-noinput.rules"

const noInputUdevBody = `# sideshow: pure display — NO local input. wlroots/libinput ignores these devices
# (LIBINPUT_IGNORE_DEVICE); remote control (wayvnc/panel) uses virtual-input and is
# unaffected. Power/lid/sleep buttons are not matched. AGENT-MANAGED
# (-no-local-input); removed when the flag is off. Effective on compositor (re)start.
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_KEYBOARD}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_MOUSE}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_TOUCHPAD}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_TOUCHSCREEN}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_POINTINGSTICK}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_TABLET}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_TABLET_PAD}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
ACTION=="add|change", SUBSYSTEM=="input", ENV{ID_INPUT_JOYSTICK}=="1", ENV{LIBINPUT_IGNORE_DEVICE}="1"
`

const noInputXorgConf = "/etc/X11/xorg.conf.d/99-sideshow-noinput.conf"

const noInputXorgBody = `# sideshow: pure display — NO local input (X11). Xorg ignores local input devices.
# AGENT-MANAGED (-no-local-input); removed when the flag is off. Effective on Xorg
# (re)start.
Section "InputClass"
    Identifier "sideshow-noinput-keyboard"
    MatchIsKeyboard "on"
    Option "Ignore" "on"
EndSection
Section "InputClass"
    Identifier "sideshow-noinput-pointer"
    MatchIsPointer "on"
    Option "Ignore" "on"
EndSection
Section "InputClass"
    Identifier "sideshow-noinput-touchpad"
    MatchIsTouchpad "on"
    Option "Ignore" "on"
EndSection
Section "InputClass"
    Identifier "sideshow-noinput-touchscreen"
    MatchIsTouchscreen "on"
    Option "Ignore" "on"
EndSection
Section "InputClass"
    Identifier "sideshow-noinput-tablet"
    MatchIsTablet "on"
    Option "Ignore" "on"
EndSection
Section "InputClass"
    Identifier "sideshow-noinput-joystick"
    MatchIsJoystick "on"
    Option "Ignore" "on"
EndSection
`

// EnsureNoLocalInput reconciles the -no-local-input rules: a libinput udev rule (all
// nodes) and, on an agent-owned-X node, an xorg.conf.d Ignore snippet. Root-only,
// best-effort. The compositor picks up the change when it (re)starts, so this runs
// synchronously BEFORE the compositor is launched (see main). Reversible — off
// removes the files and local input returns on the next compositor start.
func EnsureNoLocalInput(cfg *Config) { ApplyNoLocalInput(cfg, cfg.NoLocalInput) }

// ApplyNoLocalInput reconciles the pure-display input lockout to an explicit
// blocked value (true = ignore ALL local input). It is the runtime-callable form of
// EnsureNoLocalInput: the /api/input toggle and the boot seed both drive it from the
// persisted policy rather than the -no-local-input flag alone. Root-only,
// best-effort; returns whether the on-disk rules changed, so a caller re-enumerates
// input (restarts the compositor) only when something actually moved. The change is
// picked up when the compositor (re)starts.
func ApplyNoLocalInput(cfg *Config, blocked bool) (changed bool) {
	if os.Geteuid() != 0 {
		if blocked {
			log.Printf("[lockdown] not root: no-local-input skipped (needs root for udev/Xorg rules)")
		}
		return false
	}
	udevChanged := reconcileFile(noInputUdevRule, noInputUdevBody, blocked, "no-local-input udev rule")
	// Xorg-native ignore, only on an agent-owned-X node (definitive for X11, atop the
	// udev rule which libinput's docs say X may not honor).
	xorgChanged := reconcileFile(noInputXorgConf, noInputXorgBody, blocked && cfg.StartX, "no-local-input Xorg rule")
	if udevChanged {
		// Re-apply the property to already-enumerated devices; wlroots reads it when
		// it (re)starts.
		_, _ = runShort(10*time.Second, "udevadm", "control", "--reload-rules")
		_, _ = runShort(10*time.Second, "udevadm", "trigger", "--subsystem-match=input")
		_, _ = runShort(10*time.Second, "udevadm", "settle", "--timeout=5")
	}
	return udevChanged || xorgChanged
}

// reconcileFile writes body to path when want, else removes it; returns whether the
// on-disk state changed (so the caller can reload only when needed). Best-effort
// (logs errors). For agent-owned files under /etc (root-owned, fixed names), so
// removal is unconditional.
func reconcileFile(path, body string, want bool, what string) (changed bool) {
	if want {
		if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
			return false // already current
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Printf("[lockdown] mkdir %s: %v (%s not applied)", filepath.Dir(path), err, what)
			return false
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			log.Printf("[lockdown] write %s (%s): %v", path, what, err)
			return false
		}
		log.Printf("[lockdown] wrote %s (%s)", path, what)
		return true
	}
	if err := os.Remove(path); err == nil {
		log.Printf("[lockdown] removed %s (%s)", path, what)
		return true
	} else if !os.IsNotExist(err) {
		log.Printf("[lockdown] remove %s (%s): %v", path, what, err)
	}
	return false
}
