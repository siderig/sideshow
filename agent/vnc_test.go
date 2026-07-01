package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVNCBackendSelection checks the capture server picked per on-screen backend:
// x11vnc for the X surface, wayvnc for the labwc Wayland primary, with view-only
// enforced on an open (non-key-protected) LAN.
func TestVNCBackendSelection(t *testing.T) {
	sup := testSupervisor(t)
	v := NewVNC(sup.cfg, sup)
	v.x11vncOK, v.wayvncOK = true, true

	// No runner → X surface → x11vnc.
	if v.waylandActive() {
		t.Fatal("no runner must not be wayland-active")
	}
	cmd, err := v.buildServerCmd()
	if err != nil {
		t.Fatalf("x11vnc build: %v", err)
	}
	if cmd.Args[0] != "x11vnc" {
		t.Fatalf("want x11vnc, got %v", cmd.Args)
	}

	// Wayland primary → wayvnc, attached to the seat user's wayland socket, and
	// view-only (--disable-input) because no auth key is set.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wayland-1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sup.cfg.RuntimeDir = dir
	sup.mu.Lock()
	sup.runner = newModeRunner(sup, Mode{Type: ModeWeb, Display: DisplayWayland, Params: map[string]any{"url": "https://x"}})
	sup.mu.Unlock()

	if !v.waylandActive() {
		t.Fatal("wayland runner should be active")
	}
	if !v.backendSupported() {
		t.Fatal("wayland backend should be supported when wayvnc present")
	}
	cmd2, err := v.buildServerCmd()
	if err != nil {
		t.Fatalf("wayvnc build: %v", err)
	}
	if cmd2.Args[0] != "wayvnc" {
		t.Fatalf("want wayvnc, got %v", cmd2.Args)
	}
	if !strings.Contains(strings.Join(cmd2.Args, " "), "--disable-input") {
		t.Errorf("open LAN must be view-only (--disable-input): %v", cmd2.Args)
	}
	if !strings.Contains(strings.Join(cmd2.Env, " "), "WAYLAND_DISPLAY=wayland-1") {
		t.Errorf("wayvnc env missing the wayland socket: %v", cmd2.Env)
	}

	// Without wayvnc installed, the wayland backend is unsupported (button hidden).
	v.wayvncOK = false
	if v.backendSupported() {
		t.Error("wayland backend must be unsupported without wayvnc")
	}

	// Key-protected → input allowed (no --disable-input).
	v.wayvncOK = true
	sup.cfg.AuthKey = "secret"
	cmd3, _ := v.buildServerCmd()
	if strings.Contains(strings.Join(cmd3.Args, " "), "--disable-input") {
		t.Errorf("key-protected surface should allow input: %v", cmd3.Args)
	}
}

// TestVNCLowResKnobs checks the low-end tunables land in the right backend args:
// x11vnc gets -scale + -wait/-defer; wayvnc gets --max-fps; and a positive
// VNCNice wraps the capture command with nice/ionice so it runs below the kiosk.
func TestVNCLowResKnobs(t *testing.T) {
	sup := testSupervisor(t)
	v := NewVNC(sup.cfg, sup)
	v.x11vncOK, v.wayvncOK = true, true

	// X surface: scale + fps cap.
	sup.cfg.VNCScale = "0.5"
	sup.cfg.VNCMaxFPS = 4 // → 250ms wait/defer
	cmd, err := v.buildServerCmd()
	if err != nil {
		t.Fatalf("x11vnc build: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"-scale 0.5", "-wait 250", "-defer 250"} {
		if !strings.Contains(joined, want) {
			t.Errorf("x11vnc low-res args missing %q: %v", want, cmd.Args)
		}
	}

	// Wayland surface: fps cap maps to --max-fps; -scale does not apply.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wayland-1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sup.cfg.RuntimeDir = dir
	sup.mu.Lock()
	sup.runner = newModeRunner(sup, Mode{Type: ModeWeb, Display: DisplayWayland, Params: map[string]any{"url": "https://x"}})
	sup.mu.Unlock()
	cmd2, err := v.buildServerCmd()
	if err != nil {
		t.Fatalf("wayvnc build: %v", err)
	}
	j2 := strings.Join(cmd2.Args, " ")
	if !strings.Contains(j2, "--max-fps=4") {
		t.Errorf("wayvnc should carry --max-fps=4: %v", cmd2.Args)
	}
	if strings.Contains(j2, "-scale") {
		t.Errorf("wayvnc has no -scale: %v", cmd2.Args)
	}

	// VNCNice>0 wraps the command with nice (and ionice when present).
	exe, argv := lowPriorityWrap(15, "x11vnc", []string{"-display", ":0"})
	if !strings.Contains(exe, "nice") && !strings.Contains(strings.Join(argv, " "), "nice") {
		t.Skip("nice/ionice not on PATH in this environment")
	}
	full := append([]string{exe}, argv...)
	fj := strings.Join(full, " ")
	if !strings.Contains(fj, "-n 15") || !strings.Contains(fj, "x11vnc -display :0") {
		t.Errorf("nice wrap malformed: %v", full)
	}

	// VNCNice==0 leaves the command unchanged.
	exe0, argv0 := lowPriorityWrap(0, "x11vnc", []string{"-display", ":0"})
	if exe0 != "x11vnc" || len(argv0) != 2 {
		t.Errorf("nice=0 must not wrap: %q %v", exe0, argv0)
	}
}
