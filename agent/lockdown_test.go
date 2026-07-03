package main

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The locked rc.xml must be well-formed XML: labwc parses it with libxml2 and, on
// a parse error, silently falls back to its DEFAULT keybinds — i.e. a malformed
// file doesn't fail loudly, it silently un-locks the kiosk. This tokenizes the
// whole document so a stray/mismatched tag is caught here, not in the field.
func TestLabwcLockedRCWellFormed(t *testing.T) {
	dec := xml.NewDecoder(strings.NewReader(labwcLockedRC))
	for {
		if _, err := dec.Token(); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("labwcLockedRC is not well-formed XML: %v", err)
		}
	}
	// The lockdown works by defining binds so labwc does NOT load its defaults;
	// a stray <default/> would pull every default keybind back in.
	if strings.Contains(labwcLockedRC, "<default") {
		t.Error("labwcLockedRC must not contain <default/> — it would reload all default binds")
	}
	if !strings.Contains(labwcLockedRC, "<labwc_config>") {
		t.Error("labwcLockedRC missing the <labwc_config> root element labwc expects")
	}
	// At least one keybind AND one mousebind entry — each is what suppresses the
	// corresponding default set (labwc-config(5)).
	if !strings.Contains(labwcLockedRC, "<keybind ") {
		t.Error("labwcLockedRC has no <keybind>; labwc would load default keybinds")
	}
	if !strings.Contains(labwcLockedRC, "<mousebind ") {
		t.Error("labwcLockedRC has no <mousebind>; labwc would load default mousebinds (incl. the root menu)")
	}
}

// The matchbox kbdconfig must contain NO bindings (comment/blank lines only): a
// binding line is "<keys>=action", so any line with '=' means the kiosk kept a
// shortcut. matchbox skips '#' and blank lines.
func TestMatchboxLockedKbdHasNoBindings(t *testing.T) {
	for i, line := range strings.Split(matchboxLockedKbd, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		t.Errorf("matchboxLockedKbd line %d is a live binding, want comments only: %q", i+1, line)
	}
}

// On an X11 node (-start-x), EnsureKioskLockdown writes both artifacts — the labwc
// config dir under the state dir, and the empty matchbox kbdconfig in the seat
// user's home — and their content matches the embedded templates.
func TestEnsureKioskLockdownWritesX11(t *testing.T) {
	base := t.TempDir()
	cfg := &Config{LockInput: true, StartX: true, Home: base, StateFile: filepath.Join(base, "display.json")}
	EnsureKioskLockdown(cfg)

	rc, err := os.ReadFile(filepath.Join(base, "labwc", "rc.xml"))
	if err != nil {
		t.Fatalf("labwc rc.xml not written: %v", err)
	}
	if string(rc) != labwcLockedRC {
		t.Error("labwc rc.xml content does not match the template")
	}
	kbd, err := os.ReadFile(filepath.Join(base, ".matchbox", "kbdconfig"))
	if err != nil {
		t.Fatalf("matchbox kbdconfig not written: %v", err)
	}
	if string(kbd) != matchboxLockedKbd {
		t.Error("matchbox kbdconfig content does not match the template")
	}
}

// On a Wayland node (no -start-x), only the labwc config is written — matchbox
// isn't running there, so its home dir is left untouched.
func TestEnsureKioskLockdownWaylandSkipsMatchbox(t *testing.T) {
	base := t.TempDir()
	cfg := &Config{LockInput: true, StartX: false, Home: base, StateFile: filepath.Join(base, "display.json")}
	EnsureKioskLockdown(cfg)

	if _, err := os.Stat(filepath.Join(base, "labwc", "rc.xml")); err != nil {
		t.Errorf("labwc rc.xml should be written on a Wayland node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, ".matchbox")); !os.IsNotExist(err) {
		t.Errorf("matchbox home dir created on a non-X11 node (err=%v)", err)
	}
}

// With -lock-input off, EnsureKioskLockdown writes nothing — the launchers omit
// -C and matchbox keeps its built-in shortcuts, so there is nothing to clean up.
func TestEnsureKioskLockdownOffIsNoop(t *testing.T) {
	base := t.TempDir()
	cfg := &Config{LockInput: false, StartX: true, Home: base, StateFile: filepath.Join(base, "display.json")}
	EnsureKioskLockdown(cfg)

	if _, err := os.Stat(filepath.Join(base, "labwc")); !os.IsNotExist(err) {
		t.Errorf("labwc config dir created with -lock-input off (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(base, ".matchbox")); !os.IsNotExist(err) {
		t.Errorf("matchbox home dir created with -lock-input off (err=%v)", err)
	}
}

// Turning -lock-input off must actively remove the agent-written matchbox
// kbdconfig: matchbox reads that fixed path unconditionally (no -C to drop), so a
// leftover empty file would keep the kiosk locked after the flag is unset.
func TestEnsureKioskLockdownRestoresMatchboxOnOff(t *testing.T) {
	base := t.TempDir()
	kbd := filepath.Join(base, ".matchbox", "kbdconfig")
	state := filepath.Join(base, "display.json")

	EnsureKioskLockdown(&Config{LockInput: true, StartX: true, Home: base, StateFile: state})
	if _, err := os.Stat(kbd); err != nil {
		t.Fatalf("matchbox kbdconfig not written while locked: %v", err)
	}
	EnsureKioskLockdown(&Config{LockInput: false, StartX: true, Home: base, StateFile: state})
	if _, err := os.Stat(kbd); !os.IsNotExist(err) {
		t.Errorf("stale matchbox kbdconfig survived -lock-input off (err=%v) — kiosk stays locked", err)
	}
}

// The off-path removal must only touch OUR file: a user's own ~/.matchbox/kbdconfig
// (different content) is left intact.
func TestEnsureKioskLockdownOffKeepsUserMatchboxConfig(t *testing.T) {
	base := t.TempDir()
	kbd := filepath.Join(base, ".matchbox", "kbdconfig")
	if err := os.MkdirAll(filepath.Dir(kbd), 0o755); err != nil {
		t.Fatal(err)
	}
	userCfg := "<alt>Tab=next\n" // a real user binding, not the agent's template
	if err := os.WriteFile(kbd, []byte(userCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	EnsureKioskLockdown(&Config{LockInput: false, StartX: true, Home: base, StateFile: filepath.Join(base, "display.json")})
	if b, err := os.ReadFile(kbd); err != nil || string(b) != userCfg {
		t.Errorf("user's own ~/.matchbox/kbdconfig was clobbered on -lock-input off: %q err=%v", b, err)
	}
}

// With no -state-file, the labwc config can't be placed, so childEnv must NOT set
// SIDESHOW_LABWC_CONFIG — otherwise labwc gets `-C <relative-dir>` that was never
// written and silently loads its full defaults (guards must match the writer).
func TestChildEnvNoLabwcConfigWhenNoStateFile(t *testing.T) {
	sup := testSupervisor(t)
	sup.cfg.LockInput = true
	sup.cfg.StateFile = ""
	wayland := Mode{Type: ModeWeb, Display: DisplayWayland, Params: map[string]any{"url": "https://example.test/"}}
	if env := strings.Join(sup.childEnv(wayland), "\n"); strings.Contains(env, "SIDESHOW_LABWC_CONFIG") {
		t.Errorf("childEnv set SIDESHOW_LABWC_CONFIG with empty -state-file (labwc -C would point at an unwritten dir): %v", sup.childEnv(wayland))
	}
}

// The Wayland primary's env carries SIDESHOW_LABWC_CONFIG (the -C dir the launcher
// hands labwc) only under -lock-input, and only for a Wayland primary.
func TestChildEnvLabwcConfigOnlyWhenLocked(t *testing.T) {
	sup := testSupervisor(t)
	sup.cfg.StateFile = "/var/lib/sideshow/display.json"
	wayland := Mode{Type: ModeWeb, Display: DisplayWayland, Params: map[string]any{"url": "https://example.test/"}}
	compositor := Mode{Type: ModeWeb, Display: DisplayCompositor, Params: map[string]any{"url": "https://example.test/"}}
	want := "SIDESHOW_LABWC_CONFIG=/var/lib/sideshow/labwc"

	// Off → absent.
	sup.cfg.LockInput = false
	if env := strings.Join(sup.childEnv(wayland), "\n"); strings.Contains(env, "SIDESHOW_LABWC_CONFIG=") {
		t.Errorf("wayland env has SIDESHOW_LABWC_CONFIG with -lock-input off: %v", sup.childEnv(wayland))
	}

	// On + Wayland primary (seat-user path) → present with the state-dir sibling.
	sup.cfg.LockInput = true
	if env := strings.Join(sup.childEnv(wayland), "\n"); !strings.Contains(env, want) {
		t.Errorf("wayland env missing %q; got %v", want, sup.childEnv(wayland))
	}

	// On + legacy root labwc → still present.
	sup.cfg.WaylandRoot = true
	if env := strings.Join(sup.childEnv(wayland), "\n"); !strings.Contains(env, want) {
		t.Errorf("wayland-root env missing %q; got %v", want, sup.childEnv(wayland))
	}
	sup.cfg.WaylandRoot = false

	// On but a non-Wayland (X11 compositor) mode → absent (that path uses matchbox).
	if env := strings.Join(sup.childEnv(compositor), "\n"); strings.Contains(env, "SIDESHOW_LABWC_CONFIG=") {
		t.Errorf("compositor env should not carry SIDESHOW_LABWC_CONFIG; got %v", sup.childEnv(compositor))
	}
}

// enforceNoVTSwitch adds -novtswitch when absent and is idempotent when present,
// so a locked-down node's Xorg never allows Ctrl+Alt+Fn even if the operator
// rewrote -x-server-args.
func TestEnforceNoVTSwitch(t *testing.T) {
	if got := enforceNoVTSwitch("-seat seat0"); got != "-seat seat0 -novtswitch" {
		t.Errorf("enforceNoVTSwitch(add) = %q", got)
	}
	if got := enforceNoVTSwitch("-seat seat0 -novtswitch"); got != "-seat seat0 -novtswitch" {
		t.Errorf("enforceNoVTSwitch(idempotent) = %q", got)
	}
	if got := enforceNoVTSwitch(""); got != "-novtswitch" {
		t.Errorf("enforceNoVTSwitch(empty) = %q", got)
	}
}

// The seat-level VT lockdown applies only under -lock-input on a NON-X node (an
// agent-owned Xorg uses -novtswitch instead).
func TestVTLockActive(t *testing.T) {
	if !vtLockActive(&Config{LockInput: true, StartX: false}) {
		t.Error("wayland (!StartX) + lock-input should be VT-lock-active")
	}
	if vtLockActive(&Config{LockInput: true, StartX: true}) {
		t.Error("X11 (StartX) uses -novtswitch, not the getty mask")
	}
	if vtLockActive(&Config{LockInput: false, StartX: false}) {
		t.Error("no -lock-input → not active")
	}
}

// reconcileFile writes when want, removes when not, and reports change accurately
// (so the caller reloads udev only when the on-disk state actually changed).
func TestReconcileFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "rule.conf")
	body := "hello\n"

	if !reconcileFile(path, body, true, "test") { // create
		t.Error("first write should report changed=true")
	}
	if b, _ := os.ReadFile(path); string(b) != body {
		t.Errorf("content = %q, want %q", b, body)
	}
	if reconcileFile(path, body, true, "test") { // unchanged
		t.Error("re-writing identical content should report changed=false")
	}
	if !reconcileFile(path, body, false, "test") { // remove
		t.Error("removing a present file should report changed=true")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be gone (err=%v)", err)
	}
	if reconcileFile(path, body, false, "test") { // absent
		t.Error("removing an absent file should report changed=false")
	}
}

// The no-local-input rule bodies must carry the actual ignore directive for each
// stack — a typo would silently leave input live.
func TestNoInputRuleBodies(t *testing.T) {
	if !strings.Contains(noInputUdevBody, "LIBINPUT_IGNORE_DEVICE") || !strings.Contains(noInputUdevBody, "ID_INPUT_KEYBOARD") {
		t.Errorf("udev body missing the libinput ignore directive:\n%s", noInputUdevBody)
	}
	if !strings.Contains(noInputXorgBody, `Option "Ignore" "on"`) || !strings.Contains(noInputXorgBody, `MatchIsKeyboard "on"`) {
		t.Errorf("xorg body missing the Ignore InputClass:\n%s", noInputXorgBody)
	}
}

// The masked range must be exactly the tty1-6 autovt gettys (the agent's own VTs
// live on tty≥7, so masking never touches them), and the logind override must
// actually disable autovt spawning.
func TestVTLockGettysAndDropin(t *testing.T) {
	want := []string{
		"getty@tty1.service", "getty@tty2.service", "getty@tty3.service",
		"getty@tty4.service", "getty@tty5.service", "getty@tty6.service",
	}
	if got := vtLockGettys(); !reflect.DeepEqual(got, want) {
		t.Errorf("vtLockGettys() = %v, want %v", got, want)
	}
	if !strings.Contains(vtLockDropinBody, "NAutoVTs=0") || !strings.Contains(vtLockDropinBody, "ReserveVT=0") {
		t.Errorf("vtLockDropinBody must set NAutoVTs=0 + ReserveVT=0: %q", vtLockDropinBody)
	}
}
