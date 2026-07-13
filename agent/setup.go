package main

import (
	"context"
	"log"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Setup drives the first-run wizard: it detects the node (arch, RAM, recommended
// compositor, which feature tools are already present, whether the seat user
// exists), installs feature prerequisites as a background apt job (mirroring the
// Apt manager so a multi-minute install can't stall the HTTP handler or the
// watchdog on a weak Pi), and records completion via State.SetupComplete.
//
// It is INERT on an already-provisioned node: main() migrates any node that has a
// persisted active mode to SetupComplete=true, and the setup surface / first-boot
// redirect are gated on !SetupComplete, so the destructive apt path never runs on
// a live node.
type Setup struct {
	cfg   *Config
	state *State

	mu         sync.Mutex
	installing bool
	logTail    string
	lastResult string // "ok" | "failed" | ""
	lastPkgs   []string
}

// SetupFeature is one installable capability → the apt packages it needs. The
// operator picks features in the wizard; the union of the selected packages is
// installed in one apt run.
type SetupFeature struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	Packages   []string `json:"packages"`
	Compositor string   `json:"compositor,omitempty"` // "x11" | "wayland" | "" (both)
	Installed  bool     `json:"installed"`            // its key tools are already present
	Required   bool     `json:"required,omitempty"`
}

// SetupInfo is the GET /api/setup payload the wizard renders from.
type SetupInfo struct {
	Complete     bool            `json:"complete"`
	Arch         string          `json:"arch"`
	RAMMB        int             `json:"ram_mb"`
	Compositor   string          `json:"recommended_compositor"` // x11 | wayland
	SeatUser     string          `json:"seat_user"`
	SeatExists   bool            `json:"seat_user_exists"`
	AptAvailable bool            `json:"apt_available"`
	AuthEnabled  bool            `json:"auth_enabled"`
	Tools        map[string]bool `json:"tools"`
	Features     []SetupFeature  `json:"features"`
	Installing   bool            `json:"installing"`
	LastResult   string          `json:"last_result,omitempty"`
	LogTail      string          `json:"log_tail,omitempty"`
}

func NewSetup(cfg *Config, state *State) *Setup {
	return &Setup{cfg: cfg, state: state}
}

// toolBinaries maps the tool names the wizard reports to the binary it probes.
var toolBinaries = map[string]string{
	"chromium":  "chromium",
	"labwc":     "labwc",
	"seatd":     "seatd",
	"matchbox":  "matchbox-window-manager",
	"uxplay":    "uxplay",
	"mpv":       "mpv",
	"x11vnc":    "x11vnc",
	"wayvnc":    "wayvnc",
	"grim":      "grim",
	"wlr-randr": "wlr-randr",
	"plymouth":  "plymouth",
	"kmsshot":   "disp-kmsshot",
	"nmcli":     "nmcli",
}

// featureCatalog returns the installable features for the given compositor
// ("x11" or "wayland"), marking each Installed if its key tools are present.
func (s *Setup) featureCatalog(compositor string) []SetupFeature {
	present := func(bins ...string) bool {
		for _, b := range bins {
			if _, err := exec.LookPath(b); err != nil {
				return false
			}
		}
		return true
	}
	var feats []SetupFeature
	if compositor == "wayland" {
		feats = append(feats,
			SetupFeature{Key: "base", Label: "Wayland kiosk (labwc + Chromium)", Compositor: "wayland", Required: true,
				Packages: []string{"chromium", "labwc", "seatd", "dbus-user-session"}, Installed: present("chromium", "labwc", "seatd")},
			SetupFeature{Key: "liveview", Label: "Live screen view (wayvnc)", Compositor: "wayland",
				Packages: []string{"wayvnc", "grim"}, Installed: present("wayvnc")},
			SetupFeature{Key: "screen-ctl", Label: "Screen sleep/wake (wlr-randr)", Compositor: "wayland",
				Packages: []string{"wlr-randr"}, Installed: present("wlr-randr")},
		)
	} else {
		feats = append(feats,
			SetupFeature{Key: "base", Label: "X11 kiosk (Chromium + matchbox)", Compositor: "x11", Required: true,
				Packages: []string{"chromium", "matchbox-window-manager", "xauth", "unclutter-xfixes"}, Installed: present("chromium", "matchbox-window-manager")},
			SetupFeature{Key: "liveview", Label: "Live screen view (x11vnc)", Compositor: "x11",
				Packages: []string{"x11vnc"}, Installed: present("x11vnc")},
		)
	}
	feats = append(feats,
		SetupFeature{Key: "airplay", Label: "AirPlay receiver (uxplay)",
			Packages: []string{"uxplay", "gstreamer1.0-plugins-bad", "avahi-daemon"}, Installed: present("uxplay")},
		SetupFeature{Key: "media", Label: "Media player (mpv)",
			Packages: []string{"mpv"}, Installed: present("mpv")},
		SetupFeature{Key: "theming", Label: "System light/dark theming",
			Packages: []string{"xdg-desktop-portal", "xdg-desktop-portal-gtk", "gsettings-desktop-schemas", "xsettingsd", "adwaita-icon-theme"}, Installed: present("xsettingsd")},
		SetupFeature{Key: "splash", Label: "Boot splash (Plymouth)",
			Packages: []string{"plymouth", "plymouth-themes"}, Installed: present("plymouth")},
		SetupFeature{Key: "screenshot", Label: "Universal DRM screenshot build deps",
			Packages: []string{"gcc", "make", "pkg-config", "libdrm-dev", "libegl-dev", "libgles-dev", "libgbm-dev"}, Installed: present("disp-kmsshot")},
	)
	return feats
}

// recommendedCompositor is a coarse arch heuristic: a weak arm board (Pi) gets
// software-friendly X11 + matchbox; a capable x86 GPU gets Wayland + labwc. The
// operator can override in the wizard.
func recommendedCompositor(arch string) string {
	switch arch {
	case "amd64", "386":
		return "wayland"
	default: // arm, arm64
		return "x11"
	}
}

// Info detects the node and returns the wizard payload. On-demand (GET
// /api/setup), so the LookPath/id probes here are off the snapshot hot path.
func (s *Setup) Info(compositor string) SetupInfo {
	if compositor != "x11" && compositor != "wayland" {
		compositor = recommendedCompositor(runtime.GOARCH)
	}
	tools := map[string]bool{}
	for name, bin := range toolBinaries {
		_, err := exec.LookPath(bin)
		tools[name] = err == nil
	}
	// disp-kmsshot may be installed at the configured path rather than on PATH.
	if s.cfg.KmsShotCmd != "" {
		if _, err := exec.LookPath(s.cfg.KmsShotCmd); err == nil {
			tools["kmsshot"] = true
		}
	}
	_, aptErr := exec.LookPath("apt-get")

	s.mu.Lock()
	installing, lastResult, logTail := s.installing, s.lastResult, s.logTail
	s.mu.Unlock()

	return SetupInfo{
		Complete:     s.state.SetupComplete(),
		Arch:         runtime.GOARCH,
		RAMMB:        ramTotalMB(),
		Compositor:   compositor,
		SeatUser:     s.cfg.SeatUser,
		SeatExists:   userExists(s.cfg.SeatUser),
		AptAvailable: aptErr == nil,
		AuthEnabled:  s.cfg.AuthKey != "",
		Tools:        tools,
		Features:     s.featureCatalog(compositor),
		Installing:   installing,
		LastResult:   lastResult,
		LogTail:      logTail,
	}
}

// packagesFor resolves a list of selected feature keys (for the given
// compositor) to the deduped union of their apt packages. Unknown keys are
// ignored; the required base is always included.
func (s *Setup) packagesFor(compositor string, keys []string) []string {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, f := range s.featureCatalog(compositor) {
		if !f.Required && !want[f.Key] {
			continue
		}
		for _, p := range f.Packages {
			if !seen[p] {
				seen[p] = true
				pkgs = append(pkgs, p)
			}
		}
	}
	sort.Strings(pkgs)
	return pkgs
}

// BeginInstall claims the install slot (so two POSTs can't both run apt) and
// returns the resolved package list. The caller MUST run RunInstall in a
// goroutine to do the work and release the slot.
func (s *Setup) BeginInstall(compositor string, keys []string) ([]string, error) {
	if _, err := exec.LookPath("apt-get"); err != nil {
		return nil, &apiError{code: 501, err: errString("apt-get not available on this node")}
	}
	pkgs := s.packagesFor(compositor, keys)
	if len(pkgs) == 0 {
		return nil, &apiError{code: 400, err: errString("no packages to install")}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installing {
		return nil, &apiError{code: 409, err: errString("an install is already running")}
	}
	s.installing = true
	s.lastResult = ""
	s.lastPkgs = pkgs
	s.logTail = "installing: " + strings.Join(pkgs, " ") + "\n"
	return pkgs, nil
}

// RunInstall performs `apt-get install <pkgs>` under nice + ionice with a
// non-interactive, minimal env (the same discipline as the Apt manager), then
// releases the slot. Best-effort per-package failures are surfaced in the log.
func (s *Setup) RunInstall(pkgs []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	args := []string{"-n", "19", "ionice", "-c3", "apt-get", "-y",
		"-o", "needrestart::mode=l",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"install"}
	args = append(args, pkgs...)
	cmd := exec.CommandContext(ctx, "nice", args...)
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DEBIAN_FRONTEND=noninteractive",
	}
	out, err := cmd.CombinedOutput()

	s.mu.Lock()
	s.installing = false
	s.logTail = tailString(string(out), 8192)
	if err != nil {
		s.lastResult = "failed"
		log.Printf("[setup] install failed: %v", err)
	} else {
		s.lastResult = "ok"
		log.Printf("[setup] installed: %s", strings.Join(pkgs, " "))
	}
	s.mu.Unlock()
}

// Finish marks the wizard complete (persisted). Idempotent.
func (s *Setup) Finish() {
	s.state.SetSetupComplete(true)
	log.Printf("[setup] marked complete")
}

// Installing reports whether an apt install is in progress (used to avoid
// tearing down the node mid-install).
func (s *Setup) Installing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installing
}

// setupURL is the loopback URL the kiosk is pointed at on a fresh (unprovisioned)
// node so the first-run wizard shows on-screen when a compositor is present.
func setupURL(cfg *Config) string {
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil || port == "" || port == "80" {
		return "http://127.0.0.1/setup"
	}
	return "http://127.0.0.1:" + port + "/setup"
}

// isSetupBootstrapMode reports whether m is the first-run wizard surface the agent
// points the kiosk at on an unprovisioned node (setupURL). That surface is a
// transient BOOTSTRAP, not an operator-chosen mode: it must never be recorded as
// the persisted "active" mode, nor counted by the provisioned-node migration.
// Otherwise a restart (the agent is Restart=always) restores /setup, the migration
// flips SetupComplete=true, and /setup then falls behind the key (setupExempt only
// holds while !SetupComplete) — so the kiosk's own request to /setup 401s and the
// node locks itself out of its own wizard.
func isSetupBootstrapMode(m Mode, cfg *Config) bool {
	if m.Type != ModeWeb {
		return false
	}
	url, _ := m.Params["url"].(string)
	return url != "" && url == setupURL(cfg)
}

// userExists reports whether a local user account exists.
func userExists(name string) bool {
	if name == "" {
		return false
	}
	_, err := runShort(5*time.Second, "id", "-u", name)
	return err == nil
}
