package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DisplayInfo is the user-controllable display state shown in /api/stats and the
// webUI. The top-level Rotation/ZoomPercent/Asleep describe the PRIMARY output
// (back-compat); Outputs + Layout are the additive multi-display view.
type DisplayInfo struct {
	Rotation    int          `json:"rotation"`          // primary: 0 / 90 / 180 / 270 degrees
	ZoomPercent int          `json:"zoom_percent"`      // 100 = no zoom (global)
	Asleep      bool         `json:"asleep"`            // the primary output is powered off (sleeping)
	Layout      string       `json:"layout,omitempty"`  // single | mirror | extend
	Outputs     []OutputInfo `json:"outputs,omitempty"` // per-output view (multi-display)
}

// OutputInfo is one display output's state for /api/outputs and DisplayInfo.
type OutputInfo struct {
	Name     string        `json:"name"`
	Primary  bool          `json:"primary"`
	Rotation int           `json:"rotation"`
	Asleep   bool          `json:"asleep"`
	Geometry string        `json:"geometry,omitempty"`
	Content  OutputContent `json:"content"`
	// Rendered is true when this output's assigned Content is actually being
	// shown. Only the primary renders today; a secondary output's content is
	// persisted + reported but not yet rendered (rendering deferred). The flag
	// keeps the API honest for the future central panel.
	Rendered bool `json:"rendered"`
}

// Display owns the rotation + kiosk-zoom + layout state, persists it across
// restarts, and applies it: rotation/layout/sleep via xrandr (X11 only), zoom via
// the CDP chrome controller. The persisted state is re-applied at boot so a
// rotated/zoomed kiosk survives a reboot or agent restart.
//
// Multi-output: per-output rotation/sleep are keyed by output name; the legacy
// single-output behaviour is preserved by resolving output=="" to the primary.
// A sentinel "" key in the rotations map means "the primary, resolved lazily"
// (so an old single-output state migrates without knowing the output name yet).
type Display struct {
	cfg  *Config
	sup  *Supervisor
	cec  *CEC
	path string

	screenMu sync.Mutex // serializes Screen() so scheduler + manual don't interleave xrandr

	mu           sync.Mutex
	rotations    map[string]int  // output name → degrees ("" sentinel = primary)
	asleeps      map[string]bool // output name → powered off (not persisted)
	zoom         float64         // 1.0 = 100% (global)
	primary      string          // cached primary output name ("" until enumerated)
	layout string // single | mirror | extend
	// Legacy nightly sleep/wake window — read from display.json for a one-time
	// migration into the unified weekly Scheduler (see legacyNightly / MigrateNightly);
	// no longer enforced or re-persisted here.
	schedEnabled bool
	schedSleep   string // "HH:MM" node-local
	schedWake    string // "HH:MM" node-local

	content *Content // cross-link for per-output content in Info()/Outputs()

	// testOutputs, when non-nil, replaces live xrandr enumeration (hermetic tests
	// have no X server). Used only via setOutputsForTest.
	testOutputs []XOutput
}

type persistedDisplay struct {
	Rotation     int            `json:"rotation"` // legacy: primary's rotation (kept for downgrade)
	Rotations    map[string]int `json:"rotations,omitempty"`
	Layout       string         `json:"layout,omitempty"`
	Zoom float64 `json:"zoom"`
	// Legacy nightly window: read for migration, no longer written (omitempty drops
	// the keys from display.json once the file is next rewritten).
	SchedEnabled bool   `json:"sched_enabled,omitempty"`
	SchedSleep   string `json:"sched_sleep,omitempty"`
	SchedWake    string `json:"sched_wake,omitempty"`
}

func NewDisplay(cfg *Config, sup *Supervisor, cec *CEC) *Display {
	d := &Display{
		cfg: cfg, sup: sup, cec: cec, path: cfg.StateFile,
		rotations: map[string]int{}, asleeps: map[string]bool{},
		zoom: 1.0, layout: "single",
	}
	d.load()
	return d
}

// SetContent wires the content manager back-link (post-construction) so Info()
// and Outputs() can fold in per-output content.
func (d *Display) SetContent(c *Content) { d.content = c }

func (d *Display) load() {
	if d.path == "" {
		return
	}
	b, err := os.ReadFile(d.path)
	if err != nil {
		return // no state yet → defaults
	}
	var p persistedDisplay
	if json.Unmarshal(b, &p) != nil {
		return
	}
	// Rotations migration: prefer the new map; else seed from the legacy scalar
	// under the "" sentinel (resolved onto the real primary at enumeration).
	if len(p.Rotations) > 0 {
		for name, deg := range p.Rotations {
			if rotateValue(deg) != "" {
				d.rotations[name] = deg
			}
		}
	} else if rotateValue(p.Rotation) != "" && p.Rotation != 0 {
		d.rotations[""] = p.Rotation
	}
	if p.Layout == "mirror" || p.Layout == "extend" {
		d.layout = p.Layout
	}
	if p.Zoom > 0 {
		d.zoom = p.Zoom
	}
	if _, ok := parseHHMM(p.SchedSleep); ok {
		d.schedSleep = p.SchedSleep
	}
	if _, ok := parseHHMM(p.SchedWake); ok {
		d.schedWake = p.SchedWake
	}
	d.schedEnabled = p.SchedEnabled && d.schedSleep != "" && d.schedWake != ""
}

func (d *Display) save() {
	if d.path == "" {
		return
	}
	d.mu.Lock()
	rotations := make(map[string]int, len(d.rotations))
	for k, v := range d.rotations {
		rotations[k] = v
	}
	p := persistedDisplay{
		Rotation:  d.primaryRotationLocked(), // legacy scalar = the primary's rotation
		Rotations: rotations,
		Layout:    d.layout,
		Zoom:      d.zoom,
	}
	d.mu.Unlock()
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		log.Printf("[display] state dir: %v", err)
		return
	}
	if err := os.WriteFile(d.path, b, 0o644); err != nil {
		log.Printf("[display] state save: %v", err)
	}
}

// primaryRotationLocked returns the rotation of the primary output (caller holds
// d.mu): the cached primary's value, else the "" sentinel, else 0.
func (d *Display) primaryRotationLocked() int {
	if d.primary != "" {
		if r, ok := d.rotations[d.primary]; ok {
			return r
		}
	}
	return d.rotations[""]
}

// PrimaryName returns the cached primary output name, refreshing from xrandr if
// it isn't known yet. "" when no output can be enumerated (host/Wayland).
func (d *Display) PrimaryName() string {
	d.mu.Lock()
	p := d.primary
	d.mu.Unlock()
	if p != "" {
		return p
	}
	d.refreshOutputs()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.primary
}

// enumerate returns the connected outputs — from the test seam if set, else live
// xrandr. Best-effort: returns nil under Wayland or when xrandr is unavailable.
func (d *Display) enumerate() []XOutput {
	d.mu.Lock()
	test := d.testOutputs
	d.mu.Unlock()
	if test != nil {
		return test
	}
	if d.sup != nil && d.sup.OnWaylandPrimary() {
		return nil
	}
	out, err := d.cfg.xrandr(8 * time.Second)
	if err != nil {
		return nil
	}
	return parseXrandrOutputs(out)
}

// refreshOutputs caches the primary name and resolves the "" legacy rotation key
// onto the real primary. Best-effort; skips when nothing can be enumerated.
func (d *Display) refreshOutputs() {
	outs := d.enumerate()
	if len(outs) == 0 {
		return
	}
	primary := parseXrandrPrimary(outs)
	d.mu.Lock()
	d.primary = primary
	if primary != "" {
		if leg, ok := d.rotations[""]; ok {
			if _, exists := d.rotations[primary]; !exists {
				d.rotations[primary] = leg
			}
			delete(d.rotations, "")
		}
	}
	d.mu.Unlock()
}

// SeedZoom hands the persisted zoom to the chrome controller so the FIRST kiosk
// navigation already applies it. Call before the initial mode switch.
func (d *Display) SeedZoom() {
	d.mu.Lock()
	zoom := d.zoom
	d.mu.Unlock()
	d.sup.chrome.SetZoomDefault(zoom)
}

// ApplyRotationAtBoot re-applies the persisted per-output rotations via xrandr.
// Best-effort; call after X is up and the preferred-mode fix has run.
func (d *Display) ApplyRotationAtBoot() {
	d.refreshOutputs()
	d.mu.Lock()
	rots := make(map[string]int, len(d.rotations))
	for k, v := range d.rotations {
		rots[k] = v
	}
	primary := d.primary
	d.mu.Unlock()

	for name, rot := range rots {
		if rot == 0 {
			continue
		}
		target := name
		if target == "" {
			target = primary // the "" sentinel resolves to the primary
		}
		if err := applyRotationOutput(d.cfg, target, rot); err != nil {
			log.Printf("[display] boot rotation %s %d° failed: %v", target, rot, err)
			continue
		}
		log.Printf("[display] applied persisted rotation %s %d°", target, rot)
	}
}

// ApplyLayoutAtBoot re-applies a persisted multi-output layout (mirror/extend)
// via xrandr so a node comes back the way it was left rather than into X's
// default arrangement. Best-effort; SetLayout also re-applies per-output
// rotations, so call this BEFORE ApplyRotationAtBoot. A "single"/empty layout
// needs no action (and would wrongly blank a now-single-output node).
func (d *Display) ApplyLayoutAtBoot() {
	d.mu.Lock()
	layout := d.layout
	d.mu.Unlock()
	if layout != "mirror" && layout != "extend" {
		return
	}
	if err := d.SetLayout(layout, ""); err != nil {
		log.Printf("[display] boot layout %s skipped: %v", layout, err)
		return
	}
	log.Printf("[display] applied persisted layout %s", layout)
}

// resolveOutput maps an output request to a concrete output name: "" → primary.
func (d *Display) resolveOutput(output string) string {
	if output != "" {
		return output
	}
	return d.PrimaryName()
}

// Rotate sets a display output's rotation (0/90/180/270) via xrandr and persists
// it. output=="" targets the primary (back-compat). X11 only — 409 under Wayland.
func (d *Display) Rotate(output string, deg int) error {
	if rotateValue(deg) == "" {
		return &apiError{code: 400, err: fmt.Errorf("rotation must be 0, 90, 180, or 270")}
	}
	if d.sup.OnWaylandPrimary() {
		return &apiError{code: 409, err: fmt.Errorf("rotation is X11-only for now; not supported under the Wayland kiosk")}
	}
	if d.sup.Foreground() {
		return &apiError{code: 409, err: fmt.Errorf("a console/kms foreground mode is on screen; rotation applies to the compositor surface")}
	}
	target := d.resolveOutput(output)
	if err := applyRotationOutput(d.cfg, target, deg); err != nil {
		return &apiError{code: 500, err: err}
	}
	d.mu.Lock()
	d.rotations[target] = deg // target=="" keeps the legacy primary sentinel
	d.mu.Unlock()
	d.save()
	return nil
}

// Zoom sets the kiosk page-zoom factor (1.25 = 125%) and persists it. Global.
func (d *Display) Zoom(factor float64) error {
	if factor < 0.25 || factor > 5.0 {
		return &apiError{code: 400, err: fmt.Errorf("zoom factor must be between 0.25 and 5.0")}
	}
	if err := d.sup.SetZoom(factor); err != nil {
		return err // already an *apiError
	}
	d.mu.Lock()
	d.zoom = factor
	d.mu.Unlock()
	d.save()
	return nil
}

// Screen sleeps or wakes a display output. Under X it disables/enables the
// output via xrandr (the reliable lever where DPMS is absent); under the Wayland
// primary it uses wlr-randr against labwc instead (xrandr can't reach that
// surface). It ALSO sends CEC standby/on for a CEC-capable TV — but CEC only
// when the target is the primary (CEC is TV/bus level, not per-output). On wake
// it re-applies the output's persisted rotation (X only; --auto resets it).
// output=="" targets the primary (back-compat); under Wayland the lever is
// whole-screen so the target is ignored for the output toggle.
func (d *Display) Screen(output string, on bool) error {
	d.screenMu.Lock() // serialize off/auto so scheduler + manual calls don't interleave
	defer d.screenMu.Unlock()
	wayland := d.sup.OnWaylandPrimary()
	target := d.resolveOutput(output)
	isPrimary := target == "" || target == d.PrimaryName()
	cecOK := isPrimary && d.cec != nil && d.cec.supported
	d.mu.Lock()
	wasAsleep := d.asleeps[target]
	d.mu.Unlock()

	// powerOutput drives the output power on the right backend; under Wayland a
	// failure is tolerated when a CEC TV can still carry the sleep/wake.
	powerOutput := func(want bool) error {
		if wayland {
			if err := setWaylandOutput(d.cfg, want); err != nil {
				if cecOK {
					log.Printf("[display] wlr-randr power %v failed, relying on CEC: %v", want, err)
					return nil
				}
				return &apiError{code: 500, err: fmt.Errorf("display sleep under Wayland needs wlr-randr (or a CEC TV): %w", err)}
			}
			return nil
		}
		if err := setOutputNamed(d.cfg, target, want); err != nil {
			return &apiError{code: 500, err: err}
		}
		return nil
	}

	if on {
		if err := powerOutput(true); err != nil {
			return err
		}
		if !wayland {
			d.mu.Lock()
			rot := d.rotations[target]
			if rot == 0 && (target == "" || target == d.primary) {
				rot = d.rotations[""]
			}
			d.mu.Unlock()
			if rot != 0 {
				_ = applyRotationOutput(d.cfg, target, rot) // --auto reset rotation; restore it
			}
		}
		if cecOK {
			_ = d.cec.On() // best-effort: wake a CEC TV + take active source
		}
	} else {
		if cecOK {
			_ = d.cec.Off() // best-effort: put a CEC TV into standby
		}
		if err := powerOutput(false); err != nil {
			return err
		}
	}
	d.mu.Lock()
	d.asleeps[target] = !on
	d.mu.Unlock()
	// On wake, a GTK app (the viewer) that could not paint to the slept display may
	// not repaint on its own — bounce the GUI surface so it re-renders. No-op for
	// the Chromium kiosk (renders through sleep) and non-GUI modes.
	if on && wasAsleep {
		go d.sup.restartGUISurface()
	}
	return nil
}

// SetLayout arranges the connected outputs: single (primary on, others off),
// mirror (others --same-as the primary), or extend (others chained right). 400
// on an unknown mode or fewer than 2 outputs for mirror/extend; 409 under
// Wayland or a foreground mode. primaryReq optionally names the primary output.
func (d *Display) SetLayout(mode, primaryReq string) error {
	if mode != "single" && mode != "mirror" && mode != "extend" {
		return &apiError{code: 400, err: fmt.Errorf("layout must be single, mirror, or extend")}
	}
	if d.sup.OnWaylandPrimary() {
		return &apiError{code: 409, err: fmt.Errorf("layout is X11-only for now; not supported under the Wayland kiosk")}
	}
	if d.sup.Foreground() {
		return &apiError{code: 409, err: fmt.Errorf("a console/kms foreground mode is on screen; layout applies to the compositor surface")}
	}
	outs := d.enumerate()
	if len(outs) == 0 {
		return &apiError{code: 503, err: fmt.Errorf("no connected outputs to arrange")}
	}
	if (mode == "mirror" || mode == "extend") && len(outs) < 2 {
		return &apiError{code: 400, err: fmt.Errorf("layout %s needs ≥2 connected outputs (%d connected)", mode, len(outs))}
	}
	primary := primaryReq
	if primary == "" {
		primary = parseXrandrPrimary(outs)
	}
	var others []string
	for _, o := range outs {
		if o.Name != primary {
			others = append(others, o.Name)
		}
	}

	var cmds [][]string
	if mode == "single" {
		cmds = append(cmds, []string{"--output", primary, "--auto", "--primary"})
		for _, o := range others {
			cmds = append(cmds, []string{"--output", o, "--off"})
		}
	} else {
		built, err := xrandrLayoutArgs(mode, primary, others)
		if err != nil {
			return &apiError{code: 400, err: err}
		}
		cmds = built
	}
	for _, args := range cmds {
		if _, err := d.cfg.xrandr(10*time.Second, args...); err != nil {
			return &apiError{code: 500, err: fmt.Errorf("xrandr layout %s: %w", mode, err)}
		}
	}
	// --auto resets rotation; re-apply persisted per-output rotations.
	d.mu.Lock()
	d.primary = primary
	d.layout = mode
	rots := make(map[string]int, len(d.rotations))
	for k, v := range d.rotations {
		rots[k] = v
	}
	d.mu.Unlock()
	for name, rot := range rots {
		if rot == 0 {
			continue
		}
		t := name
		if t == "" {
			t = primary
		}
		_ = applyRotationOutput(d.cfg, t, rot)
	}
	d.save()
	return nil
}

func (d *Display) Info() DisplayInfo {
	primary := d.PrimaryName()
	d.mu.Lock()
	defer d.mu.Unlock()
	info := DisplayInfo{
		Rotation:    d.primaryRotationLocked(),
		ZoomPercent: int(d.zoom*100 + 0.5),
		Asleep:      d.asleeps[primary] || d.asleeps[""],
		Layout:      d.layout,
	}
	info.Outputs = d.outputsLocked(primary)
	return info
}

// Outputs returns the per-output view for /api/outputs. Never 500s when xrandr is
// unavailable: it falls back to the persisted/primary view.
func (d *Display) Outputs() []OutputInfo {
	outs := d.enumerate()
	// Derive the primary from the list we just fetched rather than calling
	// PrimaryName(), which would fork xrandr a second time on a cold cache.
	primary := parseXrandrPrimary(outs)
	if primary == "" {
		primary = d.PrimaryName()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(outs) == 0 {
		return d.outputsLocked(primary)
	}
	res := make([]OutputInfo, 0, len(outs))
	for _, o := range outs {
		rot := d.rotations[o.Name]
		isPrimary := o.Primary || o.Name == primary
		if rot == 0 && isPrimary {
			rot = d.rotations[""]
		}
		res = append(res, OutputInfo{
			Name:     o.Name,
			Primary:  isPrimary,
			Rotation: rot,
			Asleep:   d.asleeps[o.Name],
			Geometry: o.Geometry,
			Content:  d.contentFor(o.Name),
			Rendered: isPrimary, // only the primary renders today
		})
	}
	return res
}

// outputsLocked builds a per-output view from persisted state alone (caller holds
// d.mu) — the fallback when live enumeration is unavailable.
func (d *Display) outputsLocked(primary string) []OutputInfo {
	if primary == "" {
		// No known output — e.g. xrandr is blind under the Wayland kiosk (wlr-randr
		// enumeration is the follow-up). Return an empty but non-nil slice so
		// /api/outputs always yields a JSON array, never null (a client mapping over
		// the body would otherwise trip). DisplayInfo.Outputs is omitempty, so this
		// stays omitted from /api/stats either way.
		return []OutputInfo{}
	}
	rot := d.rotations[primary]
	if rot == 0 {
		rot = d.rotations[""]
	}
	return []OutputInfo{{
		Name:     primary,
		Primary:  true,
		Rotation: rot,
		Asleep:   d.asleeps[primary] || d.asleeps[""],
		Content:  d.contentFor(primary),
		Rendered: true,
	}}
}

// contentFor folds in the per-output content (zero = off). No lock on d needed
// for the content manager (it has its own).
func (d *Display) contentFor(name string) OutputContent {
	if d.content == nil {
		return OutputContent{Type: "off"}
	}
	return d.content.For(name)
}

// setOutputsForTest injects a fake output list (hermetic tests have no X server).
func (d *Display) setOutputsForTest(outs []XOutput) {
	d.mu.Lock()
	d.testOutputs = outs
	d.primary = ""
	d.mu.Unlock()
}

// legacyNightly returns the nightly sleep/wake window loaded from display.json, for
// a one-time migration into the unified weekly Scheduler (see Scheduler.MigrateNightly).
// The window is no longer enforced or re-persisted by Display.
func (d *Display) legacyNightly() (enabled bool, sleep, wake string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.schedEnabled, d.schedSleep, d.schedWake
}

// Asleep reports whether the primary output is currently powered off. The scheduler
// uses it to make the @sleep/@wake transitions idempotent: a boot/restart/Set that
// re-resolves to a sleep/wake it has already achieved must NOT re-issue xrandr/CEC
// (which would yank a CEC TV's input back to this node on an already-awake screen).
// This restores the guard the retired nightly ticker had (desired == actual → skip).
func (d *Display) Asleep() bool {
	primary := d.PrimaryName()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asleeps[primary] || d.asleeps[""]
}

// parseHHMM parses a 24-hour "HH:MM" into minutes-since-midnight.
func parseHHMM(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || n != 2 {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}
