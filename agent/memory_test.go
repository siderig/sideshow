package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCgMB(t *testing.T) {
	dir := t.TempDir()
	write := func(name, val string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(val), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if mb, ok := readCgMB(write("max", "max\n")); !ok || mb != 0 {
		t.Errorf(`"max" → (%d,%v), want (0,true)`, mb, ok)
	}
	if mb, ok := readCgMB(write("val", "536870912\n")); !ok || mb != 512 {
		t.Errorf("512MiB → (%d,%v), want (512,true)", mb, ok)
	}
	if mb, ok := readCgMB(filepath.Join(dir, "missing")); ok || mb != 0 {
		t.Errorf("missing file → (%d,%v), want (0,false)", mb, ok)
	}
}

func TestMbOrInfinity(t *testing.T) {
	if mbOrInfinity(0) != "infinity" || mbOrInfinity(-1) != "infinity" {
		t.Error("0 / <0 must be infinity")
	}
	if got := mbOrInfinity(560); got != "560M" {
		t.Errorf("560 → %q, want 560M", got)
	}
}

// TestMemoryApplyValidation checks the guards that fire BEFORE any cgroup write,
// so a webUI fat-finger can't pick a kiosk-killing cap (the happy path needs
// root + systemd and is exercised live, not in unit tests).
func TestMemoryApplyValidation(t *testing.T) {
	m := NewMemory(&Config{})
	if err := m.Apply(0, memFloorMB-1, 0); err == nil {
		t.Error("max below floor must be rejected")
	} else if ae, ok := err.(*apiError); !ok || ae.code != 400 {
		t.Errorf("want 400 apiError, got %v", err)
	}
	if err := m.Apply(-1, 256, 0); err == nil {
		t.Error("negative high must be rejected")
	}
}
