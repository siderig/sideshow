package main

import (
	"strings"
	"testing"
)

func normMode(t string, display string, params map[string]any) Mode {
	m := Mode{Type: t, Display: display, Params: params}
	m.normalize()
	return m
}

func TestAirplayValidateAndCommand(t *testing.T) {
	// Compositor airplay is now allowed (X11 client); other displays are rejected.
	mOK := normMode(ModeAirplay, DisplayCompositor, nil)
	if err := mOK.validate(); err != nil {
		t.Fatalf("airplay/compositor should validate, got %v", err)
	}
	mBad := normMode(ModeAirplay, DisplayConsole, nil)
	if err := mBad.validate(); err == nil {
		t.Fatal("airplay/console must be rejected")
	}

	cfg := &Config{Node: "lobby"}
	name, args, err := modeCommand(cfg, normMode(ModeAirplay, DisplayCompositor, nil))
	if err != nil {
		t.Fatalf("modeCommand airplay: %v", err)
	}
	if name != "uxplay" {
		t.Fatalf("airplay binary = %q, want uxplay", name)
	}
	got := strings.Join(args, " ")
	if strings.Contains(got, "kmssink") {
		t.Errorf("airplay must NOT use kmssink under X11: %q", got)
	}
	if !strings.Contains(got, "-n lobby") {
		t.Errorf("airplay should pass the node name via -n: %q", got)
	}
	if !strings.Contains(got, "-a") {
		t.Errorf("airplay should default to video-only (-a disables audio): %q", got)
	}

	// audio=true drops -a; explicit name wins over the node name.
	_, args2, _ := modeCommand(cfg, normMode(ModeAirplay, DisplayCompositor, map[string]any{"audio": true, "name": "Studio"}))
	g2 := strings.Join(args2, " ")
	if strings.Contains(g2, "-a") {
		t.Errorf("audio=true should not pass -a: %q", g2)
	}
	if !strings.Contains(g2, "-n Studio") {
		t.Errorf("explicit name should win: %q", g2)
	}
}

func TestAirplayKMSCommand(t *testing.T) {
	cfg := &Config{Node: "lobby"}
	m := normMode(ModeAirplay, DisplayKMS, nil)
	if err := m.validate(); err != nil {
		t.Fatalf("airplay/kms should validate, got %v", err)
	}
	_, args, err := modeCommand(cfg, m)
	if err != nil {
		t.Fatalf("modeCommand airplay/kms: %v", err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "-vs kmssink") {
		t.Errorf("airplay/kms must use -vs kmssink: %q", got)
	}
	// KMS children run as root for DRM master.
	if !m.runsAsRoot(cfg) {
		t.Error("airplay/kms must run as root (DRM master)")
	}
	if normMode(ModeAirplay, DisplayCompositor, nil).runsAsRoot(cfg) {
		t.Error("airplay/compositor must NOT run as root")
	}
}

func TestMediaKMSCommand(t *testing.T) {
	cfg := &Config{}
	m := normMode(ModeMedia, DisplayKMS, map[string]any{"url": "http://ex/v.mp4"})
	_, args, err := modeCommand(cfg, m)
	if err != nil {
		t.Fatalf("modeCommand media/kms: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "--vo=drm") {
		t.Errorf("media/kms must use --vo=drm: %q", strings.Join(args, " "))
	}
	if !m.runsAsRoot(cfg) {
		t.Error("media/kms must run as root")
	}
	// web/kms is now the cog framebuffer backend (see TestWebKMSMode), not rejected.
	wk := normMode(ModeWeb, DisplayKMS, map[string]any{"url": "http://x"})
	if err := wk.validate(); err != nil {
		t.Errorf("web/kms (cog) should validate now: %v", err)
	}
}

func TestMoonlightCommand(t *testing.T) {
	cfg := &Config{}
	// No host → the pairing GUI (no args).
	name, args, err := modeCommand(cfg, normMode(ModeMoonlight, DisplayCompositor, nil))
	if err != nil || name != "moonlight-qt" || len(args) != 0 {
		t.Fatalf("moonlight (no host) = %q %v err=%v, want moonlight-qt with no args", name, args, err)
	}
	// With host → stream host app (default app Desktop).
	_, args2, _ := modeCommand(cfg, normMode(ModeMoonlight, DisplayCompositor, map[string]any{"host": "10.0.0.5"}))
	if strings.Join(args2, " ") != "stream 10.0.0.5 Desktop" {
		t.Fatalf("moonlight stream args = %q", strings.Join(args2, " "))
	}
	_, args3, _ := modeCommand(cfg, normMode(ModeMoonlight, DisplayCompositor, map[string]any{"host": "h", "app": "Steam"}))
	if strings.Join(args3, " ") != "stream h Steam" {
		t.Fatalf("moonlight app override args = %q", strings.Join(args3, " "))
	}
}

func TestSteamlinkCommand(t *testing.T) {
	// Default launcher.
	name, args, err := modeCommand(&Config{SteamlinkCmd: "steamlink"}, normMode(ModeSteamlink, DisplayCompositor, nil))
	if err != nil || name != "steamlink" || len(args) != 0 {
		t.Fatalf("steamlink default = %q %v err=%v", name, args, err)
	}
	// Flatpak launcher splits into argv.
	name2, args2, _ := modeCommand(&Config{SteamlinkCmd: "flatpak run com.valvesoftware.SteamLink"}, normMode(ModeSteamlink, DisplayCompositor, nil))
	if name2 != "flatpak" || strings.Join(args2, " ") != "run com.valvesoftware.SteamLink" {
		t.Fatalf("steamlink flatpak split = %q %v", name2, args2)
	}
	// KMS rejected (X11 client only).
	sk := normMode(ModeSteamlink, DisplayKMS, nil)
	if err := sk.validate(); err == nil {
		t.Error("steamlink/kms must be rejected")
	}
}

func TestMiracastGated(t *testing.T) {
	// Disabled by default → modeCommand errors (surfaces as 400 via preflight).
	if _, _, err := modeCommand(&Config{}, normMode(ModeMiracast, DisplayCompositor, nil)); err == nil {
		t.Fatal("miracast must error when -allow-miracast is off")
	}
	// Enabled → the configured launcher is split into argv.
	name, args, err := modeCommand(&Config{AllowMiracast: true, MiracastCmd: "gnome-network-displays --foo"}, normMode(ModeMiracast, DisplayCompositor, nil))
	if err != nil {
		t.Fatalf("miracast enabled: %v", err)
	}
	if name != "gnome-network-displays" || strings.Join(args, " ") != "--foo" {
		t.Fatalf("miracast cmd split = %q %v", name, args)
	}
}
