package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const xrandrTwoHeads = `Screen 0: minimum 320 x 200, current 3840 x 1080, maximum 16384 x 16384
HDMI-1 connected primary 1920x1080+0+0 (normal left inverted right x axis y axis) 520mm x 290mm
   1920x1080     60.00*+  50.00    59.94
   1280x720      60.00    50.00
DP-1 connected 1920x1080+1920+0 (normal left inverted right x axis y axis) 510mm x 290mm
   1920x1080     59.95*+
   1680x1050     59.88
VGA-1 disconnected (normal left inverted right x axis y axis)
`

func TestParseXrandrOutputs(t *testing.T) {
	outs := parseXrandrOutputs(xrandrTwoHeads)
	if len(outs) != 2 {
		t.Fatalf("got %d outputs, want 2 (disconnected VGA-1 must be skipped)", len(outs))
	}
	if outs[0].Name != "HDMI-1" || !outs[0].Primary {
		t.Errorf("output[0] = %+v, want HDMI-1 primary", outs[0])
	}
	if outs[0].Geometry != "1920x1080+0+0" {
		t.Errorf("HDMI-1 geometry = %q", outs[0].Geometry)
	}
	if outs[0].Current != "1920x1080" || outs[0].Preferred != "1920x1080" {
		t.Errorf("HDMI-1 modes current=%q preferred=%q", outs[0].Current, outs[0].Preferred)
	}
	if outs[1].Name != "DP-1" || outs[1].Primary {
		t.Errorf("output[1] = %+v, want DP-1 non-primary", outs[1])
	}
	if outs[1].Geometry != "1920x1080+1920+0" {
		t.Errorf("DP-1 geometry = %q", outs[1].Geometry)
	}
	if p := parseXrandrPrimary(outs); p != "HDMI-1" {
		t.Errorf("parseXrandrPrimary = %q, want HDMI-1", p)
	}

	// Legacy parseXrandr still returns only the first connected output.
	o, ok := parseXrandr(xrandrTwoHeads)
	if !ok || o.name != "HDMI-1" {
		t.Errorf("legacy parseXrandr = (%+v,%v), want HDMI-1", o, ok)
	}
}

func TestXrandrLayoutArgs(t *testing.T) {
	mirror, err := xrandrLayoutArgs("mirror", "HDMI-1", []string{"DP-1"})
	if err != nil {
		t.Fatal(err)
	}
	wantMirror := [][]string{
		{"--output", "HDMI-1", "--auto", "--primary"},
		{"--output", "DP-1", "--auto", "--same-as", "HDMI-1"},
	}
	if !reflect.DeepEqual(mirror, wantMirror) {
		t.Errorf("mirror args = %v, want %v", mirror, wantMirror)
	}

	extend, err := xrandrLayoutArgs("extend", "HDMI-1", []string{"DP-1", "DP-2"})
	if err != nil {
		t.Fatal(err)
	}
	wantExtend := [][]string{
		{"--output", "HDMI-1", "--auto", "--primary"},
		{"--output", "DP-1", "--auto", "--right-of", "HDMI-1"},
		{"--output", "DP-2", "--auto", "--right-of", "DP-1"},
	}
	if !reflect.DeepEqual(extend, wantExtend) {
		t.Errorf("extend chain args = %v, want %v", extend, wantExtend)
	}

	if _, err := xrandrLayoutArgs("bogus", "HDMI-1", nil); err == nil {
		t.Error("unknown layout mode should error")
	}
	if _, err := xrandrLayoutArgs("mirror", "", []string{"DP-1"}); err == nil {
		t.Error("empty primary should error")
	}
}

func TestGeometryArgs(t *testing.T) {
	w, h, x, y, ok := geometryArgs("1920x1080+1920+0")
	if !ok || w != 1920 || h != 1080 || x != 1920 || y != 0 {
		t.Errorf("geometryArgs = (%d,%d,%d,%d,%v)", w, h, x, y, ok)
	}
	if _, _, _, _, ok := geometryArgs("garbage"); ok {
		t.Error("garbage geometry should not parse")
	}
	// Negative offset (valid on an extend layout to the left).
	_, _, nx, _, ok := geometryArgs("1280x720+-1280+0")
	if !ok || nx != -1280 {
		t.Errorf("negative offset = (x=%d,ok=%v)", nx, ok)
	}
}

func newTestDisplay(t *testing.T) *Display {
	t.Helper()
	dir := t.TempDir()
	return NewDisplay(&Config{StateFile: filepath.Join(dir, "display.json")}, testSupervisor(t), nil)
}

func TestLayoutValidation(t *testing.T) {
	d := newTestDisplay(t)

	if err := d.SetLayout("bogus", ""); err == nil {
		t.Error("unknown layout should 400")
	} else if ae, ok := err.(*apiError); !ok || ae.status() != 400 {
		t.Errorf("unknown layout err = %v, want 400", err)
	}

	// One output via the test seam: mirror/extend need >=2.
	d.setOutputsForTest([]XOutput{{Name: "HDMI-1", Primary: true, Geometry: "1920x1080+0+0"}})
	err := d.SetLayout("mirror", "")
	if err == nil {
		t.Fatal("mirror with one output should 400")
	}
	ae, ok := err.(*apiError)
	if !ok || ae.status() != 400 || !strings.Contains(err.Error(), "≥2") {
		t.Errorf("mirror<2 err = %v, want 400 'needs ≥2'", err)
	}
}

// Asleep reports the primary output's power state; the scheduler uses it to keep
// @sleep/@wake idempotent (skip re-issuing xrandr/CEC when already in the desired
// state), which is what stops a daytime restart from yanking a CEC TV's input.
func TestDisplayAsleep(t *testing.T) {
	d := newTestDisplay(t)
	d.setOutputsForTest([]XOutput{{Name: "HDMI-1", Primary: true, Geometry: "1920x1080+0+0"}})
	if d.Asleep() {
		t.Error("a fresh display should report awake (asleeps is empty, not persisted)")
	}
	d.mu.Lock()
	d.asleeps["HDMI-1"] = true
	d.mu.Unlock()
	if !d.Asleep() {
		t.Error("primary marked asleep should report Asleep()=true")
	}
}

// When no output can be enumerated (xrandr blind under the Wayland kiosk, or a
// host with no X), /api/outputs must still yield a JSON array — never null, which
// a client mapping over the body would trip on.
func TestOutputsEmptyIsNonNilArray(t *testing.T) {
	d := newTestDisplay(t)
	d.setOutputsForTest([]XOutput{}) // no outputs, no primary — the Wayland case

	got := d.Outputs()
	if got == nil {
		t.Fatal("Outputs() returned nil; /api/outputs would marshal to null")
	}
	if len(got) != 0 {
		t.Errorf("Outputs() = %+v, want empty slice", got)
	}
	if b, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	} else if string(b) != "[]" {
		t.Errorf("json(Outputs()) = %s, want []", b)
	}

	// DisplayInfo.Outputs is omitempty, so /api/stats stays unchanged (field omitted).
	if info := d.Info(); len(info.Outputs) != 0 {
		t.Errorf("Info().Outputs = %+v, want empty (omitted from /api/stats)", info.Outputs)
	}
}

func TestPersistedDisplayMigrate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "display.json")
	// Write a legacy state file with only the scalar rotation.
	if err := os.WriteFile(path, []byte(`{"rotation":90,"zoom":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewDisplay(&Config{StateFile: path}, testSupervisor(t), nil)
	// With no live xrandr, the legacy "" sentinel surfaces as the primary rotation.
	if got := d.Info().Rotation; got != 90 {
		t.Errorf("migrated Info().Rotation = %d, want 90", got)
	}

	// A round-trip save writes both legacy `rotation` and the new `rotations`.
	d.save()
	rawb, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(rawb)
	if !strings.Contains(raw, `"rotation": 90`) {
		t.Errorf("saved state missing legacy rotation: %s", raw)
	}
	if !strings.Contains(raw, `"rotations"`) {
		t.Errorf("saved state missing rotations map: %s", raw)
	}
}
