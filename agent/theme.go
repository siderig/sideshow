package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SetSystemColorScheme sets the desktop-wide dark/light preference so EVERY app
// that follows system settings renders in the chosen mode — GTK3/4, Qt, Chromium,
// and the WebKitGTK webview inside a custom app — not just the CDP-emulated kiosk.
// It writes the canonical sources as the seat user:
//
//   - gsettings org.gnome.desktop.interface color-scheme + gtk-theme. The
//     xdg-desktop-portal gtk backend serves this as org.freedesktop.appearance
//     `color-scheme`, the cross-toolkit "system dark mode" signal on BOTH X11 and
//     Wayland (Chromium, GTK4/libadwaita, Qt 6.5+, recent WebKitGTK follow it live).
//   - ~/.config/gtk-{3,4}.0/settings.ini — read at app startup; carries GTK3's
//     gtk-application-prefer-dark-theme, which WebKitGTK maps prefers-color-scheme
//     to. Covers newly-launched apps and toolkits that don't watch the portal.
//   - X11: an xsettingsd reload pushes the GTK setting live to already-running X
//     clients (legacy GTK3 apps that don't use the portal).
//
// Best-effort throughout: a missing tool/dir is logged, never fatal — the agent
// must not fail a theme toggle because one mechanism is absent on a given node.
func (s *Supervisor) SetSystemColorScheme(dark bool) {
	scheme, theme, prefer := "default", "Adwaita", "0"
	if dark {
		scheme, theme, prefer = "prefer-dark", "Adwaita-dark", "1"
	}

	// 1) gsettings/dconf — the source the xdg-desktop-portal gtk backend reads.
	if out, err := s.seatRun(8*time.Second, false, "gsettings", "set", "org.gnome.desktop.interface", "color-scheme", scheme); err != nil {
		log.Printf("[theme] gsettings color-scheme=%s: %v: %s", scheme, err, out)
	}
	if out, err := s.seatRun(8*time.Second, false, "gsettings", "set", "org.gnome.desktop.interface", "gtk-theme", theme); err != nil {
		log.Printf("[theme] gsettings gtk-theme=%s: %v: %s", theme, err, out)
	}

	// 2) gtk settings.ini (startup + GTK3 prefers-dark).
	ini := fmt.Sprintf("[Settings]\ngtk-application-prefer-dark-theme=%s\ngtk-theme-name=%s\n", prefer, theme)
	for _, d := range []string{"gtk-3.0", "gtk-4.0"} {
		dir := filepath.Join(s.cfg.Home, ".config", d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[theme] mkdir %s: %v", dir, err)
			continue
		}
		s.chownSeat(filepath.Dir(dir)) // ~/.config (may be freshly created)
		s.chownSeat(dir)
		p := filepath.Join(dir, "settings.ini")
		if err := os.WriteFile(p, []byte(ini), 0o644); err != nil {
			log.Printf("[theme] write %s: %v", p, err)
			continue
		}
		s.chownSeat(p)
	}

	// 3) Nudge the portal so it's up to serve color-scheme to running apps (it is
	// D-Bus activated, so this is belt-and-suspenders). Harmless if absent.
	s.seatRun(8*time.Second, false, "systemctl", "--user", "start", "xdg-desktop-portal")

	// 4) X11 live push to running GTK clients via xsettingsd.
	s.applyXSettings(theme, prefer)
}

// applyXSettings writes the xsettingsd config and SIGHUP-reloads a running daemon
// so already-open X11 GTK apps flip live (the X analogue of the Wayland portal
// push). It does not spawn xsettingsd — the X session owns its lifecycle; if none
// is running there is nothing to update and newly-launched apps read settings.ini.
func (s *Supervisor) applyXSettings(theme, prefer string) {
	dir := filepath.Join(s.cfg.Home, ".config", "xsettingsd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	s.chownSeat(dir)
	conf := fmt.Sprintf("Net/ThemeName \"%s\"\nGtk/ApplicationPreferDarkTheme %s\n", theme, prefer)
	p := filepath.Join(dir, "xsettingsd.conf")
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		return
	}
	s.chownSeat(p)
	s.seatRun(5*time.Second, true, "pkill", "-HUP", "-x", "xsettingsd") // no-op if not running
}

// restartGUISurface bounces the current GUI app so it re-reads a system change at
// startup — a color-scheme toggle OR a display wake. WebKitGTK applies
// prefers-color-scheme at page load (not live), and a GTK app cannot paint to a
// slept display and may not repaint when the display wakes; a restart makes either
// change take. Restricted to `app` — the ONLY surface that needs it. The Chromium
// kiosk is never restarted (it themes live via CDP and renders through sleep via
// GL); the cog/KMS kiosk likewise renders through sleep on DRM and is left as-is
// for color-scheme; media/airplay/console have no themable UI. The supervise loop
// respawns the killed child.
func (s *Supervisor) restartGUISurface() {
	s.mu.Lock()
	r := s.fgRunner
	if r == nil {
		r = s.runner
	}
	s.mu.Unlock()
	if r == nil {
		return
	}
	mt := r.modeType()
	if mt != ModeApp { // only a GUI app needs it — never the Chromium/cog kiosk or media
		return
	}
	if pid := r.currentPID(); pid > 0 {
		log.Printf("[gui] restarting %s mode to re-render after a system change", mt)
		killGroup(pid, syscall.SIGTERM) // supervise loop respawns it at the new setting
	}
}

// seatRun runs a command as the seat user with their session env: XDG_RUNTIME_DIR
// + the user D-Bus bus (so gsettings/dconf/systemctl --user hit the same bus the
// portal and the kiosk use), optionally the X env (DISPLAY/XAUTHORITY) for X11
// tools like xsettingsd/pkill. Drops to the seat uid when the agent is root.
func (s *Supervisor) seatRun(timeout time.Duration, withX bool, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = s.seatEnv(withX)
	if s.cfg.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: s.cfg.cred}
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *Supervisor) seatEnv(withX bool) []string {
	cfg := s.cfg
	env := []string{
		"HOME=" + cfg.Home,
		"USER=" + cfg.SeatUser,
		"LOGNAME=" + cfg.SeatUser,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"XDG_RUNTIME_DIR=" + cfg.RuntimeDir,
	}
	if bus := cfg.RuntimeDir + "/bus"; fileExists(bus) {
		env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
	}
	if withX {
		xauth := cfg.XAuthority
		if xauth == "" {
			xauth = cfg.Home + "/.Xauthority"
		}
		env = append(env, "DISPLAY="+cfg.Display, "XAUTHORITY="+xauth)
	}
	return env
}

// chownSeat hands a path to the seat user (best-effort) so files the root agent
// created are owned by the user whose session reads them.
func (s *Supervisor) chownSeat(path string) {
	if s.cfg.cred != nil {
		_ = os.Chown(path, int(s.cfg.cred.Uid), int(s.cfg.cred.Gid))
	}
}
