package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlymouthCmdlineToggle(t *testing.T) {
	dir := t.TempDir()
	cl := filepath.Join(dir, "cmdline.txt")
	if err := os.WriteFile(cl, []byte("console=tty1 root=/dev/mmcblk0p2 rootwait\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPlymouth(&Config{PlymouthCmdline: cl, PlymouthThemeDir: filepath.Join(dir, "theme"), PlymouthTheme: "sideshow"})

	if p.cmdlineHasSplash() {
		t.Fatal("fresh cmdline should not have splash")
	}
	if err := p.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	b, _ := os.ReadFile(cl)
	line := string(b)
	if !strings.Contains(line, "splash") || !strings.Contains(line, "quiet") {
		t.Fatalf("enable should add quiet+splash: %q", line)
	}
	// Original tokens preserved.
	if !strings.Contains(line, "root=/dev/mmcblk0p2") || !strings.Contains(line, "rootwait") {
		t.Fatalf("enable dropped original tokens: %q", line)
	}
	if !p.cmdlineHasSplash() {
		t.Fatal("cmdlineHasSplash should be true after enable")
	}
	// Idempotent: enabling again doesn't duplicate.
	_ = p.SetEnabled(true)
	b2, _ := os.ReadFile(cl)
	if strings.Count(string(b2), "splash") != 1 {
		t.Fatalf("splash duplicated: %q", string(b2))
	}

	if err := p.SetEnabled(false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	b3, _ := os.ReadFile(cl)
	if strings.Contains(string(b3), "splash") || strings.Contains(string(b3), "quiet") {
		t.Fatalf("disable should remove quiet+splash: %q", string(b3))
	}
}

func TestPlymouthSetEnabledNoCmdline(t *testing.T) {
	p := NewPlymouth(&Config{PlymouthCmdline: filepath.Join(t.TempDir(), "nope.txt")})
	err := p.SetEnabled(true)
	if err == nil {
		t.Fatal("missing cmdline should error")
	}
	if ae, ok := err.(*apiError); !ok || ae.code != 503 {
		t.Fatalf("want 503 apiError, got %v", err)
	}
}

func TestPlymouthScriptEscaping(t *testing.T) {
	s := plymouthScript(`evil"; Window.Foo()`)
	if strings.Contains(s, `evil"; Window.Foo()`) {
		t.Fatalf("message not escaped in script: %s", s)
	}
	if !strings.Contains(s, `evil\"`) {
		t.Fatalf("expected escaped quote in script: %s", s)
	}
}
