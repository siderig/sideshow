package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// XServer is the agent-owned X11 base: the agent starts and supervises Xorg (as
// root — for DRM master + input) plus a minimal window manager (matchbox), instead
// of leaning on a display manager (lightdm). It's the X11 analogue of the labwc
// Wayland base the agent already launches on Wayland nodes, and it makes the one
// supervisor the sole owner of the screen (cleaner VT/DRM-master handoff with the
// direct-KMS modes). Long-lived: the per-mode X clients (Chromium, an app) come
// and go as clients of this server; matchbox just fullscreens + focuses whichever
// is up (no decorations — every mode is a single fullscreen surface).
type XServer struct {
	cfg    *Config
	ctx    context.Context
	cancel context.CancelFunc
}

func NewXServer(cfg *Config) *XServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &XServer{cfg: cfg, ctx: ctx, cancel: cancel}
}

// Start writes the auth cookie, launches Xorg (root) + the WM (seat user), and
// blocks until X accepts connections so the first mode can attach. Both processes
// are then supervised (restart-on-exit) until Stop.
func (x *XServer) Start() error {
	writeNoDPMSConf() // disable X's own blank/DPMS timers before Xorg reads its config
	if err := x.writeAuth(); err != nil {
		return fmt.Errorf("xauth: %w", err)
	}
	go x.supervise("Xorg", x.buildXorg)
	if err := x.waitReady(25 * time.Second); err != nil {
		return err
	}
	log.Printf("[xserver] %s ready", x.cfg.Display)
	go x.supervise("wm", x.buildWM)
	if strings.TrimSpace(x.cfg.CursorHideCmd) != "" {
		go x.supervise("cursor", x.buildCursorHide)
	}
	return nil
}

// Stop tears down the WM + X server (ctx cancel kills the supervised children).
func (x *XServer) Stop() { x.cancel() }

// writeAuth generates a fresh MIT-MAGIC-COOKIE-1 for the display and registers it
// in the seat user's Xauthority — the same file Xorg reads (-auth) and the clients
// use (XAUTHORITY). Owned by the seat user so the priv-dropped clients can read it;
// root Xorg reads it regardless.
func (x *XServer) writeAuth() error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	_ = os.Remove(x.cfg.XAuthority)
	cmd := exec.Command("xauth", "-f", x.cfg.XAuthority, "add", x.cfg.Display, ".", hex.EncodeToString(b))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	if x.cfg.cred != nil {
		_ = os.Chown(x.cfg.XAuthority, int(x.cfg.cred.Uid), int(x.cfg.cred.Gid))
	}
	return nil
}

// nodpmsXorgConf disables the X server's screen-saver blank and DPMS power timers
// so an idle kiosk never auto-blanks. The agent owns Xorg (-start-x) and manages
// sleep EXPLICITLY via `xrandr --off` (see setOutput); but a modern Xorg — e.g.
// Debian trixie's modeset driver on the Pi — ships the DPMS extension ENABLED
// with a ~10-minute default, which would otherwise blank the display with no
// schedule or command (disp's older Xorg lacks the extension, so this never bit
// there). Zeroing all four timers is the config-level, tool-free equivalent of
// `xset s off -dpms` — it needs no x11-xserver-utils on the node and applies the
// instant Xorg starts. Best-effort: a failure just leaves the default timers.
const nodpmsXorgConf = "/etc/X11/xorg.conf.d/10-sideshow-nodpms.conf"

// nodpmsConf zeroes all four X power timers — BlankTime (legacy screen-saver) and
// the three DPMS phases — so none of them ever fire on an idle kiosk.
const nodpmsConf = `# AGENT-MANAGED (sideshow -start-x): never auto-blank/sleep the kiosk.
# The agent sleeps the display explicitly via xrandr, so disable X's own screen-
# saver + DPMS timers — a modern Xorg's default DPMS (~10 min) must not blank an
# idle screen. Remove this file (and restart Xorg) to restore the X defaults.
Section "ServerFlags"
	Option "BlankTime"   "0"
	Option "StandbyTime" "0"
	Option "SuspendTime" "0"
	Option "OffTime"     "0"
EndSection
`

func writeNoDPMSConf() {
	if err := os.MkdirAll("/etc/X11/xorg.conf.d", 0o755); err != nil {
		log.Printf("[xserver] mkdir xorg.conf.d: %v (DPMS blanking left at X default)", err)
		return
	}
	if err := os.WriteFile(nodpmsXorgConf, []byte(nodpmsConf), 0o644); err != nil {
		log.Printf("[xserver] write %s: %v (DPMS blanking left at X default)", nodpmsXorgConf, err)
	}
}

// buildXorg runs the X server as ROOT on the X VT. Root gets DRM master + input
// directly (no logind session needed), and the supervisor drives VT switches to
// hand the master to/from direct-KMS modes — so a single owner arbitrates the
// display instead of an uncoordinated display manager.
func (x *XServer) buildXorg() *exec.Cmd {
	args := []string{
		x.cfg.Display, fmt.Sprintf("vt%d", x.cfg.XVT),
		"-auth", x.cfg.XAuthority, "-nolisten", "tcp",
	}
	args = append(args, strings.Fields(x.cfg.XServerArgs)...)
	cmd := exec.CommandContext(x.ctx, x.cfg.XServerCmd, args...)
	cmd.Stdout = prefixWriter(os.Stdout, "Xorg")
	cmd.Stderr = prefixWriter(os.Stderr, "Xorg")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // root: no cred drop
	return cmd
}

// buildSeatClient runs argv as the seat user against the agent's X server — the
// shared shape for every long-lived X client the base owns (the WM, the cursor
// hider): seat-user creds, DISPLAY/XAUTHORITY pointed at our server, prefixed logs.
func (x *XServer) buildSeatClient(name string, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(x.ctx, argv[0], argv[1:]...)
	cmd.Env = []string{
		"DISPLAY=" + x.cfg.Display,
		"XAUTHORITY=" + x.cfg.XAuthority,
		"HOME=" + x.cfg.Home,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	cmd.Stdout = prefixWriter(os.Stdout, name)
	cmd.Stderr = prefixWriter(os.Stderr, name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if x.cfg.cred != nil {
		cmd.SysProcAttr.Credential = x.cfg.cred
	}
	return cmd
}

// buildWM runs the minimal window manager as the seat user against the X server.
// matchbox fullscreens + focuses the single client — the only WM service a kiosk
// needs (X11 has no focus/EWMH-fullscreen policy without a WM). Its keyboard
// shortcuts (Alt+Tab/Alt+F4/…), if the build has any, are neutered under
// -lock-input by the empty ~/.matchbox/kbdconfig the agent writes
// (EnsureKioskLockdown) — matchbox has no config flag to pass here.
func (x *XServer) buildWM() *exec.Cmd {
	return x.buildSeatClient("wm", append([]string{x.cfg.WMCmd}, strings.Fields(x.cfg.WMArgs)...))
}

// buildCursorHide runs the cursor-hider as the seat user. The agent's bespoke X
// base has no session manager, so the distro's XDG-autostart / Xsession.d hook that
// normally starts one never fires — and a fresh Xorg parks the pointer dead-center
// over the fullscreen kiosk. unclutter-xfixes hides it via the XFIXES extension
// (display-wide, no pointer grab, so x11vnc's injected input still reaches the
// kiosk). X11-only: labwc already hides the idle pointer itself. Guarded by a
// non-empty CursorHideCmd in Start, so Fields() here yields at least the binary.
func (x *XServer) buildCursorHide() *exec.Cmd {
	return x.buildSeatClient("cursor", strings.Fields(x.cfg.CursorHideCmd))
}

// supervise runs build() and restarts it on exit with capped backoff until ctx is
// cancelled — the Xorg/WM analogue of the mode runner's restart-on-exit.
func (x *XServer) supervise(name string, build func() *exec.Cmd) {
	backoff := time.Second
	for {
		if x.ctx.Err() != nil {
			return
		}
		cmd := build()
		if err := cmd.Start(); err != nil {
			log.Printf("[xserver] %s start: %v", name, err)
		} else {
			log.Printf("[xserver] %s started pid=%d", name, cmd.Process.Pid)
			err := cmd.Wait()
			if x.ctx.Err() != nil {
				return
			}
			log.Printf("[xserver] %s exited (%v); restarting", name, err)
			backoff = time.Second // a clean run resets the backoff
		}
		if !x.sleep(backoff) {
			return
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (x *XServer) sleep(d time.Duration) bool {
	select {
	case <-x.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// waitReady blocks until the X server accepts a connection on its unix socket
// (creation of the socket is X's "ready to accept clients" signal).
func (x *XServer) waitReady(timeout time.Duration) error {
	sock := "/tmp/.X11-unix/X" + strings.TrimPrefix(x.cfg.Display, ":")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if x.ctx.Err() != nil {
			return fmt.Errorf("xserver cancelled before ready")
		}
		if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("X server %s not ready after %s", x.cfg.Display, timeout)
}
