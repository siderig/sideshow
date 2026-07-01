package main

import (
	"fmt"
	"strings"
)

// Mode is the unit the supervisor arbitrates: exactly one owns the screen at a
// time. See docs/node-api.md §1.
type Mode struct {
	Type    string         `json:"type"`              // web | app | airplay | media | moonlight | steamlink | miracast | off
	Params  map[string]any `json:"params,omitempty"`  // type-specific
	Display string         `json:"display,omitempty"` // compositor | kms  (default compositor)
}

// Known mode types and display paths.
const (
	ModeWeb       = "web"
	ModeApp       = "app"
	ModeAirplay   = "airplay"   // AirPlay receiver (uxplay) as an X11 client
	ModeMedia     = "media"     // mpv as an X11 client
	ModeMoonlight = "moonlight" // Moonlight game/desktop-stream receiver (moonlight-qt)
	ModeSteamlink = "steamlink" // Steam Link / Steam Remote Play receiver
	ModeMiracast  = "miracast"  // Miracast/wireless-display sink (experimental; gated)
	ModeOff       = "off"

	DisplayCompositor = "compositor" // X11 client (drawn into lightdm's running Xorg session)
	DisplayWayland    = "wayland"    // labwc compositor + Chromium-Wayland, a primary on its own VT
	DisplayConsole    = "console"    // a TTY/console app on a dedicated VT (htop, a shell)
	DisplayKMS        = "kms"        // owns DRM/KMS directly on a dedicated VT (mpv --vo=drm)
)

// isWayland reports whether this is the labwc/Wayland web primary (its own
// compositor on a dedicated VT, run as root, CDP on a separate port) rather than
// the default X11 client surface.
func (m *Mode) isWayland() bool { return m.Type == ModeWeb && m.Display == DisplayWayland }

// isWaylandPrimary reports whether this mode is a labwc Wayland primary on its
// own VT — the web kiosk OR a GUI app hosted as a labwc client. It gates the
// shared compositor machinery (dedicated VT, seatd seat, GPU env), unlike
// isWayland() which is specifically the Chromium-Wayland web kiosk (CDP + the
// web launcher).
func (m *Mode) isWaylandPrimary() bool { return m.Display == DisplayWayland }

// foreground reports whether a mode owns the screen via a dedicated VT (console
// or direct-KMS) rather than as a client of the compositor. Foreground modes are
// layered on top of the persistent compositor surface via VT switching.
func (m *Mode) foreground() bool {
	return m.Display == DisplayConsole || m.Display == DisplayKMS
}

// runsAsRoot reports whether this mode's child must run as root rather than the
// seat user. Direct-KMS modes need DRM master on the mode VT (a priv-dropped
// child not in a logind session on that VT can't SET_MASTER); the legacy
// -wayland-root labwc path opens DRM via libseat's builtin backend as root.
func (m Mode) runsAsRoot(cfg *Config) bool {
	return m.Display == DisplayKMS || (m.isWaylandPrimary() && cfg.WaylandRoot)
}

// normalize fills defaults and lower-cases the type/display.
func (m *Mode) normalize() {
	m.Type = strings.ToLower(strings.TrimSpace(m.Type))
	m.Display = strings.ToLower(strings.TrimSpace(m.Display))
	if m.Display == "" {
		m.Display = DisplayCompositor
	}
	if m.Params == nil {
		m.Params = map[string]any{}
	}
}

// validate checks the request is well-formed and that v0 actually implements it.
// Returns a user-facing error (→ HTTP 400) or nil.
//
// Scope guard. Implemented: web/app on compositor; app on console (a TTY app on
// a dedicated VT — the DRM-master handoff is done via VT switching). Rejected:
//   - web on a foreground VT (Chromium is a compositor client);
//   - display=kms — the foreground VT switch works, but the child is not yet
//     bound to the mode VT / granted DRM master, so chvt would show an empty VT.
//     Gated until that's wired (and a KMS app like mpv is installed);
//   - airplay/media, which need tools disp currently lacks (uxplay's kmssink, mpv).
//
// Rejecting here (400) avoids tearing down the working mode to launch something
// that fails on the only screen.
func (m *Mode) validate() error {
	switch m.Display {
	case DisplayCompositor, DisplayWayland, DisplayConsole, DisplayKMS:
	default:
		return fmt.Errorf("display must be %q, %q, %q or %q", DisplayCompositor, DisplayWayland, DisplayConsole, DisplayKMS)
	}
	// Wayland is a labwc primary (its own VT): the web kiosk, or a GUI app hosted
	// as a labwc client. Not meaningful for console/off/receiver modes.
	if m.Display == DisplayWayland && m.Type != ModeWeb && m.Type != ModeApp {
		return fmt.Errorf("display=wayland is only valid for web or app modes")
	}
	// KMS is a foreground DRM-direct surface (its own VT, no compositor): media
	// (mpv --vo=drm), airplay (uxplay -vs kmssink), a framebuffer app, or web
	// (cog/WPE WebKit on DRM — a light kiosk with no X) — never off.
	if m.Display == DisplayKMS && m.Type != ModeMedia && m.Type != ModeAirplay && m.Type != ModeApp && m.Type != ModeWeb {
		return fmt.Errorf("display=kms is only valid for web, media, airplay, or app modes")
	}
	switch m.Type {
	case ModeWeb:
		if m.Display != DisplayCompositor && m.Display != DisplayWayland && m.Display != DisplayKMS {
			return fmt.Errorf("web mode supports display=compositor (X11), display=wayland (labwc), or display=kms (cog/WPE on the framebuffer)")
		}
		url := m.str("url")
		if url == "" {
			return fmt.Errorf("web mode requires params.url")
		}
		// Kiosk URLs are http(s) only — also defense-in-depth for the Wayland
		// launcher, which passes the URL through to a shell.
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return fmt.Errorf("web mode url must start with http:// or https://")
		}
	case ModeApp:
		if len(m.argv()) == 0 {
			return fmt.Errorf("app mode requires params.argv (non-empty array)")
		}
	case ModeAirplay:
		// AirPlay receiver (uxplay). display=compositor → an X11 client
		// (autovideosink) into the running Xorg session; display=kms → direct
		// DRM/KMS (uxplay -vs kmssink) on its own VT — the performant, backend-
		// agnostic path (no X/Wayland needed), the right choice on a Pi.
		if m.Display != DisplayCompositor && m.Display != DisplayKMS {
			return fmt.Errorf("airplay mode supports display=compositor (X11 client) or display=kms (direct framebuffer)")
		}
	case ModeMoonlight:
		// Moonlight receiver: moonlight-qt as an X11 client. With params.host it
		// streams that host directly; without, it opens the pairing GUI.
		if m.Display != DisplayCompositor {
			return fmt.Errorf("moonlight mode supports display=compositor only (moonlight-qt as an X11 client)")
		}
	case ModeSteamlink:
		// Steam Link receiver: the steamlink client as an X11 client (its own UI
		// picks/pairs the host PC). The launcher is operator-configured
		// (-steamlink-cmd) since packaging varies (apt 'steamlink' vs a Flatpak).
		if m.Display != DisplayCompositor {
			return fmt.Errorf("steamlink mode supports display=compositor only (X11 client)")
		}
	case ModeMiracast:
		// Miracast/wireless-display sink. EXPERIMENTAL and gated by -allow-miracast
		// (it can fight the node's own Wi-Fi uplink on a single-adapter box). The
		// cfg-dependent allow check lives in modeCommand (it has the Config).
		if m.Display != DisplayCompositor {
			return fmt.Errorf("miracast mode supports display=compositor only")
		}
	case ModeMedia:
		// Native media (mpv). display=compositor → an X11 client into the running
		// Xorg session; display=kms → direct DRM (mpv --vo=drm) on its own VT (no
		// compositor). wayland/console are rejected.
		if m.Display != DisplayCompositor && m.Display != DisplayKMS {
			return fmt.Errorf("media mode supports display=compositor (X11 client) or display=kms (mpv --vo=drm)")
		}
		url, path := m.str("url"), m.str("path")
		if (url == "") == (path == "") {
			return fmt.Errorf("media mode requires exactly one of params.url or params.path")
		}
		if url != "" && !isMediaURL(url) {
			return fmt.Errorf("media mode url must be http(s)://, rtsp://, rtmp:// or file://")
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return fmt.Errorf("media mode path must be absolute")
		}
	case ModeOff:
		// nothing to validate
	case "":
		return fmt.Errorf("missing mode.type")
	default:
		return fmt.Errorf("unknown mode.type %q", m.Type)
	}
	return nil
}

// copyParams returns a shallow value-copy of a params map (nil-safe). The stored
// values are scalars/strings/argv slices that are replaced wholesale rather than
// mutated element-wise, so a one-level copy is enough to hand the JSON encoder a
// snapshot it can iterate without holding the runner lock.
func copyParams(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// str returns a string param or "".
func (m *Mode) str(key string) string {
	if v, ok := m.Params[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// boolOr returns a bool param or the default.
func (m *Mode) boolOr(key string, def bool) bool {
	if v, ok := m.Params[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// argv returns params.argv as a []string (JSON decodes arrays as []any).
func (m *Mode) argv() []string {
	v, ok := m.Params["argv"]
	if !ok {
		return nil
	}
	switch a := v.(type) {
	case []string:
		return a
	case []any:
		out := make([]string, 0, len(a))
		for _, e := range a {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// isMediaURL reports whether u is a media source scheme mpv can stream.
func isMediaURL(u string) bool {
	for _, p := range []string{"http://", "https://", "rtsp://", "rtmp://", "file://"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// usesChrome reports whether this mode drives Chromium over CDP. The framebuffer
// web backend (web + display=kms) is cog/WPE WebKit, which has no CDP — so it is
// NOT chrome-driven and gets none of the agent's CDP controls, nor the
// watchdog's CDP-wedged restart path.
func (m *Mode) usesChrome() bool { return m.Type == ModeWeb && m.Display != DisplayKMS }

// isCogKiosk reports whether this is the cog kiosk (web on the DRM/KMS
// framebuffer). cog runs directly (no CDP); the agent controls it in place over
// its D-Bus interface (cogctl open/reload). Mutually exclusive with usesChrome.
func (m *Mode) isCogKiosk() bool { return m.Type == ModeWeb && m.Display == DisplayKMS }

// equivalent reports whether switching from a→b needs no child restart.
// Same web type+display but a different URL is handled by an in-place CDP
// re-navigate, so it is NOT "equivalent" here (equivalent == truly identical).
func (a Mode) equivalent(b Mode) bool {
	if a.Type != b.Type || a.Display != b.Display {
		return false
	}
	switch a.Type {
	case ModeWeb:
		return a.str("url") == b.str("url") && a.boolOr("dark", true) == b.boolOr("dark", true)
	case ModeMedia:
		return a.str("url") == b.str("url") &&
			a.str("path") == b.str("path") &&
			a.boolOr("loop", true) == b.boolOr("loop", true) &&
			a.boolOr("mute", false) == b.boolOr("mute", false)
	case ModeAirplay:
		return a.str("name") == b.str("name") && a.boolOr("audio", false) == b.boolOr("audio", false)
	case ModeMoonlight:
		return a.str("host") == b.str("host") && a.str("app") == b.str("app")
	case ModeSteamlink:
		return true
	case ModeMiracast:
		return true
	case ModeOff:
		return true
	default:
		return fmt.Sprint(a.argv()) == fmt.Sprint(b.argv())
	}
}

// label is a short human/process label for logs.
func (m Mode) label() string {
	switch m.Type {
	case ModeWeb:
		return "web:" + m.str("url")
	case ModeApp:
		av := m.argv()
		if len(av) > 0 {
			return "app:" + av[0]
		}
		return "app"
	case ModeMedia:
		if u := m.str("url"); u != "" {
			return "media:" + u
		}
		return "media:" + m.str("path")
	case ModeAirplay:
		return "airplay"
	case ModeMoonlight:
		if h := m.str("host"); h != "" {
			return "moonlight:" + h
		}
		return "moonlight"
	case ModeSteamlink:
		return "steamlink"
	case ModeMiracast:
		return "miracast"
	default:
		return m.Type
	}
}
