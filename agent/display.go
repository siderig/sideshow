package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// geometryRe matches an xrandr CRTC geometry like "1920x1080+0+0" (offsets may
// be negative on a multi-head extend layout).
var geometryRe = regexp.MustCompile(`^\d+x\d+\+-?\d+\+-?\d+$`)

// rotateValue maps a rotation in degrees to the xrandr --rotate keyword.
// 0=normal, 90=right (clockwise), 180=inverted, 270=left (counter-clockwise).
func rotateValue(deg int) string {
	switch deg {
	case 0:
		return "normal"
	case 90:
		return "right"
	case 180:
		return "inverted"
	case 270:
		return "left"
	}
	return ""
}

// setOutput powers the connected X output off (disable the CRTC → the HDMI
// signal stops → the display enters power-save) or back on (--auto = preferred
// mode). This is the sleep lever on a stack WITHOUT the DPMS extension (disp's
// Xorg lacks it), and works on any HDMI display regardless of CEC. After --auto
// the caller re-applies any rotation (--auto resets it).
func setOutput(c *Config, on bool) error {
	out, err := c.xrandr(8 * time.Second)
	if err != nil {
		return fmt.Errorf("xrandr query: %w", err)
	}
	o, ok := parseXrandr(out)
	if !ok {
		return fmt.Errorf("no connected output to power")
	}
	arg := "--off"
	if on {
		arg = "--auto"
	}
	if _, err := c.xrandr(10*time.Second, "--output", o.name, arg); err != nil {
		return fmt.Errorf("xrandr %s: %w", arg, err)
	}
	return nil
}

// applyRotation rotates the connected X output via xrandr (X11 only — the
// Wayland path would need wlr-randr against labwc; the caller gates on backend).
func applyRotation(c *Config, deg int) error {
	val := rotateValue(deg)
	if val == "" {
		return fmt.Errorf("rotation must be 0, 90, 180, or 270")
	}
	out, err := c.xrandr(8 * time.Second)
	if err != nil {
		return fmt.Errorf("xrandr query: %w", err)
	}
	o, ok := parseXrandr(out)
	if !ok {
		return fmt.Errorf("no connected output to rotate")
	}
	if _, err := c.xrandr(8*time.Second, "--output", o.name, "--rotate", val); err != nil {
		return fmt.Errorf("xrandr rotate: %w", err)
	}
	return nil
}

// xOutput is the parsed state of one xrandr output: its name and the modes
// marked current (*) and preferred (+).
type xOutput struct{ name, current, preferred string }

// xrandr runs xrandr against the seat user's X session (DISPLAY/XAUTHORITY),
// dropping to the seat uid when the agent is root — same identity the display
// children run under, so the X cookie is readable.
func (c *Config) xrandr(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xrandr", args...)
	cmd.Env = []string{
		"DISPLAY=" + c.Display,
		"XAUTHORITY=" + c.XAuthority,
		"HOME=" + c.Home,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if c.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: c.cred}
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// parseXrandr extracts the first connected output and its current/preferred
// modes from `xrandr` (no-arg) output. The header line looks like
// "HDMI-1 connected primary 1920x1080+0+0 ..."; mode lines are indented like
// "   1920x1080     60.00*+" where * = current and + = preferred.
func parseXrandr(s string) (xOutput, bool) {
	var o xOutput
	inTarget := false
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' { // output header
			f := strings.Fields(line)
			if o.name == "" && len(f) >= 2 && f[1] == "connected" {
				o.name = f[0]
				inTarget = true
			} else {
				inTarget = false // only the first connected output
			}
			continue
		}
		if !inTarget {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		mode := f[0]
		for _, tok := range f[1:] {
			if strings.Contains(tok, "*") {
				o.current = mode
			}
			if strings.Contains(tok, "+") {
				o.preferred = mode
			}
		}
	}
	return o, o.name != ""
}

// currentResolution returns the active mode of the connected X output (e.g.
// "1920x1080"), or "" if X/xrandr is unavailable.
func currentResolution(c *Config) string {
	if c.Display == "" {
		return ""
	}
	out, err := c.xrandr(6 * time.Second)
	if err != nil {
		return ""
	}
	o, _ := parseXrandr(out)
	return o.current
}

// EnsurePreferredMode forces the connected output to its EDID-preferred mode if
// it isn't already there. This is the boot-time robustness fix: a display that
// is slow to wake (or was unplugged) at boot leaves the vc4-KMS connector on a
// 1024x768 fallback that config.txt's legacy hdmi_mode does NOT override
// (node-disp-inventory.md §9). We re-assert the preferred mode a few times to
// cover a display that only provides EDID a few seconds after X starts.
func EnsurePreferredMode(c *Config) {
	if c.Display == "" {
		return
	}
	// A slow display may not have presented EDID when X first came up; retry.
	for attempt := 1; attempt <= 4; attempt++ {
		if applyPreferredMode(c) {
			return // already correct or successfully set
		}
		time.Sleep(8 * time.Second)
	}
}

// applyPreferredMode does one query+fix pass. Returns true when the output is
// (now) at its preferred mode or there is nothing to do; false to retry.
func applyPreferredMode(c *Config) bool {
	out, err := c.xrandr(8 * time.Second)
	if err != nil {
		log.Printf("[display] xrandr query failed: %v", err)
		return false
	}
	o, ok := parseXrandr(out)
	if !ok {
		log.Printf("[display] no connected output found; retrying")
		return false
	}
	if o.preferred == "" {
		// Connector offered no preferred mode — usually means no EDID yet
		// (display asleep/unplugged). Retry; don't force a guess.
		log.Printf("[display] %s has no preferred mode yet (EDID missing?); retrying", o.name)
		return false
	}
	if o.current == o.preferred {
		log.Printf("[display] %s already at preferred mode %s", o.name, o.preferred)
		return true
	}
	log.Printf("[display] %s at %q; forcing preferred %q", o.name, o.current, o.preferred)
	if _, err := c.xrandr(8*time.Second, "--output", o.name, "--mode", o.preferred); err != nil {
		log.Printf("[display] set preferred mode failed: %v", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// multi-output enumeration + layout (pure; the single-output helpers above stay
// the boot/back-compat path). Rendering of secondary-output content is deferred
// (see node-api.md); these provide enumeration + per-output addressing + layout.
// ---------------------------------------------------------------------------

// XOutput is one connected xrandr output: its name, whether it is the X primary,
// its current geometry ("WxH+X+Y", empty when --off), and its current/preferred
// modes.
type XOutput struct {
	Name      string
	Primary   bool
	Geometry  string
	Current   string
	Preferred string
}

// parseXrandrOutputs returns ALL connected outputs from `xrandr` (no-arg) output
// (disconnected outputs are skipped). The header line looks like
// "HDMI-1 connected primary 1920x1080+0+0 ..."; mode lines are indented.
func parseXrandrOutputs(s string) []XOutput {
	var outs []XOutput
	cur := -1
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' { // output header
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == "connected" {
				o := XOutput{Name: f[0]}
				for _, tok := range f[2:] {
					if tok == "primary" {
						o.Primary = true
					} else if o.Geometry == "" && geometryRe.MatchString(tok) {
						o.Geometry = tok
					}
				}
				outs = append(outs, o)
				cur = len(outs) - 1
			} else {
				cur = -1 // disconnected (or unknown) — ignore its mode lines
			}
			continue
		}
		if cur < 0 {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		mode := f[0]
		for _, tok := range f[1:] {
			if strings.Contains(tok, "*") {
				outs[cur].Current = mode
			}
			if strings.Contains(tok, "+") {
				outs[cur].Preferred = mode
			}
		}
	}
	return outs
}

// parseXrandrPrimary returns the name of the primary output: the one flagged
// primary, else the first connected, else "".
func parseXrandrPrimary(outs []XOutput) string {
	for _, o := range outs {
		if o.Primary {
			return o.Name
		}
	}
	if len(outs) > 0 {
		return outs[0].Name
	}
	return ""
}

// applyRotationOutput rotates a named output via xrandr (X11 only).
func applyRotationOutput(c *Config, output string, deg int) error {
	val := rotateValue(deg)
	if val == "" {
		return fmt.Errorf("rotation must be 0, 90, 180, or 270")
	}
	if output == "" {
		return applyRotation(c, deg) // resolve the primary the legacy way
	}
	if _, err := c.xrandr(8*time.Second, "--output", output, "--rotate", val); err != nil {
		return fmt.Errorf("xrandr rotate: %w", err)
	}
	return nil
}

// setOutputNamed powers a named output off/on via xrandr.
func setOutputNamed(c *Config, output string, on bool) error {
	if output == "" {
		return setOutput(c, on) // resolve the primary the legacy way
	}
	arg := "--off"
	if on {
		arg = "--auto"
	}
	if _, err := c.xrandr(10*time.Second, "--output", output, arg); err != nil {
		return fmt.Errorf("xrandr %s: %w", arg, err)
	}
	return nil
}

// xrandrLayoutArgs builds the ordered xrandr argv slices (one xrandr call per
// slice) for a multi-output layout. Pure — does NOT exec. Modes:
//   - mirror: every other output --same-as the primary.
//   - extend: the others chained --right-of the previous (primary first).
func xrandrLayoutArgs(mode, primary string, others []string) ([][]string, error) {
	if primary == "" {
		return nil, fmt.Errorf("layout needs a primary output")
	}
	var cmds [][]string
	cmds = append(cmds, []string{"--output", primary, "--auto", "--primary"})
	switch mode {
	case "mirror":
		for _, o := range others {
			cmds = append(cmds, []string{"--output", o, "--auto", "--same-as", primary})
		}
	case "extend":
		prev := primary
		for _, o := range others {
			cmds = append(cmds, []string{"--output", o, "--auto", "--right-of", prev})
			prev = o
		}
	default:
		return nil, fmt.Errorf("unknown layout %q (single|mirror|extend)", mode)
	}
	return cmds, nil
}

// ---------------------------------------------------------------------------
// Wayland (labwc) output power, via wlr-randr against the wlr-output-management
// protocol. This is the sleep lever under the Wayland primary, where xrandr
// can't reach the visible surface. wlr-randr runs as the seat user against
// labwc's wayland socket.
// ---------------------------------------------------------------------------

// firstWaylandSocket returns the name of the first wayland-* display socket in
// runtimeDir (e.g. "wayland-1"), or "" if none — labwc creates it on start.
// (The .lock companion file is skipped.)
func firstWaylandSocket(runtimeDir string) string {
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "wayland-") && !strings.HasSuffix(n, ".lock") {
			return n
		}
	}
	return ""
}

// wlrRandr runs wlr-randr against the labwc Wayland session (WAYLAND_DISPLAY +
// XDG_RUNTIME_DIR), dropping to the seat uid when the agent is root — the same
// identity labwc runs under, so the wayland socket is reachable.
func (c *Config) wlrRandr(timeout time.Duration, args ...string) (string, error) {
	sock := firstWaylandSocket(c.RuntimeDir)
	if sock == "" {
		return "", fmt.Errorf("no wayland socket in %s (is labwc running?)", c.RuntimeDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wlr-randr", args...)
	cmd.Env = []string{
		"XDG_RUNTIME_DIR=" + c.RuntimeDir,
		"WAYLAND_DISPLAY=" + sock,
		"HOME=" + c.Home,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if c.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: c.cred}
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// parseWlrOutputs returns the output names from `wlr-randr` output. Each output
// is a header line at column 0 whose first token is the name ("HDMI-A-1 ..."),
// followed by indented detail lines.
func parseWlrOutputs(s string) []string {
	var outs []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 {
			outs = append(outs, f[0])
		}
	}
	return outs
}

// setWaylandOutput powers every Wayland output off or on via wlr-randr. Whole-
// screen (Wayland multi-output addressing isn't wired); on=false → --off enters
// the display's power-save, on=true → --on re-enables it.
func setWaylandOutput(c *Config, on bool) error {
	out, err := c.wlrRandr(8 * time.Second)
	if err != nil {
		return fmt.Errorf("wlr-randr query: %w", err)
	}
	names := parseWlrOutputs(out)
	if len(names) == 0 {
		return fmt.Errorf("no wayland outputs found")
	}
	arg := "--off"
	if on {
		arg = "--on"
	}
	for _, n := range names {
		if _, err := c.wlrRandr(10*time.Second, "--output", n, arg); err != nil {
			return fmt.Errorf("wlr-randr %s %s: %w", n, arg, err)
		}
	}
	return nil
}

// geometryArgs parses an xrandr geometry "WxH+X+Y" into its components. Pure;
// used by the (deferred) secondary-output content launchers to position a
// windowed child on the right output.
func geometryArgs(geometry string) (w, h, x, y int, ok bool) {
	if !geometryRe.MatchString(geometry) {
		return 0, 0, 0, 0, false
	}
	// Split "WxH+X+Y" on the first '+': head = "WxH", tail = "X+Y".
	plus := strings.IndexByte(geometry, '+')
	wh := strings.SplitN(geometry[:plus], "x", 2)
	xy := strings.SplitN(geometry[plus+1:], "+", 2)
	if len(wh) != 2 || len(xy) != 2 {
		return 0, 0, 0, 0, false
	}
	wi, e1 := strconv.Atoi(wh[0])
	hi, e2 := strconv.Atoi(wh[1])
	xi, e3 := strconv.Atoi(xy[0])
	yi, e4 := strconv.Atoi(xy[1])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return 0, 0, 0, 0, false
	}
	return wi, hi, xi, yi, true
}
