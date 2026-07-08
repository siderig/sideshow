// Command agent is the sideshow per-node supervisor: it owns the screen,
// arbitrates display modes (web / app / airplay / …), supervises the active
// mode's child process with restart-on-exit, and serves a local control
// webUI + JSON API on :80. See docs/node-api.md and docs/ROADMAP.md §9.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config is the node-specific wiring, all overridable by flags so the same
// binary runs on disp (X11/openbox today) or any other node.
type Config struct {
	Addr           string // listen address for the webUI+API, e.g. ":80"
	Node           string // node name shown in the API
	SeatUser       string // user to run display children as (priv-dropped from root)
	Display        string // X DISPLAY for compositor-hosted modes
	XAuthority     string // Xauthority for the seat user
	RuntimeDir     string // XDG_RUNTIME_DIR for the seat user
	Home           string // HOME for the seat user
	Chromium       string // chromium binary path
	ProfileDir     string // chromium user-data-dir (separate from the legacy kiosk)
	DefaultBg      string // Chromium base background color (ARGB hex) — kills the white flash before a page paints
	ChromiumLowMem bool   // apply a low-memory kiosk flag profile (one renderer, capped JS heap, no GPU process) for weak nodes
	CDPHost        string // remote-debugging bind address (localhost only)
	CDPPort        int    // remote-debugging port
	StartMode      string // mode to enter on launch: web | wayland | off
	InitialURL     string // url for the initial web mode
	NoPrivDrop     bool   // run children as the current user (testing as the seat user)
	XVT            int    // the VT the compositor (Xorg) runs on — compositor modes live here
	ModeVT         int    // the VT console/kms foreground modes run on (≥8: no autovt getty)
	FixMode        bool   // force the X output to its EDID-preferred mode at startup
	CECDevice      string // HDMI-CEC device for TV control (cec-ctl), e.g. /dev/cec0
	CECName        string // OSD name to announce on the CEC bus
	CECMonitor     bool   // watch the CEC bus to learn the TV's real power state
	VNCPort        int    // localhost RFB port for the on-demand x11vnc server
	VNCScale       string // downscale the live-view capture (x11vnc -scale, e.g. "0.5"); "" = native
	VNCMaxFPS      int    // cap the live-view update rate in frames/sec; 0 = uncapped
	VNCNice        int    // run the capture server at this nice increment (+ idle I/O); 0 = normal

	KmsShotCmd        string // universal DRM/KMS screenshot helper (disp-kmsshot); "" disables the KMS backend
	DriCard           string // DRM primary node disp-kmsshot reads the scanout from
	ScreenshotBackend string // screenshot source: auto (KMS, fall back to CDP/scrot) | kms | cdp | scrot

	AllowMiracast       bool   // enable the (experimental) miracast mode — may disrupt Wi-Fi
	MiracastCmd         string // launcher for miracast mode (an X11 wireless-display sink)
	MiracastIface       string // pin the miracast P2P sink to this wireless iface (a 2nd adapter); exported to the launcher
	MiracastMaxMinutes  int    // auto-stop miracast after N minutes (0 = unlimited)
	MiracastAbortAfterS int    // auto-stop + restore if the uplink is lost N seconds while miracast is on (0 = off)
	SteamlinkCmd        string // launcher for steamlink mode (apt 'steamlink' or a Flatpak)

	PlymouthCmdline  string // kernel cmdline file the boot-splash toggle edits
	PlymouthThemeDir string // the sideshow Plymouth theme directory on the node
	PlymouthTheme    string // the Plymouth theme name

	ChromiumPolicyDir string // dir for the kiosk Chromium managed-policy file (empty disables)
	DisableTranslate  bool   // disable the kiosk's Translate UI via managed policy
	CookieExtension   string // Web Store extension ID to force-install for cookie-dialog dismissal (empty disables)

	WaylandLauncher string // script that runs labwc + Chromium-Wayland
	WaylandVT       int    // VT the labwc Wayland primary owns (≥8; X stays VT-suspended on XVT)
	WaylandCDPPort  int    // remote-debugging port of the Wayland Chromium (distinct from X's)
	WaylandRoot     bool   // legacy: run labwc as root via libseat 'builtin' (software/pixman)

	StartX        bool   // agent starts + owns Xorg + a WM (matchbox) instead of a display manager (lightdm)
	XServerCmd    string // X server binary launched when -start-x
	XServerArgs   string // extra args for the agent-launched X server
	WMCmd         string // window manager launched under the agent's X server (fullscreen + focus only)
	WMArgs        string // window-manager args
	CursorHideCmd string // cursor-hider supervised under -start-x (hides the pointer a fresh Xorg parks over the kiosk; empty disables)
	LockInput     bool   // kiosk input lockdown: strip the compositor's window-switch/menu/close keybinds (labwc + matchbox) and enforce -novtswitch on X11
	NoLocalInput  bool   // pure display: make the compositor ignore ALL local keyboard/pointer/touch devices (udev + xorg.conf.d); remote (VNC/panel) input still works

	CogCmd          string // the WPE WebKit browser binary for the framebuffer web mode (web + display=kms)
	CogVideoMode    string // COG_PLATFORM_DRM_VIDEO_MODE for cog (e.g. "1920x1080"); empty = EDID-preferred
	CogCtlCmd       string // cogctl: cog's D-Bus control client, for in-place navigate/reload of the cog kiosk
	StateFile       string // persisted display state (rotation + zoom), survives restarts
	DocsDir         string // directory the document feature may serve local files from (no path escapes)
	MediaDir        string // uploadable media library dir (images/videos/audio/docs), served at /media
	AuthKeyFile     string // file holding the UI/API auth key (empty/missing → no auth)
	AuthKey         string // resolved auth key (from -auth-key or the key file)
	InitAuthKey     bool   // first-run provisioning: mint a random key at -auth-key-file if it is missing/empty
	AllowShutdown   bool   // allow POST /api/shutdown (poweroff) — off for unattended nodes
	AllowCustomRoot bool   // allow custom app modes to run as ROOT on the framebuffer (display=kms)
	NodeLabel       string // human label (e.g. "Lobby screen") for the fleet view
	NodeGroup       string // group/site for bulk control
	AutoHostname    bool   // on first boot, rename a stock-default hostname to sideshow-<serial4>
	Comitup         bool   // start the comitup recovery Wi-Fi AP at boot when there is no network
	HeartbeatURL    string // POST status here on a timer (the central aggregator); "" = off
	HeartbeatSec    int    // heartbeat interval (seconds)
	Watchdog        bool   // run the network/render watchdog
	WatchdogProbe   string // TCP address the watchdog dials to test connectivity
	WatchdogReboot  bool   // let the watchdog reboot the node when the mode stays down

	HTTPSAddr            string // listen address for the opt-in self-signed HTTPS listener (e.g. ":443")
	TailscaleAuthKeyFile string // staged pre-auth key consumed + shredded on first boot (flashed images); "" disables

	cred *syscall.Credential // resolved seat-user credential when running as root
}

// version is stamped by the release build (-ldflags "-X main.version=…"); "dev"
// for a plain `go build`.
var version = "dev"

func main() {
	log.SetOutput(io.MultiWriter(os.Stderr, logs)) // also feed the /api/logs ring
	log.Printf("sideshow-agent %s", version)
	cfg := &Config{}
	flag.StringVar(&cfg.Addr, "addr", ":80", "webUI+API listen address")
	flag.StringVar(&cfg.Node, "node", hostname(), "node name")
	flag.StringVar(&cfg.SeatUser, "seat-user", "sideshow", "user to run display children as")
	flag.StringVar(&cfg.Display, "display", ":0", "X DISPLAY for compositor modes")
	flag.StringVar(&cfg.XAuthority, "xauthority", "", "Xauthority path (default: <seat-home>/.Xauthority)")
	flag.StringVar(&cfg.RuntimeDir, "xdg-runtime-dir", "", "XDG_RUNTIME_DIR (default: /run/user/<uid>)")
	flag.StringVar(&cfg.Home, "home", "", "HOME for the seat user (default: from passwd)")
	flag.StringVar(&cfg.Chromium, "chromium", "/usr/bin/chromium", "chromium binary")
	flag.StringVar(&cfg.ProfileDir, "profile-dir", "", "chromium user-data-dir (default: <seat-home>/.config/sideshow-chromium)")
	flag.StringVar(&cfg.DefaultBg, "default-bg", "ff000000", "Chromium base background color, ARGB hex (e.g. ff000000=black, ffffffff=white) — painted before a page loads, killing the white flash")
	flag.BoolVar(&cfg.ChromiumLowMem, "chromium-low-mem", false, "low-end kiosk: apply a memory-reduction Chromium flag profile (single renderer, --process-per-site, no GPU process, capped V8 heap + disk cache) — the middle ground between a full Chromium kiosk and the framebuffer cog browser")
	flag.StringVar(&cfg.CDPHost, "cdp-host", "127.0.0.1", "Chromium remote-debugging bind address (keep localhost)")
	flag.IntVar(&cfg.CDPPort, "cdp-port", 9222, "Chromium remote-debugging port")
	flag.StringVar(&cfg.StartMode, "start-mode", "web", "mode to enter on launch: web (X11) | wayland (labwc GPU) | off")
	flag.StringVar(&cfg.InitialURL, "url", "about:blank", "initial web URL (first-run default only; the operator sets real content via the webUI)")
	flag.BoolVar(&cfg.NoPrivDrop, "no-priv-drop", false, "run children as the current user (no setuid)")
	flag.IntVar(&cfg.XVT, "x-vt", 7, "VT the compositor (Xorg) runs on")
	flag.IntVar(&cfg.ModeVT, "mode-vt", 9, "VT for console/kms foreground modes (≥8 avoids autovt gettys; kept distinct from -wayland-vt so a console mode can layer over the Wayland primary)")
	flag.BoolVar(&cfg.FixMode, "fix-mode", true, "force the connected X output to its EDID-preferred mode at startup (boot robustness)")
	flag.StringVar(&cfg.CECDevice, "cec-device", "/dev/cec0", "HDMI-CEC device (cec-ctl) for TV power control")
	flag.StringVar(&cfg.CECName, "cec-name", "sideshow", "OSD name announced on the CEC bus")
	flag.BoolVar(&cfg.CECMonitor, "cec-monitor", false, "watch the CEC bus to track the TV's real power state (a persistent cec-ctl --monitor)")
	flag.IntVar(&cfg.VNCPort, "vnc-port", 5900, "localhost RFB port for the on-demand x11vnc server")
	flag.StringVar(&cfg.VNCScale, "vnc-scale", "", "low-end live view: downscale the capture (x11vnc -scale value, e.g. 0.5 or 2/3); empty = native resolution. x11vnc only")
	flag.IntVar(&cfg.VNCMaxFPS, "vnc-max-fps", 0, "low-end live view: cap the capture/update rate in frames/sec (x11vnc -wait/-defer, wayvnc --max-fps); 0 = uncapped")
	flag.IntVar(&cfg.VNCNice, "vnc-nice", 0, "low-end live view: run the capture server at this nice increment (1-19) plus idle I/O priority so it can't starve the kiosk; 0 = normal priority")
	flag.StringVar(&cfg.KmsShotCmd, "kmsshot-cmd", "disp-kmsshot", "universal DRM/KMS screenshot helper that captures any mode below the compositor (X11/Wayland/cog-KMS/console); empty disables the KMS backend (falls back to CDP/scrot)")
	flag.StringVar(&cfg.DriCard, "dri-card", "/dev/dri/card0", "DRM primary node disp-kmsshot reads the scanout from")
	flag.StringVar(&cfg.ScreenshotBackend, "screenshot-backend", "auto", "screenshot source: auto (KMS scanout, fall back to CDP for web / scrot for X / grim for Wayland) | kms | cdp | scrot | grim")
	flag.BoolVar(&cfg.AllowMiracast, "allow-miracast", false, "enable the experimental miracast mode (a wireless-display sink); off by default because Wi-Fi P2P can disrupt the node's own uplink on a single-adapter box")
	flag.StringVar(&cfg.MiracastCmd, "miracast-cmd", "gnome-network-displays", "launcher for miracast mode — an X11 wireless-display sink (e.g. gnome-network-displays, or a wrapper around lazycast)")
	flag.StringVar(&cfg.MiracastIface, "miracast-iface", "", "pin the Miracast P2P sink to this wireless interface (a dedicated 2nd adapter avoids contending with the uplink radio); exported to the launcher as SIDESHOW_MIRACAST_IFACE")
	flag.IntVar(&cfg.MiracastMaxMinutes, "miracast-max-minutes", 30, "auto-stop Miracast after this many minutes (0 = unlimited) — bounds how long the P2P sink can hold the radio")
	flag.IntVar(&cfg.MiracastAbortAfterS, "miracast-abort-after", 30, "auto-stop Miracast + restore the previous mode if the node's uplink stays down this many seconds while it's on screen (0 = disabled) — self-recovery from the single-radio P2P hazard")
	flag.StringVar(&cfg.SteamlinkCmd, "steamlink-cmd", "steamlink", "launcher for steamlink mode (e.g. 'steamlink' from apt, or 'flatpak run com.valvesoftware.SteamLink')")
	flag.StringVar(&cfg.PlymouthCmdline, "plymouth-cmdline", "/boot/firmware/cmdline.txt", "kernel cmdline file the boot-splash toggle edits (add/remove 'splash')")
	flag.StringVar(&cfg.PlymouthThemeDir, "plymouth-theme-dir", "/usr/share/plymouth/themes/sideshow", "the sideshow Plymouth theme directory on the node")
	flag.StringVar(&cfg.PlymouthTheme, "plymouth-theme", "sideshow", "Plymouth theme name")
	flag.StringVar(&cfg.ChromiumPolicyDir, "chromium-policy-dir", "/etc/chromium/policies/managed", "dir for the kiosk Chromium managed-policy file (empty disables policy management)")
	flag.BoolVar(&cfg.DisableTranslate, "disable-translate", true, "disable Chromium's Translate UI in the kiosk (via managed policy)")
	flag.StringVar(&cfg.CookieExtension, "cookie-extension", "edibdbjcniadpccecjdfdjjppcpchdlm", "Chrome Web Store extension ID to force-install for auto-dismissing cookie dialogs (default: I-still-dont-care-about-cookies; empty disables)")
	flag.StringVar(&cfg.WaylandLauncher, "wayland-launcher", "/home/sideshow/run-wayland.sh", "script that launches labwc + Chromium-Wayland (run as root)")
	flag.IntVar(&cfg.WaylandVT, "wayland-vt", 8, "VT the labwc Wayland primary owns (X stays VT-suspended on -x-vt)")
	flag.IntVar(&cfg.WaylandCDPPort, "wayland-cdp-port", 9223, "Chromium remote-debugging port under Wayland (distinct from -cdp-port)")
	flag.BoolVar(&cfg.WaylandRoot, "wayland-root", false, "legacy: run labwc as root via libseat 'builtin' (software/pixman); default runs it as the seat user via seatd (GPU/GLES2, matches RPi OS)")
	flag.BoolVar(&cfg.StartX, "start-x", false, "the agent starts + owns Xorg + a window manager (matchbox) itself instead of relying on a display manager (lightdm) — for X11 nodes; the X11 modes attach to it")
	flag.StringVar(&cfg.XServerCmd, "x-server-cmd", "Xorg", "X server binary the agent launches under -start-x")
	flag.StringVar(&cfg.XServerArgs, "x-server-args", "-seat seat0 -novtswitch", "extra args for the agent-launched X server (display, vt, -auth, -nolisten tcp are added automatically)")
	flag.StringVar(&cfg.WMCmd, "wm-cmd", "matchbox-window-manager", "minimal window manager the agent runs under -start-x (fullscreens + focuses the single client; the X11 analogue of labwc)")
	flag.StringVar(&cfg.WMArgs, "wm-args", "-use_titlebar no", "args for the window manager")
	flag.StringVar(&cfg.CursorHideCmd, "cursor-hide-cmd", "unclutter-xfixes --timeout 1 --start-hidden", "cursor-hider the agent supervises under -start-x as a seat-user X client — hides the mouse pointer a fresh Xorg parks over the kiosk (XFIXES-based, no pointer grab so VNC input still works); empty disables. Wayland already hides the idle pointer itself.")
	flag.BoolVar(&cfg.LockInput, "lock-input", false, "kiosk input lockdown: strip the compositor's window-switch/menu/close keybinds so Alt+Tab / Super / right-click-menu / Alt+F4 are inert (labwc rc.xml with no defaults; empty ~/.matchbox/kbdconfig on X11), and enforce -novtswitch on the agent's Xorg. Off by default (changes local input handling — enable per node). NOTE: does not disable VT switching under Wayland — that is a logind/getty task (see docs/node-api.md).")
	flag.BoolVar(&cfg.NoLocalInput, "no-local-input", false, "pure display: the compositor ignores ALL local keyboard/mouse/touch devices, so nothing reaches the kiosk (stronger than -lock-input, which leaves the page interactive). Installs a libinput udev rule (+ an xorg.conf.d Ignore on -start-x); remote control (VNC/panel) still works via virtual-input. Off restores local input on the next compositor start.")
	flag.StringVar(&cfg.CogCmd, "cog-cmd", "cog", "the framebuffer web mode (web + display=kms): WPE WebKit's cog, rendered directly on DRM/KMS with no X/Wayland — a far lighter kiosk for weak nodes")
	flag.StringVar(&cfg.CogVideoMode, "cog-video-mode", "", "DRM video mode for cog (COG_PLATFORM_DRM_VIDEO_MODE), e.g. 1920x1080; empty = EDID-preferred")
	flag.StringVar(&cfg.CogCtlCmd, "cogctl-cmd", "cogctl", "cog's D-Bus control client — used to navigate/reload the running cog kiosk in place (cog has no CDP)")
	flag.StringVar(&cfg.StateFile, "state-file", "/var/lib/sideshow/display.json", "persisted display state (rotation + zoom)")
	flag.StringVar(&cfg.DocsDir, "docs-dir", "/var/lib/sideshow/docs", "directory the document feature may serve local files from (no path escapes)")
	flag.StringVar(&cfg.MediaDir, "media-dir", "/var/lib/sideshow/media", "uploadable media library dir (images/videos/audio/docs), served at /media (no path escapes)")
	flag.StringVar(&cfg.AuthKeyFile, "auth-key-file", "/etc/sideshow/agent.key", "file holding the UI/API key (missing/empty → no auth)")
	flag.StringVar(&cfg.AuthKey, "auth-key", "", "UI/API key (overrides -auth-key-file; mainly for testing)")
	flag.BoolVar(&cfg.InitAuthKey, "init-auth-key", false, "first-run provisioning: mint a unique random key at -auth-key-file if it is missing/empty (used by flashed images so no shared secret is ever baked in)")
	flag.StringVar(&cfg.HTTPSAddr, "https-addr", ":443", "listen address for the opt-in self-signed HTTPS listener (served alongside -addr when enabled)")
	flag.StringVar(&cfg.TailscaleAuthKeyFile, "tailscale-authkey-file", "/etc/sideshow/tailscale.authkey", "staged Tailscale pre-auth key: if present at first boot the node joins the tailnet, then the file is shredded (flashed images); missing = never auto-joins")
	flag.BoolVar(&cfg.AllowShutdown, "allow-shutdown", true, "allow POST /api/shutdown (poweroff); set false on unattended nodes that can't be powered back on remotely")
	flag.BoolVar(&cfg.AllowCustomRoot, "allow-custom-root", false, "allow custom app modes to run as ROOT on the framebuffer (display=kms) — off by default (arbitrary root program execution behind the auth gate)")
	flag.StringVar(&cfg.NodeLabel, "node-label", "", "human label for the fleet view, e.g. \"Lobby screen\"")
	flag.StringVar(&cfg.NodeGroup, "node-group", "", "group/site name for bulk control")
	flag.BoolVar(&cfg.AutoHostname, "auto-hostname", false, "on first boot, rename a stock-default hostname (raspberrypi/debian) to sideshow-<serial4>; never touches an operator-chosen or load-bearing name")
	flag.BoolVar(&cfg.Comitup, "comitup", false, "at boot, start the comitup recovery Wi-Fi AP when there is no default route and a wireless device exists (so a headless node with no network is still reachable to configure Wi-Fi)")
	flag.StringVar(&cfg.HeartbeatURL, "heartbeat-url", "", "POST node status here on a timer (central aggregator); empty disables")
	flag.IntVar(&cfg.HeartbeatSec, "heartbeat-interval", 60, "heartbeat interval in seconds")
	flag.BoolVar(&cfg.Watchdog, "watchdog", true, "run the network/render watchdog (reload kiosk on network recovery; restart a CDP-wedged kiosk)")
	flag.StringVar(&cfg.WatchdogProbe, "watchdog-probe", "1.1.1.1:53", "TCP address the watchdog dials to test connectivity")
	flag.BoolVar(&cfg.WatchdogReboot, "watchdog-reboot", false, "let the watchdog reboot the node when the mode stays down (failed) for several minutes")
	flag.Parse()

	if err := cfg.resolve(); err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("sideshow agent: node=%s addr=%s seat=%s(uid drop=%v) display=%s cdp=%s:%d",
		cfg.Node, cfg.Addr, cfg.SeatUser, cfg.cred != nil, cfg.Display, cfg.CDPHost, cfg.CDPPort)

	// Shape the kiosk Chromium's defaults (no Translate bar, auto-dismiss cookie
	// dialogs) via a managed policy, before the first kiosk launch reads it.
	EnsureChromiumPolicy(cfg)
	// Place the compositor input-lockdown config (labwc rc.xml / matchbox
	// kbdconfig) before the first labwc/matchbox launch reads it. No-op unless
	// -lock-input.
	EnsureKioskLockdown(cfg)
	// Pure-display input lockout (udev + xorg.conf.d) is applied from the PERSISTED
	// policy just after the State manager is constructed below — before the compositor
	// starts — so the first labwc/Xorg enumerates with the right devices and a reboot
	// always matches the operator's saved choice, not just the -no-local-input flag.
	// Reconcile the seat-level VT-switch lockdown (mask login gettys on a non-X
	// -lock-input node; unmask when off). Off the hot path — it shells out to
	// systemctl and isn't needed for the kiosk to come up.
	go ReconcileVTLockdown(cfg)

	sup := NewSupervisor(cfg)
	apt := NewApt()
	cec := NewCEC(cfg)
	display := NewDisplay(cfg, sup, cec)
	stats := NewStats(cfg, apt, display)
	vnc := NewVNC(cfg, sup)
	power := NewPower(cfg, sup, display)
	plymouth := NewPlymouth(cfg)
	content := NewContent(cfg, sup)
	state := NewState(cfg)
	library := NewLibrary(cfg)
	playlist := NewPlaylist(cfg)
	actions := NewActions(cfg)
	actions.Seed(state.Info().CustomModes) // migrate legacy app-only custom modes on first run
	// Local-input policy: the persisted setting is the source of truth; the
	// -no-local-input flag seeds it once on first boot. Apply it (udev/Xorg rules)
	// synchronously here — before the compositor starts — and on every boot, so a
	// reboot always matches the saved policy rather than the flag.
	if !state.LocalInputSet() {
		state.SetLocalInput(!cfg.NoLocalInput)
	}
	ApplyNoLocalInput(cfg, !state.LocalInputAllowed(!cfg.NoLocalInput))
	miracast := NewMiracast(cfg, sup, state)
	sup.SetMiracast(miracast)
	netmgr := NewNet(cfg)
	setup := NewSetup(cfg, state)
	tsmgr := NewTailscale(cfg, netmgr)
	// Cross-link display ↔ content (post-construction to avoid a constructor
	// cycle): content routes per-output assignments via the display's primary;
	// the display folds per-output content into /api/outputs + /api/status.
	display.SetContent(content)
	content.SetDisplay(display)
	srv := NewServer(cfg, sup, stats, apt, cec, vnc, display, power, content, plymouth, state, library, playlist, actions, miracast, netmgr, setup, tsmgr)
	// The self-signed HTTPS listener wraps the same handler (srv), so it is created
	// after srv and linked back in (avoids a constructor cycle), then brought up if
	// the operator had previously enabled it.
	tlsm := NewTLSManager(cfg, netmgr, srv)
	srv.SetTLS(tlsm)
	// After an unattended return-to-base (a console mode the operator quit, or a
	// crash recovery), re-record what's now on screen so the persisted "active"
	// matches the screen — else a reboot would relaunch the retired mode.
	sup.SetOnSettle(srv.recordActive)
	apt.Start()
	stats.Start()
	cec.Start()
	if cfg.CECMonitor {
		cec.StartMonitor()
	}
	// One display timeline: fold any legacy nightly window (display.json) into the
	// unified weekly scheduler, then run only that scheduler (weekly.go). This retires
	// the second, independent nightly ticker so the two can't fight over screen power.
	srv.MigrateNightly(display.legacyNightly())
	srv.StartScheduler() // weekly time→action schedule + nightly window (weekly.go)
	content.Start()
	miracast.Start()
	NewWatchdog(cfg, sup).Start()
	NewHeartbeat(cfg, sup, stats, netmgr).Start()
	tsmgr.Start()         // background tailnet-status refresher (fork-free snapshot)
	tlsm.StartIfEnabled() // restore the self-signed HTTPS listener if it was on
	// Boot-time network actions (all opt-in, no-ops unless configured): rename a
	// stock-default hostname, raise the comitup recovery AP if the node booted with
	// no network, and consume a staged Tailscale pre-auth key (flashed images).
	// Off the hot path (they fork).
	go func() {
		netmgr.MaybeAutoName()
		netmgr.MaybeStartComitup()
		tsmgr.MaybeJoinFromKeyFile()
	}()
	display.SeedZoom() // before the initial navigate so a persisted zoom takes effect

	// Boot robustness: a display that was asleep/unplugged when X started leaves
	// the vc4-KMS connector on a 1024x768 fallback (config.txt's hdmi_mode does
	// NOT override it). Re-assert the EDID-preferred mode, then the persisted
	// rotation, in the background — staggered ~25s so these xrandr forks don't
	// stack load onto the kiosk's Chromium cold-start (watchdog-fragile window).
	go func() {
		time.Sleep(25 * time.Second)
		if cfg.FixMode {
			EnsurePreferredMode(cfg)
		}
		display.ApplyLayoutAtBoot() // restore mirror/extend first (it re-applies rotation)
		display.ApplyRotationAtBoot()
	}()

	// Own the X server when asked: the agent starts + supervises Xorg + a minimal
	// WM itself (no display manager). The X11 modes below attach to this server.
	var xserver *XServer
	if cfg.StartX {
		xserver = NewXServer(cfg)
		if err := xserver.Start(); err != nil {
			log.Printf("X server start failed: %v (X11 modes will not work until it is up)", err)
		}
	}

	// Enter the initial mode. Prefer the persisted active mode so a reboot or
	// agent restart returns to the same screen; fall back to -start-mode/-url on a
	// fresh node (no state yet). Non-fatal: the API still comes up so you can
	// recover from the webUI even if the first child fails. Recording the mode
	// after a successful switch keeps the persisted "active" in sync and, on a
	// fresh node, pins the operator's -url instead of re-applying the default
	// every boot. "wayland" boots the labwc GPU primary directly; "web" is X11.
	initial, restored := state.Restore()
	// A node that already has a persisted mode is provisioned → migrate the setup
	// flag to complete so the first-run wizard + its pre-auth /setup exemption stay
	// inert (older state.json predates the flag being written). Synchronous, before
	// the HTTP server starts, so there is no window where /setup is reachable
	// pre-auth on a live node.
	if restored && !state.SetupComplete() {
		state.SetSetupComplete(true)
	}
	if !restored {
		switch {
		case !state.SetupComplete():
			// Fresh, unprovisioned node → the first-run setup wizard. Reached from a
			// browser via the printed LAN URL; also shown on the kiosk when a
			// compositor + Chromium already exist.
			initial = Mode{Type: ModeWeb, Params: map[string]any{"url": setupURL(cfg)}}
		case cfg.StartMode == ModeOff || cfg.StartMode == "none" || cfg.StartMode == "":
			initial = Mode{Type: ModeOff}
		case cfg.StartMode == "wayland":
			initial = Mode{Type: ModeWeb, Display: DisplayWayland, Params: map[string]any{"url": cfg.InitialURL}}
		default:
			initial = Mode{Type: ModeWeb, Params: map[string]any{"url": cfg.InitialURL}}
		}
	}
	// Only record on a FRESH node (to pin the operator's -url so the default
	// doesn't re-apply every boot). When restoring, the active mode is
	// already persisted and unchanged — re-recording it here would race a
	// concurrent operator switch in the cold-boot window and clobber their choice.
	if initial.Type == ModeOff || initial.Type == "" {
		log.Printf("starting in off mode")
		if !restored {
			state.RecordMode(Mode{Type: ModeOff})
		}
	} else {
		go func() {
			if err := sup.Switch(initial); err != nil {
				log.Printf("initial mode failed: %v", err)
				return
			}
			if restored {
				// Surface the footgun: once a mode is persisted, -url/-start-mode in
				// the unit are ignored (Restore wins). Change the display from the
				// webUI, or delete state.json, to override.
				log.Printf("restored persisted mode %q; -url/-start-mode now apply on first boot only", initial.label())
			} else {
				// Record what actually came up (from Status, the race-free source of
				// truth) so a fresh node pins the operator's -url once.
				state.RecordMode(modeFromStatus(sup.Status()))
			}
		}()
	}

	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv}
	go func() {
		log.Printf("serving webUI + API on %s", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	// Graceful shutdown: stop the active child so we never orphan Chromium.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down: stopping active mode")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	sup.Shutdown()
	if xserver != nil {
		xserver.Stop()
	}
	log.Printf("bye")
}

// resolve fills derived defaults and (when running as root) the seat-user
// credential used to drop privileges for display children.
func (c *Config) resolve() error {
	u, err := user.Lookup(c.SeatUser)
	if err != nil {
		return fmt.Errorf("seat user %q: %w", c.SeatUser, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if c.Home == "" {
		c.Home = u.HomeDir
	}
	if c.XAuthority == "" {
		c.XAuthority = c.Home + "/.Xauthority"
	}
	if c.RuntimeDir == "" {
		c.RuntimeDir = fmt.Sprintf("/run/user/%d", uid)
	}
	if c.ProfileDir == "" {
		c.ProfileDir = c.Home + "/.config/sideshow-chromium"
	}
	for _, p := range []*string{&c.DocsDir, &c.MediaDir} {
		if *p == "" {
			continue
		}
		if abs, err := filepath.Abs(*p); err == nil {
			*p = filepath.Clean(abs)
		} else {
			*p = filepath.Clean(*p)
		}
	}

	// Auth key: an explicit -auth-key wins; otherwise read the key file. With
	// -init-auth-key (first-boot provisioning for flashed images), mint a unique
	// random key at the file first if it is missing/empty — so every node self-keys
	// once, before it serves a request, and no shared secret is ever baked into an
	// image. A missing/empty file without -init-auth-key still just means no auth.
	if c.AuthKey == "" && c.AuthKeyFile != "" {
		if c.InitAuthKey {
			if err := ensureAuthKeyFile(c.AuthKeyFile); err != nil {
				log.Printf("[auth] init key: %v", err)
			}
		}
		if b, err := os.ReadFile(c.AuthKeyFile); err == nil {
			c.AuthKey = strings.TrimSpace(string(b))
		}
	}

	// Under input lockdown, guarantee the agent's Xorg keeps VT switching off even
	// if an operator overrode -x-server-args and dropped -novtswitch.
	if c.LockInput {
		c.XServerArgs = enforceNoVTSwitch(c.XServerArgs)
	}

	// Decide whether to drop privileges for children. Only when we are root and
	// the seat user is someone else, and not explicitly disabled.
	if !c.NoPrivDrop && os.Geteuid() == 0 && uid != 0 {
		groups := supplementaryGroups(u)
		c.cred = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}
	}
	return nil
}

// ensureAuthKeyFile mints a unique random key at path if it is missing or empty
// (dir 0700, file 0600). Used by -init-auth-key for first-boot provisioning, so a
// flashed image self-keys once per node and never ships a shared secret.
func ensureAuthKeyFile(path string) error {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return nil // already keyed
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(b[:])), 0o600); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600) // enforce 0600 regardless of umask
	log.Printf("[auth] minted a per-node key at %s", path)
	return nil
}

// supplementaryGroups returns the seat user's group ids (so Chromium keeps
// access to video/render/input groups for KMS/DRI and input).
func supplementaryGroups(u *user.User) []uint32 {
	ids, err := u.GroupIds()
	if err != nil {
		return nil
	}
	out := make([]uint32, 0, len(ids))
	for _, s := range ids {
		if g, err := strconv.Atoi(s); err == nil {
			out = append(out, uint32(g))
		}
	}
	return out
}

// waylandAppLauncherSh hosts an arbitrary GUI app as a labwc Wayland client —
// the app analogue of the web kiosk's run-wayland.sh, embedded so there is no
// per-node script to deploy. The agent runs it via `sh -c <script> sh <argv...>`
// with the seat/Wayland env (childEnv), so the app argv arrives as "$@". It saves
// the argv to a file and writes a fixed-path wrapper that reconstructs + execs it
// (so labwc's -s never parses the argv — no shell injection, mirroring
// run-wayland.sh), then runs dbus-run-session -- labwc -s <wrapper>. labwc sets
// WAYLAND_DISPLAY for the client and auto-starts Xwayland, so Wayland-native AND
// X11-only GUI apps both work.
const waylandAppLauncherSh = `set -eu
[ "$#" -ge 1 ] || { echo "wayland app: empty argv" >&2; exit 2; }
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
mkdir -p "$XDG_RUNTIME_DIR" 2>/dev/null || true
chmod 700 "$XDG_RUNTIME_DIR" 2>/dev/null || true
LIBSEAT_BACKEND="${LIBSEAT_BACKEND:-seatd}"; export LIBSEAT_BACKEND
ARGV="${XDG_RUNTIME_DIR}/sideshow-wl-app.argv"
WRAP="${XDG_RUNTIME_DIR}/sideshow-wl-app.sh"
: > "$ARGV"
for a in "$@"; do printf '%s\n' "$a" >> "$ARGV"; done
cat > "$WRAP" <<WRAPEOF
#!/bin/sh
set --
while IFS= read -r __l; do set -- "\$@" "\$__l"; done < "$ARGV"
exec "\$@"
WRAPEOF
chmod 755 "$WRAP"
# Prefer the seat user's session bus (where xdg-desktop-portal + the GTK
# color-scheme live) when the agent handed one down, so the app's webview follows
# system dark/light. Only spin up a private bus when there is no user bus.
if [ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ]; then
  exec env LIBSEAT_BACKEND="$LIBSEAT_BACKEND" ${WLR_RENDERER:+WLR_RENDERER="$WLR_RENDERER"} labwc ${SIDESHOW_LABWC_CONFIG:+-C "$SIDESHOW_LABWC_CONFIG"} -s "$WRAP"
fi
exec env LIBSEAT_BACKEND="$LIBSEAT_BACKEND" ${WLR_RENDERER:+WLR_RENDERER="$WLR_RENDERER"} dbus-run-session -- labwc ${SIDESHOW_LABWC_CONFIG:+-C "$SIDESHOW_LABWC_CONFIG"} -s "$WRAP"
`

// modeCommand resolves a mode to the binary + args to run (pure, no side
// effects — preflight uses it to check the binary exists before any teardown).
func modeCommand(cfg *Config, m Mode) (name string, args []string, err error) {
	switch m.Type {
	case ModeWeb:
		if m.isWayland() {
			// The launcher (run-wayland.sh) owns the labwc + Chromium-Wayland
			// invocation (libseat builtin, pixman, CDP on the Wayland port). The
			// agent execs it as root and attaches CDP afterwards.
			return cfg.WaylandLauncher, []string{m.str("url")}, nil
		}
		if m.Display == DisplayKMS {
			// Framebuffer web: cog (WPE WebKit) directly on DRM/KMS — no X, no
			// compositor, far lighter than Chromium on a weak node. Runs as root for
			// DRM master on the mode VT (see Mode.runsAsRoot). cog shows the launch URL
			// immediately; in-place URL changes go over cog's D-Bus control (cogctl
			// open). No CDP, so no theme/zoom/screenshot — cog's WebDriver automation
			// is unreliable on the DRM backend, so we use the rock-solid D-Bus path.
			return cfg.CogCmd, cogArgs(cfg, m.str("url")), nil
		}
		return cfg.Chromium, chromiumArgs(cfg, m.str("url")), nil
	case ModeApp:
		// Any app mode that would run as ROOT — the framebuffer (display=kms) OR a
		// display=wayland primary on a legacy -wayland-root node (both per
		// Mode.runsAsRoot) — is gated behind -allow-custom-root, so a saved custom
		// mode can't silently run an arbitrary program as root.
		if m.runsAsRoot(cfg) && !cfg.AllowCustomRoot {
			return "", nil, fmt.Errorf("this app mode would run as root (display=%s); disabled — start the agent with -allow-custom-root", m.Display)
		}
		av := m.argv()
		if m.Display == DisplayWayland {
			// Host the app as a labwc Wayland client (Xwayland covers X11-only apps).
			// `sh -c <launcher> sh <argv...>` delivers the app argv to the launcher as
			// "$@". Note: preflight checks /bin/sh, not the app binary, so a bad app
			// path surfaces as a failed start rather than a 400 at switch time.
			return "/bin/sh", append([]string{"-c", waylandAppLauncherSh, "sh"}, av...), nil
		}
		return av[0], av[1:], nil
	case ModeAirplay:
		// AirPlay receiver (uxplay). -n sets the name shown on the sender; -a
		// disables audio (default video-only — a headless Pi may have no working
		// audio sink). Discovery is via Avahi/mDNS (uxplay handles it).
		//   - display=compositor: NO -vs → the default autovideosink picks an X11/GL
		//     sink, drawing into the running Xorg session (needs X).
		//   - display=kms: -vs kmssink → GStreamer scans out directly via DRM/KMS on
		//     the mode VT (no X/Wayland; the performant path on a Pi). Runs as root
		//     for DRM master (see buildCmd).
		name := m.str("name")
		if name == "" {
			name = cfg.Node
		}
		if name != "" {
			args = append(args, "-n", name)
		}
		if m.Display == DisplayKMS {
			args = append(args, "-vs", "kmssink")
		}
		if !m.boolOr("audio", false) {
			args = append(args, "-a")
		}
		return "uxplay", args, nil
	case ModeMoonlight:
		// Moonlight receiver: moonlight-qt as an X11 client. With params.host it
		// streams that host fullscreen (params.app, default "Desktop"); without a
		// host it opens the GUI so an operator can pair (needs a keyboard/remote).
		// Pairing with a host is a one-time `moonlight-qt pair <host>` done out of
		// band. The binary is moonlight-qt (Cloudsmith/Flathub package name).
		host := m.str("host")
		if host == "" {
			return "moonlight-qt", nil, nil // GUI (pairing / host picker)
		}
		app := m.str("app")
		if app == "" {
			app = "Desktop"
		}
		return "moonlight-qt", []string{"stream", host, app}, nil
	case ModeSteamlink:
		// Steam Link receiver as an X11 client. Packaging varies (apt 'steamlink' on
		// RPi OS; a Flatpak elsewhere), so the launcher is operator-configured. Its
		// own UI handles host discovery + pairing.
		cmd := strings.Fields(cfg.SteamlinkCmd)
		if len(cmd) == 0 {
			return "", nil, fmt.Errorf("steamlink launcher is empty (-steamlink-cmd)")
		}
		return cmd[0], cmd[1:], nil
	case ModeMiracast:
		// Miracast/wireless-display sink. EXPERIMENTAL: it manipulates Wi-Fi (P2P)
		// and can knock a single-adapter node off its own uplink, so it is gated by
		// -allow-miracast and runs an operator-chosen launcher (-miracast-cmd).
		if !cfg.AllowMiracast {
			return "", nil, fmt.Errorf("miracast is disabled (start the agent with -allow-miracast; see node-api.md)")
		}
		cmd := strings.Fields(cfg.MiracastCmd)
		if len(cmd) == 0 {
			return "", nil, fmt.Errorf("miracast enabled but -miracast-cmd is empty")
		}
		return cmd[0], cmd[1:], nil
	case ModeMedia:
		// Native media (mpv). display=compositor → an X11 child (DISPLAY set, seat
		// user), like app mode. display=kms → --vo=drm: mpv scans out directly via
		// DRM/KMS on the mode VT (no compositor), running as root for DRM master.
		args = []string{"--fullscreen", "--no-osc", "--no-input-default-bindings", "--really-quiet"}
		if m.Display == DisplayKMS {
			args = append(args, "--vo=drm")
		}
		src := m.str("url")
		if src == "" {
			src = m.str("path")
		}
		if src == "" {
			return "", nil, fmt.Errorf("media mode requires params.url or params.path")
		}
		if m.boolOr("loop", true) {
			args = append(args, "--loop-file=inf")
		}
		if m.boolOr("mute", false) {
			args = append(args, "--mute=yes")
		}
		// "--" ends option parsing so a src can never be parsed as an mpv flag
		// (defense-in-depth beyond validate()'s scheme/leading-slash check).
		return "mpv", append(args, "--", src), nil
	default:
		return "", nil, fmt.Errorf("unsupported mode type %q", m.Type)
	}
}

// buildCmd constructs the child process for a mode, with the seat env, a fresh
// process group (so we can reap Chromium's whole tree), and — when running as
// root — credentials dropped to the seat user. The returned *os.File is the
// console-mode controlling TTY (nil otherwise); the caller closes it once the
// child has started.
func (s *Supervisor) buildCmd(m Mode) (*exec.Cmd, *os.File, error) {
	cfg := s.cfg
	name, args, err := modeCommand(cfg, m)
	if err != nil {
		return nil, nil, err
	}

	cmd := exec.Command(name, args...)
	cmd.Dir = cfg.Home
	cmd.Env = s.childEnv(m)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	// Every display child drops to the seat user — including the Wayland primary,
	// which now runs as the seat user via seatd (the GPU/GLES2 path that matches
	// RPi OS). The exceptions run as root: the legacy -wayland-root fallback (labwc
	// opens DRM via libseat's 'builtin' backend) and any direct-KMS mode (the child
	// must SET_MASTER on the mode VT, which a priv-dropped non-session process
	// can't). See Mode.runsAsRoot.
	if cfg.cred != nil && !m.runsAsRoot(cfg) {
		cmd.SysProcAttr.Credential = cfg.cred
	}

	// Console modes render to a dedicated VT: give the child that TTY as its
	// controlling terminal (so curses apps like htop draw + read the keyboard),
	// in its own session so killing it reaps cleanly. Other modes log to the
	// agent's stdout/stderr and use a process group for whole-tree teardown.
	var tty *os.File
	if m.Display == DisplayConsole {
		f, err := s.openModeTTY()
		if err != nil {
			return nil, nil, err
		}
		tty = f
		cmd.Stdin, cmd.Stdout, cmd.Stderr = f, f, f
		// New session so the child detaches from the agent and killing it reaps
		// cleanly. We deliberately do NOT Setctty: TIOCSCTTY after the setuid
		// drop returns EPERM, and a curses app (htop) reads/writes the inherited
		// TTY fd directly — it doesn't need the VT to be its controlling terminal.
		cmd.SysProcAttr.Setsid = true
	} else {
		cmd.Stdout = prefixWriter(os.Stdout, m.label())
		cmd.Stderr = prefixWriter(os.Stderr, m.label())
		cmd.SysProcAttr.Setpgid = true
	}
	return cmd, tty, nil
}

// openModeTTY opens the foreground-mode VT device and, when dropping
// privileges, hands ownership to the seat user so the child keeps access to its
// controlling terminal after setuid. The returned file is the parent's copy and
// must be closed once the child has started (the child keeps its own dup).
func (s *Supervisor) openModeTTY() (*os.File, error) {
	path := fmt.Sprintf("/dev/tty%d", s.cfg.ModeVT)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if s.cfg.cred != nil {
		// chown to the seat uid (like openvt/getty do) so the post-setuid child
		// can reopen /dev/tty; best-effort, the inherited fd works regardless.
		_ = os.Chown(path, int(s.cfg.cred.Uid), -1)
	}
	return f, nil
}

// childEnv builds the environment for a display child: the seat user's identity
// plus the X session vars (compositor modes). KMS-direct modes don't need
// DISPLAY but it's harmless to pass it.
func (s *Supervisor) childEnv(m Mode) []string {
	cfg := s.cfg
	// The Wayland primary: the agent is the single source of truth for what
	// run-wayland.sh reads (CDP_PORT, PROFILE, LIBSEAT_BACKEND, optionally
	// WLR_RENDERER) so they can't drift from how the agent attaches CDP / chvt's.
	if m.isWaylandPrimary() {
		const path = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
		lang := "LANG=" + getenvOr("LANG", "C.UTF-8")
		if cfg.WaylandRoot {
			// Legacy: labwc as root via libseat 'builtin', software (pixman).
			env := []string{
				"HOME=/root", "USER=root", "LOGNAME=root", path, lang,
				"XDG_RUNTIME_DIR=/run/user/0",
				"LIBSEAT_BACKEND=builtin",
				"WLR_RENDERER=pixman",
				fmt.Sprintf("CDP_PORT=%d", cfg.WaylandCDPPort),
				"PROFILE=/tmp/sideshow-wl-chromium",
				"DISP_BG=" + cfg.DefaultBg,
			}
			if labwcLockActive(cfg) {
				env = append(env, "SIDESHOW_LABWC_CONFIG="+labwcConfigDir(cfg))
			}
			return env
		}
		// Default: labwc as the seat user via seatd → wlroots GLES2 GPU renderer
		// (matches RPi OS's labwc desktop). The seat user must be in the seatd
		// group and have an XDG_RUNTIME_DIR (logind provides /run/user/<uid> via
		// the lightdm session). WLR_RENDERER is left unset → GLES2.
		env := []string{
			"HOME=" + cfg.Home, "USER=" + cfg.SeatUser, "LOGNAME=" + cfg.SeatUser, path, lang,
			"XDG_RUNTIME_DIR=" + cfg.RuntimeDir,
			"XDG_SEAT=seat0",
			fmt.Sprintf("XDG_VTNR=%d", cfg.WaylandVT),
			"XDG_SESSION_TYPE=wayland",
			"LIBSEAT_BACKEND=seatd",
			fmt.Sprintf("CDP_PORT=%d", cfg.WaylandCDPPort),
			"PROFILE=" + cfg.Home + "/.cache/sideshow-wl-chromium",
			"DISP_BG=" + cfg.DefaultBg,
		}
		if labwcLockActive(cfg) {
			env = append(env, "SIDESHOW_LABWC_CONFIG="+labwcConfigDir(cfg))
		}
		if bus := cfg.RuntimeDir + "/bus"; fileExists(bus) {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
		}
		return env
	}
	// Direct-KMS children run as root (DRM master): give them root's identity +
	// runtime dir, and DON'T set DISPLAY (no X) — GStreamer/mpv open /dev/dri
	// directly on the mode VT the supervisor chvt'd to.
	if m.Display == DisplayKMS {
		env := []string{
			"HOME=/root", "USER=root", "LOGNAME=root",
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"XDG_RUNTIME_DIR=/run/user/0",
			"LANG=" + getenvOr("LANG", "C.UTF-8"),
		}
		// Framebuffer web (cog): COG_PLATFORM=drm selects the backend (belt-and-
		// suspenders with the --platform=drm arg); POINTER=1 lets cog accept
		// pointer-class input (no CURSOR → no visible pointer). Point cog at the
		// agent-provided session bus so cogctl can drive it (navigate/reload),
		// independent of whether a root login session's /run/user/0/bus exists.
		if m.Type == ModeWeb {
			env = append(env, "COG_PLATFORM=drm", "COG_PLATFORM_DRM_POINTER=1",
				"DBUS_SESSION_BUS_ADDRESS="+cogBusAddr)
			if cfg.CogVideoMode != "" {
				env = append(env, "COG_PLATFORM_DRM_VIDEO_MODE="+cfg.CogVideoMode)
			}
		} else if bus := "/run/user/0/bus"; fileExists(bus) {
			// Other KMS children (airplay/media) use a root login bus if present.
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
		}
		return env
	}
	env := []string{
		"HOME=" + cfg.Home,
		"USER=" + cfg.SeatUser,
		"LOGNAME=" + cfg.SeatUser,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"XDG_RUNTIME_DIR=" + cfg.RuntimeDir,
		"LANG=" + getenvOr("LANG", "C.UTF-8"),
	}
	if m.Display == DisplayCompositor {
		env = append(env,
			"DISPLAY="+cfg.Display,
			"XAUTHORITY="+cfg.XAuthority,
		)
		// Hand X11 GUI children the seat user's session bus so they can reach the
		// xdg-desktop-portal color-scheme (system dark/light) — the X11 counterpart
		// of the Wayland app launcher's bus. Without it a WebKitGTK app follows only
		// the startup GTK setting, never a live/portal change.
		if bus := cfg.RuntimeDir + "/bus"; fileExists(bus) {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
		}
	}
	if m.Display == DisplayConsole {
		// Console apps use the Linux VT console; without a valid $TERM, ncurses
		// apps (htop) abort with "Error opening terminal: unknown".
		env = append(env, "TERM=linux")
	}
	// Hand the session bus to GUI children if it's where logind puts it. Without
	// this, Chromium logs D-Bus errors and some integrations misbehave.
	if bus := cfg.RuntimeDir + "/bus"; fileExists(bus) {
		env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
	}
	// Miracast: hand the launcher the operator-chosen P2P interface, so a wrapper /
	// custom -miracast-cmd can bind the sink (wpa_supplicant/gnome-network-displays)
	// to a dedicated 2nd adapter instead of the uplink radio.
	if m.Type == ModeMiracast && s.miracast != nil {
		if ifc := s.miracast.Iface(); ifc != "" {
			env = append(env, "SIDESHOW_MIRACAST_IFACE="+ifc)
		}
	}
	// Extra env is only honored for app mode (an explicitly arbitrary child);
	// it must never override the agent-set identity/X/PATH vars of managed
	// modes (web), where a caller-supplied LD_PRELOAD/XAUTHORITY would be a
	// privilege/security footgun.
	if m.Type == ModeApp {
		if raw, ok := m.Params["env"].(map[string]any); ok {
			for k, v := range raw {
				env = append(env, fmt.Sprintf("%s=%v", k, v))
			}
		}
	}
	return env
}

// postStart runs after the child is up. For the X/Wayland web kiosk it attaches
// the CDP controller and navigates. The cog kiosk (web+kms) needs nothing here —
// cog renders the launch URL itself, and runtime control is over D-Bus, not an
// attach (ROADMAP §9).
func (s *Supervisor) postStart(m Mode) error {
	if !m.usesChrome() {
		return nil
	}
	return s.chrome.Attach(m.str("url"), m.boolOr("dark", true), attachWaitCold)
}

// chromiumArgs returns the kiosk flag set (curated from disp's working viewer
// launcher) plus localhost CDP debugging. The agent then attaches over CDP.
func chromiumArgs(cfg *Config, url string) []string {
	args := []string{
		"--kiosk",
		"--start-fullscreen",
		"--no-first-run",
		"--no-default-browser-check",
		"--noerrdialogs",
		"--disable-infobars",
		"--disable-notifications",
		"--disable-session-crashed-bubble",
		"--disable-restore-session-state",
		"--disable-features=InfiniteSessionRestore,Translate",
		"--autoplay-policy=no-user-gesture-required",
		"--disable-gesture-requirement-for-media-playback",
		"--use-fake-ui-for-media-stream",
		"--overscroll-history-navigation=0",
		"--disable-pinch",
		"--hide-scrollbars", // no visible scrollbars on the kiosk (CDP also enforces this)
		"--password-store=basic",
		"--ozone-platform=x11",
		"--user-data-dir=" + cfg.ProfileDir,
		"--remote-debugging-address=" + cfg.CDPHost,
		fmt.Sprintf("--remote-debugging-port=%d", cfg.CDPPort),
		"--remote-allow-origins=*", // localhost-bound; lets chromedp's ws connect
	}
	// Low-memory profile for weak nodes (the Pi 3B blows through ~730 MB RAM on a
	// heavy page and thrashes its SD-backed swap to death). Collapse to a single
	// renderer, drop the futile GPU process (V3D is GLES2-only — Chromium is
	// software-rendered anyway), keep /dev/shm off the tiny tmpfs, and cap the V8
	// heap + disk cache so a leaky page is GC'd/killed rather than taking the box
	// down. Deliberately NOT --single-process (that removes crash isolation and
	// makes an OOM kill the whole browser) and NOT --memory-pressure-off (we WANT
	// Chromium to shed memory under pressure).
	if cfg.ChromiumLowMem {
		args = append(args,
			"--renderer-process-limit=1",
			"--process-per-site",
			"--disable-gpu",
			"--disable-dev-shm-usage",
			"--disable-background-networking",
			"--disable-component-update",
			"--disk-cache-size=16777216", // 16 MiB
			"--js-flags=--max-old-space-size=256",
		)
	}
	// Base background color painted before any page loads (and behind pages with
	// no background): an opaque black default removes the white flash on launch
	// and between navigations. Empty disables (Chromium's white default).
	if cfg.DefaultBg != "" {
		args = append(args, "--default-background-color="+cfg.DefaultBg)
	}
	return append(args, url)
}

// cogArgs is the launch line for the framebuffer web kiosk: WPE WebKit's cog on
// the DRM/KMS backend with the initial URL. The DRM video mode + pointer toggle
// are passed via env (childEnv). Runtime URL changes go over D-Bus (cogctl), not
// a relaunch.
func cogArgs(cfg *Config, url string) []string {
	return []string{"--platform=drm", url}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "node"
	}
	return strings.SplitN(h, ".", 2)[0]
}

func getenvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
