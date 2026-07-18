package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mediaMode(params map[string]any) Mode {
	m := Mode{Type: ModeMedia, Params: params}
	m.normalize()
	return m
}

func TestMediaValidate(t *testing.T) {
	tests := []struct {
		name    string
		display string
		params  map[string]any
		wantErr string // substring; "" = expect nil
	}{
		{"http url", DisplayCompositor, map[string]any{"url": "http://ex/v.mp4"}, ""},
		{"https url", DisplayCompositor, map[string]any{"url": "https://ex/v.mp4"}, ""},
		{"rtsp url", DisplayCompositor, map[string]any{"url": "rtsp://ex/s"}, ""},
		{"rtmp url", DisplayCompositor, map[string]any{"url": "rtmp://ex/s"}, ""},
		{"file url", DisplayCompositor, map[string]any{"url": "file:///v.mp4"}, ""},
		{"abs path", DisplayCompositor, map[string]any{"path": "/srv/v.mp4"}, ""},
		{"kms allowed (mpv --vo=drm)", DisplayKMS, map[string]any{"url": "http://ex/v"}, ""},
		// media is neither web nor app, so the "display=wayland is only valid for
		// web or app modes" guard rejects it before the type switch — still a 400.
		{"wayland rejected", DisplayWayland, map[string]any{"url": "http://ex/v"}, "wayland"},
		{"console rejected", DisplayConsole, map[string]any{"url": "http://ex/v"}, "display=kms"},
		{"missing src", DisplayCompositor, map[string]any{}, "exactly one"},
		{"both src", DisplayCompositor, map[string]any{"url": "http://ex/v", "path": "/v"}, "exactly one"},
		{"relative path", DisplayCompositor, map[string]any{"path": "v.mp4"}, "absolute"},
		{"bad scheme", DisplayCompositor, map[string]any{"url": "ftp://ex/v"}, "must be"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := Mode{Type: ModeMedia, Display: tc.display, Params: tc.params}
			m.normalize()
			err := m.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAppWaylandMode(t *testing.T) {
	// app + display:wayland is now valid: a GUI app hosted as a labwc client.
	m := Mode{Type: ModeApp, Display: DisplayWayland, Params: map[string]any{"argv": []any{"/usr/bin/myapp", "--flag"}}}
	m.normalize()
	if err := m.validate(); err != nil {
		t.Fatalf("app+wayland validate() = %v, want nil", err)
	}
	if !m.isWaylandPrimary() {
		t.Error("app+wayland isWaylandPrimary() = false, want true")
	}
	if m.isWayland() {
		t.Error("app+wayland isWayland() = true, want false (it is not the Chromium web kiosk)")
	}
	// modeCommand hosts it under labwc via the embedded launcher; the app argv
	// rides through as the trailing positional args ("$@" for the launcher).
	name, args, err := modeCommand(&Config{}, m)
	if err != nil {
		t.Fatalf("modeCommand: %v", err)
	}
	if name != "/bin/sh" {
		t.Errorf("name = %q, want /bin/sh", name)
	}
	if len(args) < 5 || args[0] != "-c" || args[2] != "sh" {
		t.Fatalf("args = %v, want [-c <launcher> sh argv...]", args)
	}
	// labwc runs the startup wrapper; an optional `-C <dir>` (only under
	// -lock-input, absent here) may sit between the binary and -s.
	if !strings.Contains(args[1], "labwc ") || !strings.Contains(args[1], `-s "$WRAP"`) {
		t.Errorf("launcher does not run labwc with the startup wrapper: %q", args[1])
	}
	if args[3] != "/usr/bin/myapp" || args[4] != "--flag" {
		t.Errorf("app argv not passed through: %v", args[3:])
	}
}

// TestWebKMSMode covers the cog framebuffer backend (web + display=kms): it
// validates, launches cog directly on DRM/KMS, is the cog kiosk (NOT chrome/CDP),
// and runs as root on a foreground VT.
func TestWebKMSMode(t *testing.T) {
	cfg := &Config{Chromium: "/usr/bin/chromium", CogCmd: "cog", CogCtlCmd: "cogctl"}

	m := Mode{Type: ModeWeb, Display: DisplayKMS, Params: map[string]any{"url": "https://ex/live"}}
	m.normalize()
	if err := m.validate(); err != nil {
		t.Fatalf("web+kms should validate: %v", err)
	}
	if m.usesChrome() {
		t.Error("web+kms must not be chrome-driven")
	}
	if !m.isCogKiosk() {
		t.Error("web+kms must be the cog kiosk")
	}
	if !m.runsAsRoot(cfg) || !m.foreground() {
		t.Error("web+kms must run as root (DRM master) on a foreground VT")
	}
	// The supervised child is cog directly, on --platform=drm with the URL.
	name, args, err := modeCommand(cfg, m)
	if err != nil {
		t.Fatalf("modeCommand: %v", err)
	}
	if name != "cog" {
		t.Errorf("want cog child, got %q", name)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "--platform=drm") || !strings.Contains(joined, "https://ex/live") {
		t.Errorf("cog args = %v, want --platform=drm + url", args)
	}

	// The X11 web backend stays chrome-driven, not the cog kiosk.
	x := Mode{Type: ModeWeb, Display: DisplayCompositor, Params: map[string]any{"url": "https://ex/live"}}
	x.normalize()
	if !x.usesChrome() || x.isCogKiosk() {
		t.Error("web+compositor must remain chrome-driven (CDP), not the cog kiosk")
	}
}

// TestChromiumLowMem checks the opt-in low-memory profile only adds its flags
// when enabled, including the single-renderer + capped-heap levers.
func TestChromiumLowMem(t *testing.T) {
	base := chromiumArgs(&Config{ProfileDir: "/p"}, "https://ex")
	if strings.Contains(strings.Join(base, " "), "renderer-process-limit") {
		t.Error("low-mem flags must be off by default")
	}
	low := chromiumArgs(&Config{ProfileDir: "/p", ChromiumLowMem: true}, "https://ex")
	lj := strings.Join(low, " ")
	for _, want := range []string{"--renderer-process-limit=1", "--process-per-site", "--disable-gpu", "--js-flags=--max-old-space-size=256"} {
		if !strings.Contains(lj, want) {
			t.Errorf("low-mem profile missing %q: %v", want, low)
		}
	}
	if low[len(low)-1] != "https://ex" {
		t.Errorf("url must stay last: %v", low)
	}
}

func TestMediaModeCommand(t *testing.T) {
	cfg := &Config{}
	name, args, err := modeCommand(cfg, mediaMode(map[string]any{"url": "http://ex/v.mp4"}))
	if err != nil {
		t.Fatalf("modeCommand: %v", err)
	}
	if name != "mpv" {
		t.Fatalf("binary = %q, want mpv", name)
	}
	got := strings.Join(args, " ")
	// Defaults: loop on (--loop-file=inf), mute off (no --mute=yes).
	for _, want := range []string{"--fullscreen", "--really-quiet", "--loop-file=inf", "http://ex/v.mp4"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "--mute=yes") {
		t.Errorf("args %q should not mute by default", got)
	}
	if strings.Contains(got, "--vo=drm") {
		t.Errorf("compositor media must not use --vo=drm: %q", got)
	}
	if args[len(args)-1] != "http://ex/v.mp4" {
		t.Errorf("src must be last arg, got %q", args[len(args)-1])
	}

	// mute on, loop off.
	_, args2, _ := modeCommand(cfg, mediaMode(map[string]any{"path": "/srv/v.mp4", "loop": false, "mute": true}))
	g2 := strings.Join(args2, " ")
	if strings.Contains(g2, "--loop-file=inf") {
		t.Errorf("loop=false should omit --loop-file=inf: %q", g2)
	}
	if !strings.Contains(g2, "--mute=yes") {
		t.Errorf("mute=true should add --mute=yes: %q", g2)
	}
	if args2[len(args2)-1] != "/srv/v.mp4" {
		t.Errorf("path src must be last arg, got %q", args2[len(args2)-1])
	}
}

// TestMediaEquivalent is the regression guard for the equivalent() media case:
// without it the default branch compared argv() (nil==nil) and a URL change
// never restarted mpv.
func TestMediaEquivalent(t *testing.T) {
	a := mediaMode(map[string]any{"url": "http://ex/a.mp4"})
	sameAsA := mediaMode(map[string]any{"url": "http://ex/a.mp4"})
	if !a.equivalent(sameAsA) {
		t.Error("identical media modes (same url, default loop) should be equivalent")
	}
	diffURL := mediaMode(map[string]any{"url": "http://ex/b.mp4"})
	if a.equivalent(diffURL) {
		t.Error("different media urls must NOT be equivalent (would skip restart)")
	}
	diffMute := mediaMode(map[string]any{"url": "http://ex/a.mp4", "mute": true})
	if a.equivalent(diffMute) {
		t.Error("different mute must NOT be equivalent")
	}
}

// TestClearChromiumSingleton covers the renamed-node self-heal: a stale
// SingletonLock (a dangling "<oldhost>-<pid>" symlink) must be removed before a
// Chromium launch, the call must be idempotent, and a never-created profile dir
// must not error.
func TestClearChromiumSingleton(t *testing.T) {
	dir := t.TempDir()
	markers := []string{"SingletonLock", "SingletonSocket", "SingletonCookie"}
	// Seed the stale markers a renamed node leaves behind — SingletonLock as the
	// dangling "<oldhost>-<pid>" symlink that blocks Chromium from starting.
	for _, name := range markers {
		if err := os.Symlink("sideshow-dbc5-1065", filepath.Join(dir, name)); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	clearChromiumSingleton(dir)
	for _, name := range markers {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s: want removed, Lstat err = %v", name, err)
		}
	}
	// Idempotent (nothing left to remove) and safe on a dir that never existed.
	clearChromiumSingleton(dir)
	clearChromiumSingleton(filepath.Join(dir, "no-such-profile"))
}

// TestParseMemTotalMB covers the low-RAM auto-default decision: a Pi 3B (~905 MB)
// must parse below the threshold (so low-mem auto-enables), a multi-GB node must
// parse above it (so it does not), and malformed/absent input yields 0 ("unknown,
// skip") rather than a spurious low-mem enable.
func TestParseMemTotalMB(t *testing.T) {
	pi3b := "MemTotal:         905324 kB\nMemFree:          131072 kB\nSwapTotal:        926600 kB\n"
	if got := parseMemTotalMB(pi3b); got != 884 {
		t.Errorf("Pi 3B MemTotal = %d MB, want 884", got)
	}
	if got := parseMemTotalMB(pi3b); !(got > 0 && got < autoLowMemThresholdMB) {
		t.Errorf("Pi 3B (%d MB) must fall under the %d MB low-mem threshold", got, autoLowMemThresholdMB)
	}
	if got := parseMemTotalMB("MemTotal:        8232000 kB\n"); got <= autoLowMemThresholdMB {
		t.Errorf("8 GB node = %d MB, must exceed the %d MB threshold", got, autoLowMemThresholdMB)
	}
	for _, bad := range []string{"", "MemAvailable: 100 kB\n", "MemTotal: notanumber kB", "garbage"} {
		if got := parseMemTotalMB(bad); got != 0 {
			t.Errorf("parseMemTotalMB(%q) = %d, want 0", bad, got)
		}
	}
}

// TestSplitCSV covers the -wayland-disable-output parsing: trims spaces, drops
// empties, and yields nil (not a 1-elem [""]) for empty/comma-only input so the
// Wayland output-disable loop is a clean no-op when the flag is unset.
func TestSplitCSV(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"eDP-1", []string{"eDP-1"}},
		{"eDP-1,HDMI-A-2", []string{"eDP-1", "HDMI-A-2"}},
		{"  eDP-1 , , HDMI-A-2 ", []string{"eDP-1", "HDMI-A-2"}},
		{",", nil},
	}
	for _, tc := range tests {
		if got := splitCSV(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestEnabledWaylandOutputs parses wlr-randr's per-output enabled state so the
// hotplug watcher can spot an output labwc re-enabled (e.g. the internal panel that
// came back after the external TV woke from standby).
func TestEnabledWaylandOutputs(t *testing.T) {
	sample := `HDMI-A-1 "Sony SONY TV 0x01010101 (HDMI-A-1)"
  Make: Sony
  Enabled: yes
  Position: 1440,0
eDP-1 "Apple Computer Inc Color LCD (eDP-1)"
  Make: Apple Computer Inc
  Enabled: no
  Position: 0,0`
	got := enabledWaylandOutputs(sample)
	want := map[string]bool{"HDMI-A-1": true, "eDP-1": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("enabledWaylandOutputs = %v, want %v", got, want)
	}
	if len(enabledWaylandOutputs("")) != 0 {
		t.Error("empty input must yield an empty map")
	}
}

// TestWaylandOutputsToDisable covers the blackout guard: only disable a listed
// output when another output stays enabled to hold the kiosk — never blank the last
// screen (which stranded disp-deb-air with zero outputs overnight when the TV slept).
func TestWaylandOutputsToDisable(t *testing.T) {
	names := []string{"eDP-1"}
	tests := []struct {
		name    string
		enabled map[string]bool
		want    []string
	}{
		{"TV on → disable internal", map[string]bool{"eDP-1": true, "HDMI-A-1": true}, []string{"eDP-1"}},
		{"TV off (standby) → keep internal on", map[string]bool{"eDP-1": true, "HDMI-A-1": false}, nil},
		{"TV absent → keep internal on", map[string]bool{"eDP-1": true}, nil},
		{"internal already off", map[string]bool{"eDP-1": false, "HDMI-A-1": true}, nil},
		{"nothing enabled", map[string]bool{}, nil},
	}
	for _, tc := range tests {
		if got := waylandOutputsToDisable(tc.enabled, names); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Multiple listed outputs: only the enabled ones come back, and only with a keeper present.
	multi := waylandOutputsToDisable(map[string]bool{"eDP-1": true, "DP-2": true, "HDMI-A-1": true}, []string{"eDP-1", "DP-2"})
	if !reflect.DeepEqual(multi, []string{"eDP-1", "DP-2"}) {
		t.Errorf("multi: got %v, want [eDP-1 DP-2]", multi)
	}
}
