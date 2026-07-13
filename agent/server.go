package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Server is the local control surface: webUI on GET / and JSON on /api/*.
type Server struct {
	cfg       *Config
	sup       *Supervisor
	stats     *Stats
	apt       *Apt
	cec       *CEC
	vnc       *VNC
	display   *Display
	power     *Power
	content   *Content
	plymouth  *Plymouth
	state     *State
	library   *Library
	playlist  *Playlist
	actions   *Actions
	miracast  *Miracast
	net       *Net
	setup     *Setup
	ts        *Tailscale
	tlsm      *TLSManager
	scheduler *Scheduler
	mem       *Memory
	tz        *Timezone
	mux       *http.ServeMux
}

func NewServer(cfg *Config, sup *Supervisor, stats *Stats, apt *Apt, cec *CEC, vnc *VNC, display *Display, power *Power, content *Content, plymouth *Plymouth, state *State, library *Library, playlist *Playlist, actions *Actions, miracast *Miracast, netmgr *Net, setup *Setup, ts *Tailscale) *Server {
	s := &Server{cfg: cfg, sup: sup, stats: stats, apt: apt, cec: cec, vnc: vnc, display: display, power: power, content: content, plymouth: plymouth, state: state, library: library, playlist: playlist, actions: actions, miracast: miracast, net: netmgr, setup: setup, ts: ts, mem: NewMemory(cfg), tz: NewTimezone(), mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/auth", s.handleAuth)
	s.mux.HandleFunc("/api/logout", s.handleLogout)
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/standby", s.handleStandby)
	s.mux.HandleFunc("/api/reboot", s.handleReboot)
	s.mux.HandleFunc("/api/shutdown", s.handleShutdown)
	s.mux.HandleFunc("/api/restart", s.handleRestart)
	s.mux.HandleFunc("/api/status", s.handleStatus)
	s.mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/history", s.handleHistory)
	s.mux.HandleFunc("/api/mode", s.handleMode)
	s.mux.HandleFunc("/api/url", s.handleURL)
	s.mux.HandleFunc("/api/media", s.handleMedia)
	s.mux.HandleFunc("/api/custom", s.handleCustom)
	s.mux.HandleFunc("/api/custom/delete", s.handleCustomDelete)
	s.mux.HandleFunc("/api/custom/launch", s.handleCustomLaunch)
	s.mux.HandleFunc("/api/actions", s.handleActions)
	s.mux.HandleFunc("/api/actions/delete", s.handleActionDelete)
	s.mux.HandleFunc("/api/actions/reorder", s.handleActionReorder)
	s.mux.HandleFunc("/api/action/", s.handleActionFire)
	s.mux.HandleFunc("/api/miracast", s.handleMiracast)
	s.mux.HandleFunc("/api/hostname", s.handleHostname)
	s.mux.HandleFunc("/api/timezone", s.handleTimezone)
	s.mux.HandleFunc("/api/timezone/detect", s.handleTimezoneDetect)
	s.mux.HandleFunc("/api/wifi", s.handleWifi)
	s.mux.HandleFunc("/api/wifi/delete", s.handleWifiForget)
	s.mux.HandleFunc("/api/tailscale", s.handleTailscale)
	s.mux.HandleFunc("/api/tailscale/up", s.handleTailscaleUp)
	s.mux.HandleFunc("/api/tailscale/down", s.handleTailscaleDown)
	s.mux.HandleFunc("/api/tailscale/serve", s.handleTailscaleServe)
	s.mux.HandleFunc("/api/tls", s.handleTLS)
	s.mux.HandleFunc("/api/tls/regenerate", s.handleTLSRegenerate)
	s.mux.HandleFunc("/api/input", s.handleInput)
	s.mux.HandleFunc("/setup", s.handleSetupPage)
	s.mux.HandleFunc("/api/setup", s.handleSetup)
	s.mux.HandleFunc("/api/setup/install", s.handleSetupInstall)
	s.mux.HandleFunc("/api/setup/finish", s.handleSetupFinish)
	s.mux.HandleFunc("/api/theme", s.handleTheme)
	s.mux.HandleFunc("/api/rotate", s.handleRotate)
	s.mux.HandleFunc("/api/layout", s.handleLayout)
	s.mux.HandleFunc("/api/outputs", s.handleOutputs)
	s.mux.HandleFunc("/api/outputs/content", s.handleOutputContent)
	s.mux.HandleFunc("/api/zoom", s.handleZoom)
	s.mux.HandleFunc("/api/schedule/week", s.handleScheduleWeek)
	s.mux.HandleFunc("/api/stats", s.handleStats)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
	s.mux.HandleFunc("/api/upgrade", s.handleUpgrade)
	s.mux.HandleFunc("/api/document", s.handleDocument)
	s.mux.HandleFunc("/api/reload", s.handleReload)
	s.mux.HandleFunc("/api/message", s.handleMessage)
	s.mux.HandleFunc("/api/cec", s.handleCEC)
	s.mux.HandleFunc("/api/screen", s.handleScreen)
	s.mux.HandleFunc("/api/plymouth", s.handlePlymouth)
	s.mux.HandleFunc("/api/memory", s.handleMemory)
	s.mux.HandleFunc("/api/vnc", s.handleVNC)
	s.mux.HandleFunc("/api/screenshot", s.handleScreenshot)
	s.mux.HandleFunc("/api/library", s.handleLibrary)
	s.mux.HandleFunc("/api/library/upload", s.handleLibraryUpload)
	s.mux.HandleFunc("/api/library/mkdir", s.handleLibraryMkdir)
	s.mux.HandleFunc("/api/library/rename", s.handleLibraryRename)
	s.mux.HandleFunc("/api/library/delete", s.handleLibraryDelete)
	s.mux.HandleFunc("/media/", s.handleMediaFile)
	s.mux.HandleFunc("/api/playlist-media", s.handlePlaylistMedia)
	s.mux.HandleFunc("/show", s.handleShowPage)
	s.mux.HandleFunc("/docfs/", s.handleDocFS)
	// Live screen view: the WS bridge (most specific) wins over the static noVNC
	// viewer subtree; ServeMux 301s "/vnc" → "/vnc/".
	s.mux.HandleFunc("/vnc/ws", s.vnc.HandleWS)
	if sub, err := fs.Sub(novncFS, "web/novnc"); err == nil {
		s.mux.Handle("/vnc/", http.StripPrefix("/vnc/", http.FileServer(http.FS(sub))))
	}
	s.mux.HandleFunc("/", s.handleIndex)
	s.scheduler = NewScheduler(cfg, s.fireScheduled)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL.Path)
	if s.authEnabled() && !authExempt(r.Method, r.URL.Path) && !kioskLocalExempt(r) && !s.setupExempt(r) && !s.authed(r) {
		writeErr(w, &apiError{code: 401, err: fmt.Errorf("unauthorized: unlock with your key")})
		return
	}
	s.mux.ServeHTTP(w, r)
}

// kioskLocalExempt lets the locally-running kiosk fetch the document/media
// viewer surfaces it is pointed at without an auth cookie (it has none — it's a
// CDP-driven Chromium). Scoped to loopback so it never widens LAN access: only a
// process on this host can reach these without the key.
func kioskLocalExempt(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/docfs/") {
		return true
	}
	if strings.HasPrefix(p, "/media/") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		return true // the kiosk / playlist player fetches uploaded media over loopback
	}
	if p == "/show" && r.Method == http.MethodGet {
		return true // the media-playlist player page
	}
	if p == "/api/playlist-media" && r.Method == http.MethodGet {
		return true // the /show player fetches its playlist config
	}
	return false
}

// setupExempt opens the first-run setup surface (/setup + /api/setup*) WITHOUT
// the key — but ONLY while the node is not yet provisioned (!SetupComplete). This
// is the bootstrap window on a fresh node: the operator has not entered a key yet
// and the LAN is the trust boundary (same model as the plain-HTTP auth). Once the
// wizard finishes, SetupComplete flips and this surface is gated like everything
// else, closing the pre-auth hole.
func (s *Server) setupExempt(r *http.Request) bool {
	if s.state.SetupComplete() {
		return false
	}
	switch r.URL.Path {
	case "/setup", "/api/setup", "/api/setup/install", "/api/setup/finish":
		return true
	}
	// NOTE: the Tailscale/TLS endpoints are deliberately NOT exempt. Joining a
	// tailnet or toggling TLS is security-sensitive and must always require the
	// key: on a keyed node the wizard's encryption panel needs an unlock, and on an
	// unkeyed node these are open anyway (auth is off). Exempting them would let an
	// unauthenticated LAN client enroll a not-yet-provisioned node into their own
	// tailnet — persistent access surviving setup.
	return false
}

// ---- API types (docs/node-api.md §2) ----

type ModeStatus struct {
	Type       string         `json:"type"`
	Params     map[string]any `json:"params,omitempty"`
	Display    string         `json:"display,omitempty"`
	State      string         `json:"state"`
	Since      string         `json:"since,omitempty"`
	Restarts   int            `json:"restarts"`
	LastErr    string         `json:"last_error,omitempty"`
	Background string         `json:"background,omitempty"` // suspended compositor mode behind a foreground VT mode

	PID     int  `json:"-"` // surfaced via StatusResponse.Child
	Running bool `json:"-"`
}

type childInfo struct {
	PID     int  `json:"pid"`
	Running bool `json:"running"`
}

type cdpInfo struct {
	Attached bool   `json:"attached"`
	URL      string `json:"url,omitempty"`
}

type StatusResponse struct {
	Node    string     `json:"node"`
	Label   string     `json:"label,omitempty"` // human label for the fleet view
	Group   string     `json:"group,omitempty"` // group/site
	Time    string     `json:"time"`
	UptimeS int64      `json:"uptime_s"`
	Mode    ModeStatus `json:"mode"`
	Child   childInfo  `json:"child"`
	CDP     cdpInfo    `json:"cdp"`
	Health  string     `json:"health"`
	Auth    bool       `json:"auth"` // the control surface is key-protected
}

// Snapshot is the single aggregated document the webUI polls (GET /api/snapshot)
// so the control surface has one source of truth and one request per tick
// instead of a fan-out of per-feature polls (heavy on a weak node). It folds in
// the cheap, cached Info() of every manager; the multi-output view
// (/api/outputs, an xrandr fork) stays a separate, slower poll.
type Snapshot struct {
	Node    string     `json:"node"`
	Label   string     `json:"label,omitempty"`
	Group   string     `json:"group,omitempty"`
	Time    string     `json:"time"`
	UptimeS int64      `json:"uptime_s"`
	Auth    bool       `json:"auth"`
	Health  string     `json:"health"`
	Mode    ModeStatus `json:"mode"`
	Child   childInfo  `json:"child"`
	CDP     cdpInfo    `json:"cdp"`

	Stats         SysStats           `json:"stats"`
	Display       DisplayInfo        `json:"display"`
	Content       ContentInfo        `json:"content"`
	Document      DocumentInfo       `json:"document"`
	CEC           CECInfo            `json:"cec"`
	VNC           VNCStatus          `json:"vnc"`
	Memory        MemoryStatus       `json:"memory"`
	Plymouth      PlymouthInfo       `json:"plymouth"`
	State         StateInfo          `json:"state"`
	Playlist      PlaylistSummary    `json:"playlist"`
	Actions       []Action           `json:"actions"`
	CurrentAction string             `json:"current_action,omitempty"`
	Miracast      MiracastInfo       `json:"miracast"`
	Net           NetInfo            `json:"net"`
	Timezone      TimezoneInfo       `json:"timezone"`
	Tailscale     TailscaleInfo      `json:"tailscale"`
	TLS           TLSInfo            `json:"tls"`
	Input         InputInfo          `json:"input"`
	ScheduleWeek  WeeklyScheduleInfo `json:"schedule_week"`
	Caps          SnapshotCaps       `json:"caps"`
}

// SnapshotCaps are agent-level gates the UI needs to show or hide controls,
// beyond the per-manager `supported` flags already carried in the blocks above.
type SnapshotCaps struct {
	Shutdown bool `json:"shutdown"`
	Miracast bool `json:"miracast"`
}

// InputInfo is the node-global local-input policy (snapshot + GET /api/input).
type InputInfo struct {
	Allowed   bool `json:"allowed"`   // does the local keyboard/mouse drive the kiosk?
	Supported bool `json:"supported"` // the agent can enforce it (running as root)
}

// apiError carries an HTTP status with the error.
type apiError struct {
	code int
	err  error
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) status() int {
	if e.code == 0 {
		return 500
	}
	return e.code
}

// ---- handlers ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ms := s.sup.Status()
	cdp := cdpInfo{Attached: s.sup.chrome.Attached()} // read once → consistent pair
	if cdp.Attached {
		cdp.URL = s.sup.chrome.endpoint()
	}
	resp := StatusResponse{
		Node:    s.net.Hostname(),
		Label:   s.cfg.NodeLabel,
		Group:   s.cfg.NodeGroup,
		Time:    time.Now().UTC().Format(time.RFC3339),
		UptimeS: int64(time.Since(s.sup.startedAt).Seconds()),
		Mode:    ms,
		Child:   childInfo{PID: ms.PID, Running: ms.Running},
		CDP:     cdp,
		Health:  health(ms),
		Auth:    s.authEnabled(),
	}
	writeJSON(w, 200, resp)
}

// handleSnapshot returns the single aggregated control-surface document — mode,
// stats, display, content, caps, and the persisted state/history — so the webUI
// can render and refresh from one request per tick.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	ms := s.sup.Status()
	cdp := cdpInfo{Attached: s.sup.chrome.Attached()} // read once → consistent pair
	if cdp.Attached {
		cdp.URL = s.sup.chrome.endpoint()
	}
	curMode := Mode{Type: ms.Type, Display: ms.Display, Params: ms.Params}
	writeJSON(w, 200, Snapshot{
		Node:    s.net.Hostname(),
		Label:   s.cfg.NodeLabel,
		Group:   s.cfg.NodeGroup,
		Time:    time.Now().UTC().Format(time.RFC3339),
		UptimeS: int64(time.Since(s.sup.startedAt).Seconds()),
		Auth:    s.authEnabled(),
		Health:  health(ms),
		Mode:    ms,
		Child:   childInfo{PID: ms.PID, Running: ms.Running},
		CDP:     cdp,

		Stats:         s.stats.Snapshot(),
		Display:       s.display.Info(),
		Content:       s.content.Info(),
		Document:      s.content.DocInfo(),
		CEC:           s.cec.Info(),
		VNC:           s.vnc.Status(),
		Memory:        s.mem.Status(),
		Plymouth:      s.plymouth.Info(),
		State:         s.state.Info(),
		Playlist:      s.playlist.Summary(),
		Actions:       s.actions.List(),
		CurrentAction: s.actions.CurrentSlug(curMode),
		Miracast:      s.miracast.Info(),
		Net:           s.net.Info(),
		Timezone:      s.tz.Info(),
		Tailscale:     s.ts.Info(),
		TLS:           s.tlsInfo(),
		Input:         s.inputInfo(),
		ScheduleWeek:  s.scheduler.Info(),
		Caps:          SnapshotCaps{Shutdown: s.cfg.AllowShutdown, Miracast: s.cfg.AllowMiracast},
	})
}

// handleState returns the persisted active mode + setup flag + history.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.state.Info())
}

// handleCustom lists (GET) or saves (POST) custom modes — named launchers for an
// arbitrary program. `POST {"id"?,"name","command","args":[],"display","env":{}}`.
func (s *Server) handleCustom(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"custom_modes": s.state.Info().CustomModes})
	case http.MethodPost:
		var cm CustomMode
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&cm); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		saved, err := s.state.UpsertCustom(cm)
		if err != nil {
			writeErr(w, &apiError{code: 400, err: err})
			return
		}
		writeJSON(w, 200, saved)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleCustomDelete removes a saved custom mode. `POST {"id":"…"}`.
func (s *Server) handleCustomDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct{ ID string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if !s.state.DeleteCustom(body.ID) {
		writeErr(w, &apiError{code: 404, err: fmt.Errorf("no custom mode %q", body.ID)})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleCustomLaunch switches to a saved custom mode. `POST {"id":"…"}`. The
// root gate (display=kms) and app-mode validation are enforced by the switch.
func (s *Server) handleCustomLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct{ ID string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	cm, ok := s.state.GetCustom(body.ID)
	if !ok {
		writeErr(w, &apiError{code: 404, err: fmt.Errorf("no custom mode %q", body.ID)})
		return
	}
	if err := s.sup.Switch(cm.mode()); err != nil {
		writeErr(w, err)
		return
	}
	s.recordActive()
	writeJSON(w, 200, s.sup.Status())
}

// ---- saved actions (docs/node-api.md §2) ----

// handleActions lists (GET) or saves (POST) saved actions — named, slugged,
// fireable launchers that wrap any mode. POST body is an Action
// `{id?,slug?,name,mode:{type,params,display}}`; the mode is validated so an
// un-fireable action can't be saved.
func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.actions.Info())
	case http.MethodPost:
		var a Action
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&a); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		saved, err := s.actions.Upsert(a)
		if err != nil {
			writeErr(w, &apiError{code: 400, err: err})
			return
		}
		writeJSON(w, 200, saved)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleActionDelete removes a saved action by slug or id. `POST {"slug"|"id":"…"}`.
func (s *Server) handleActionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Slug string `json:"slug"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	key := body.Slug
	if key == "" {
		key = body.ID
	}
	if !s.actions.Delete(key) {
		writeErr(w, &apiError{code: 404, err: fmt.Errorf("no action %q", key)})
		return
	}
	writeJSON(w, 200, s.actions.Info())
}

// handleActionReorder sets the display/fire order. `POST {"order":["slug-a",…]}`.
func (s *Server) handleActionReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	s.actions.Reorder(body.Order)
	writeJSON(w, 200, s.actions.Info())
}

// handleActionFire switches to a saved action by slug: `POST /api/action/<slug>`.
// The Stream-Deck-friendly entry point — send the shared key as an X-Sideshow-Key
// (or Authorization: Bearer) header. Fire = sup.Switch(action.Mode), so it
// inherits the supervisor's validation (400), switch-in-progress (409), and
// launch (500) errors as-is.
func (s *Server) handleActionFire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/action/"), "/")
	if slug == "" {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing action slug (POST /api/action/<slug>)")})
		return
	}
	a, ok := s.actions.Get(slug)
	if !ok {
		writeErr(w, &apiError{code: 404, err: fmt.Errorf("no action %q", slug)})
		return
	}
	if err := s.runAction(a); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.sup.Status())
}

// runAction puts a saved action on screen: a web action with no explicit backend
// (notably a playlist action → /show) inherits the node's current web backend
// (X11 vs Wayland) so it works on both, and content owners are cleared first so a
// timer can't navigate away from it. Shared by the fire endpoint + the scheduler.
func (s *Server) runAction(a Action) error {
	mode := a.Mode
	if mode.Type == ModeWeb && mode.Display == "" {
		if cur := s.sup.Status(); cur.Type == ModeWeb && cur.Display != "" {
			mode.Display = cur.Display
		}
	}
	s.content.DisableOwners()
	if err := s.sup.Switch(mode); err != nil {
		return err
	}
	s.recordActive()
	return nil
}

// fireScheduled is the weekly scheduler's actuator: "@sleep" powers the display
// off, "@wake" powers it back on without changing the mode (both are what the
// nightly window synthesizes); any other value is a saved action slug — wake the
// display if a prior @sleep put it down, then run the action.
func (s *Server) fireScheduled(action string) error {
	switch action {
	case scheduleSleepAction:
		if s.display.Asleep() {
			return nil // already off — don't re-issue standby/CEC on a redundant transition
		}
		return s.display.Screen("", false)
	case scheduleWakeAction:
		if !s.display.Asleep() {
			return nil // already on — don't re-run xrandr / yank a CEC TV's input on an awake screen
		}
		return s.display.Screen("", true)
	}
	a, ok := s.actions.Get(action)
	if !ok {
		return fmt.Errorf("scheduled action %q not found", action)
	}
	if s.display.Info().Asleep {
		_ = s.display.Screen("", true) // undo a prior scheduled @sleep before showing content
	}
	return s.runAction(a)
}

// StartScheduler launches the weekly-schedule enforcement loop (see weekly.go).
func (s *Server) StartScheduler() { s.scheduler.Start() }

// MigrateNightly folds a legacy nightly sleep/wake window (from display.json) into
// the unified scheduler once, before StartScheduler. See Scheduler.MigrateNightly.
func (s *Server) MigrateNightly(enabled bool, sleep, wake string) {
	s.scheduler.MigrateNightly(enabled, sleep, wake)
}

// handleScheduleWeek returns (GET) or sets (POST) the weekly schedule.
func (s *Server) handleScheduleWeek(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.scheduler.Info())
	case http.MethodPost:
		var info WeeklyScheduleInfo
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&info); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		out, err := s.scheduler.Set(info)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, out)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleMiracast returns (GET) or sets (POST) the Miracast safety config: the P2P
// interface pin, the session time-box, and the uplink auto-abort. The hard
// -allow-miracast deploy-time gate is reported (allowed) but not settable here.
func (s *Server) handleMiracast(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.miracast.Info())
	case http.MethodPost:
		var body struct {
			Iface       string `json:"iface"`
			MaxMinutes  int    `json:"max_minutes"`
			AbortAfterS int    `json:"abort_after_s"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		writeJSON(w, 200, s.miracast.Set(body.Iface, body.MaxMinutes, body.AbortAfterS))
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleHostname returns (GET) the node-identity info, or renames the node
// (POST {"name":"…"}). A rename is refused for a load-bearing (protected)
// hostname and validated as an RFC-1123 label; it takes effect live (the header
// updates on the next poll) — no agent restart needed.
func (s *Server) handleHostname(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.net.Info())
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		info, err := s.net.SetHostname(body.Name)
		if err != nil {
			writeErr(w, &apiError{code: 400, err: err})
			return
		}
		writeJSON(w, 200, info)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleTimezone returns (GET) the current zone + settable flag + the full
// picker list (the list forks timedatectl, so it is served here on demand, not
// in the snapshot), or sets the zone (POST {"zone":"…"}). A set takes effect
// system-wide immediately; the agent's own scheduler adopts the new zone at its
// next restart (Go caches the process's local zone).
func (s *Server) handleTimezone(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.tz.Full())
	case http.MethodPost:
		var body struct {
			Zone string `json:"zone"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		info, err := s.tz.Set(body.Zone)
		if err != nil {
			writeErr(w, &apiError{code: 400, err: err})
			return
		}
		writeJSON(w, 200, info)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleTimezoneDetect guesses the node's zone from its public IP via a public
// geolocation service (POST). Opt-in — the operator triggers it, and the guess
// is only suggested to the UI, never applied. Returns {"zone":"…"}.
func (s *Server) handleTimezoneDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	zone, err := s.tz.Detect(r.Context())
	if err != nil {
		writeErr(w, &apiError{code: 502, err: err})
		return
	}
	writeJSON(w, 200, map[string]string{"zone": zone})
}

// handleWifi lists Wi-Fi state (GET — a live nmcli scan + saved networks) or
// joins a network (POST {"ssid":"…","psk":"…"}). PSKs are never returned. The
// scan is on-demand (not in the snapshot) since it forks nmcli.
func (s *Server) handleWifi(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.net.WifiStatus())
	case http.MethodPost:
		var body struct {
			SSID   string `json:"ssid"`
			PSK    string `json:"psk"`
			Hidden bool   `json:"hidden"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		if err := s.net.WifiConnect(body.SSID, body.PSK, body.Hidden); err != nil {
			writeErr(w, &apiError{code: 400, err: err})
			return
		}
		writeJSON(w, 200, s.net.WifiStatus())
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleWifiForget deletes a saved Wi-Fi connection. POST {"ssid":"…"}.
func (s *Server) handleWifiForget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		SSID string `json:"ssid"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.net.WifiForget(body.SSID); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, s.net.WifiStatus())
}

// ---- encryption: Tailscale overlay + self-signed HTTPS (both opt-in) ----

// SetTLS links the self-signed-HTTPS manager after construction (it wraps the
// server as its handler, so it can't be built before the server exists).
func (s *Server) SetTLS(m *TLSManager) { s.tlsm = m }

// tlsInfo is the snapshot's self-signed-HTTPS block, guarding the brief
// pre-SetTLS window (SetTLS runs right after NewServer, before serving starts).
func (s *Server) tlsInfo() TLSInfo {
	if s.tlsm == nil {
		return TLSInfo{HTTPSAddr: s.cfg.HTTPSAddr}
	}
	return s.tlsm.Info()
}

// handleTailscale reports overlay status (GET). Auth keys are write-only (POST
// /api/tailscale/up) and never returned here.
func (s *Server) handleTailscale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET")})
		return
	}
	writeJSON(w, 200, s.ts.Status())
}

// handleTailscaleUp joins a tailnet. POST {"authkey":"…","ssh":bool,"serve":bool}.
func (s *Server) handleTailscaleUp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		AuthKey string `json:"authkey"`
		SSH     bool   `json:"ssh"`
		Serve   bool   `json:"serve"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.ts.Up(body.AuthKey, body.SSH, body.Serve); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, s.ts.Info())
}

// handleTailscaleDown leaves the tailnet (POST) — the clean opt-out.
func (s *Server) handleTailscaleDown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if err := s.ts.Down(); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, s.ts.Info())
}

// handleTailscaleServe toggles the ts.net HTTPS front. POST {"enabled":bool}.
func (s *Server) handleTailscaleServe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.ts.SetServe(body.Enabled); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, s.ts.Info())
}

// handleTLS reports (GET) or toggles (POST {"enabled":bool}) the opt-in
// self-signed HTTPS listener.
func (s *Server) handleTLS(w http.ResponseWriter, r *http.Request) {
	if s.tlsm == nil {
		writeErr(w, &apiError{code: 503, err: fmt.Errorf("TLS manager not ready")})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.tlsm.Info())
	case http.MethodPost:
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		var err error
		if body.Enabled {
			err = s.tlsm.Enable()
		} else {
			err = s.tlsm.Disable()
		}
		if err != nil {
			writeErr(w, &apiError{code: 400, err: err})
			return
		}
		writeJSON(w, 200, s.tlsm.Info())
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleTLSRegenerate replaces the self-signed cert with a fresh one (POST).
func (s *Server) handleTLSRegenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if s.tlsm == nil {
		writeErr(w, &apiError{code: 503, err: fmt.Errorf("TLS manager not ready")})
		return
	}
	if err := s.tlsm.Regenerate(); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, s.tlsm.Info())
}

// inputInfo reports the node-global local-input policy for the snapshot + GET
// /api/input: whether local input reaches the kiosk, and whether the agent can
// actually enforce it (it needs root to write the udev/Xorg rules).
func (s *Server) inputInfo() InputInfo {
	return InputInfo{
		Allowed:   s.state.LocalInputAllowed(!s.cfg.NoLocalInput),
		Supported: os.Geteuid() == 0,
	}
}

// handleInput returns (GET) or sets (POST) whether the physical keyboard/mouse
// controls the kiosk. `POST {"allowed":bool}`. The persisted policy is the source
// of truth (re-applied at boot); remote control (VNC/panel) is unaffected. A change
// is applied live on a Wayland node by restarting the compositor (labwc
// re-enumerates input); on an agent-owned-X node the Xorg rule takes effect on the
// next agent restart/reboot (reported via applied_live:false).
func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.inputInfo())
	case http.MethodPost:
		var body struct {
			Allowed *bool `json:"allowed"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		if body.Allowed == nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing allowed (bool)")})
			return
		}
		if os.Geteuid() != 0 {
			writeErr(w, &apiError{code: 501, err: fmt.Errorf("local-input control needs the agent to run as root")})
			return
		}
		allowed := *body.Allowed
		s.state.SetLocalInput(allowed)
		changed := ApplyNoLocalInput(s.cfg, !allowed)
		// Apply live where we can: on a Wayland node the running compositor IS the
		// mode, so RestartMode() relaunches labwc and it re-enumerates input. On an
		// agent-owned Xorg node the xorg.conf.d rule needs an Xorg restart — deferred
		// to the next agent restart/reboot (the persisted policy is applied at boot).
		// Run the live apply SYNCHRONOUSLY so applied_live reflects what actually
		// happened (a concurrent switch makes RestartMode 409). It blocks the request
		// for the ~3–5s compositor restart — acceptable for a deliberate toggle.
		appliedLive := false
		if changed && s.sup.Status().Display == DisplayWayland {
			if err := s.sup.RestartMode(); err != nil {
				log.Printf("[input] live apply (RestartMode) failed: %v", err)
			} else {
				appliedLive = true
			}
		}
		writeJSON(w, 200, map[string]any{
			"allowed":      allowed,
			"supported":    true,
			"applied_live": appliedLive,
			"changed":      changed,
		})
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleSetupPage serves the first-run wizard. Reachable pre-auth only while
// !SetupComplete (see setupExempt); afterward it is gated like any other page.
func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(setupHTML)
}

// handleSetup returns the wizard detection payload (GET, optional ?compositor=).
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET")})
		return
	}
	writeJSON(w, 200, s.setup.Info(r.URL.Query().Get("compositor")))
}

// handleSetupInstall starts a background apt install of the selected feature
// packages. POST {"compositor":"x11|wayland","features":["airplay",…]}.
func (s *Server) handleSetupInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Compositor string   `json:"compositor"`
		Features   []string `json:"features"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	pkgs, err := s.setup.BeginInstall(body.Compositor, body.Features)
	if err != nil {
		writeErr(w, err)
		return
	}
	go s.setup.RunInstall(pkgs)
	writeJSON(w, 200, map[string]any{"ok": true, "installing": pkgs})
}

// handleSetupFinish marks the wizard complete (SetSetupComplete). POST.
func (s *Server) handleSetupFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if s.setup.Installing() {
		writeErr(w, &apiError{code: 409, err: fmt.Errorf("an install is still running")})
		return
	}
	s.setup.Finish()
	writeJSON(w, 200, map[string]any{"ok": true, "complete": true})
}

// handleHistory lists the shown-content history (GET) or edits it (POST):
// `{"delete":"<id>"}` removes one entry; `{"clear":true}` empties it.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"history": s.state.Info().History})
	case http.MethodPost:
		var body struct {
			Delete string `json:"delete"`
			Clear  bool   `json:"clear"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		switch {
		case body.Clear:
			s.state.ClearHistory()
		case body.Delete != "":
			if !s.state.DeleteHistory(body.Delete) {
				writeErr(w, &apiError{code: 404, err: fmt.Errorf("no history entry %q", body.Delete)})
				return
			}
		default:
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("specify delete:<id> or clear:true")})
			return
		}
		writeJSON(w, 200, map[string]any{"history": s.state.Info().History})
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleHealth is a bare liveness probe for an external monitor/heartbeat. It is
// auth-exempt (no sensitive data) and returns 503 when the mode is down.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ms := s.sup.Status()
	h := health(ms)
	code := 200
	if h == "down" {
		code = 503
	}
	writeJSON(w, code, map[string]any{
		"status": h, "mode": ms.Type, "state": ms.State,
		"uptime_s": int64(time.Since(s.sup.startedAt).Seconds()),
	})
}

func (s *Server) handleStandby(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if err := s.power.Standby(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "standby": true})
}

func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	s.power.Reboot()
	writeJSON(w, 202, map[string]any{"ok": true, "rebooting": true})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if err := s.power.Shutdown(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true, "shutting_down": true})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if err := s.sup.RestartMode(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.sup.Status())
}

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var m Mode
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&m); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.sup.Switch(m); err != nil {
		writeErr(w, err)
		return
	}
	s.recordActive()
	writeJSON(w, 200, s.sup.Status())
}

// modeFromStatus rebuilds a Mode from the supervisor's live ModeStatus. Status
// already deep-copies Params under the runner lock, so the result is safe to hand
// to State.RecordMode without racing the runner's in-place setURL/setDark.
func modeFromStatus(ms ModeStatus) Mode {
	return Mode{Type: ms.Type, Display: ms.Display, Params: ms.Params}
}

// recordActive persists whatever mode is now on screen as the node's active mode
// (so a reboot restores it). It reads the live Status — the single source of
// truth every mode change funnels into — rather than a switch input, so
// output-content routing and in-place URL re-navigations record correctly too.
// Deliberately NOT called for transient stops (power.Standby) so a reboot after
// standby comes back to the last real content rather than a black screen.
func (s *Server) recordActive() {
	m := modeFromStatus(s.sup.Status())
	// The first-run wizard is a transient bootstrap surface, never an operator
	// choice — persisting it would let a restart restore /setup and the
	// provisioned-node migration then lock the node out of its own wizard.
	if isSetupBootstrapMode(m, s.cfg) {
		return
	}
	s.state.RecordMode(m)
}

// handleURL is sugar for switching to / re-navigating a web mode.
func (s *Server) handleURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		URL  string `json:"url"`
		Dark *bool  `json:"dark"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if body.URL == "" {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing url")})
		return
	}
	dark := true
	if body.Dark != nil {
		dark = *body.Dark
	}
	// "Change the URL" must not change the compositor backend: inherit the
	// current web mode's display (so a Wayland node stays Wayland) rather than
	// falling back to the compositor/X11 default — which is broken on an X-less
	// node. Falls through to the default only when not already in a web mode.
	// A direct URL change is no content owner's job — clear the active document
	// viewer so a timer doesn't re-assert old content over the new URL.
	s.content.DisableOwners()
	m := Mode{Type: ModeWeb, Params: map[string]any{"url": body.URL, "dark": dark}}
	if cur := s.sup.Status(); cur.Type == ModeWeb && cur.Display != "" {
		m.Display = cur.Display
	}
	if err := s.sup.Switch(m); err != nil {
		writeErr(w, err)
		return
	}
	s.recordActive()
	writeJSON(w, 200, s.sup.Status())
}

// handleMedia switches to native media mode (mpv as an X11 client on the
// compositor). `POST {"url"|"path":"…","loop":true,"mute":false}` — exactly one
// of url/path; loop/mute optional (loop default true, mute default false).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		URL  string `json:"url"`
		Path string `json:"path"`
		Loop *bool  `json:"loop"`
		Mute *bool  `json:"mute"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	params := map[string]any{}
	if body.URL != "" {
		params["url"] = body.URL
	}
	if body.Path != "" {
		params["path"] = body.Path
	}
	if body.Loop != nil {
		params["loop"] = *body.Loop
	}
	if body.Mute != nil {
		params["mute"] = *body.Mute
	}
	mm := Mode{Type: ModeMedia, Display: DisplayCompositor, Params: params}
	if err := s.sup.Switch(mm); err != nil {
		writeErr(w, err) // validate() enforces XOR/scheme/abs-path → 400
		return
	}
	s.recordActive()
	writeJSON(w, 200, s.sup.Status())
}

// handleTheme switches the kiosk's light/dark appearance in place (CDP
// prefers-color-scheme emulation) without re-navigating or restarting Chromium.
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Dark *bool `json:"dark"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if body.Dark == nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing dark (bool)")})
		return
	}
	if err := s.sup.SetTheme(*body.Dark); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.sup.Status())
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.stats.Snapshot())
}

// handleLogs returns the tail of the agent + child log ring (text, or JSON with
// ?format=json). ?tail=N caps the number of lines (default 200).
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	tail := 200
	if t := r.URL.Query().Get("tail"); t != "" {
		if n, err := strconv.Atoi(t); err == nil && n > 0 {
			tail = n
		}
	}
	lines := logs.tail(tail)
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, 200, map[string]any{"lines": lines})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

// handleRotate rotates the display (X11) via xrandr. `POST {"degrees":0|90|180|270}`.
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Degrees *int   `json:"degrees"`
		Output  string `json:"output"` // optional; default = primary
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if body.Degrees == nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing degrees (0|90|180|270)")})
		return
	}
	if err := s.display.Rotate(body.Output, *body.Degrees); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.display.Info())
}

// handleLayout arranges the connected outputs (multi-display).
// `POST {"mode":"single|mirror|extend","primary":"HDMI-1"}` (primary optional).
func (s *Server) handleLayout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Mode    string `json:"mode"`
		Primary string `json:"primary"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if body.Mode == "" {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing mode (single|mirror|extend)")})
		return
	}
	if err := s.display.SetLayout(body.Mode, body.Primary); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.display.Info())
}

// handleOutputs lists the connected display outputs and their per-output state.
func (s *Server) handleOutputs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET")})
		return
	}
	writeJSON(w, 200, s.display.Outputs())
}

// handleOutputContent assigns content to an output (multi-display). Only the
// PRIMARY output's content renders today; secondary content is persisted +
// reported (rendering deferred — see node-api.md).
// `POST {"output":"HDMI-2","type":"web|media|off|mirror","url":"…","path":"…"}`
func (s *Server) handleOutputContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Output string `json:"output"`
		Type   string `json:"type"`
		URL    string `json:"url"`
		Path   string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.content.SetOutputContent(body.Output, OutputContent{Type: body.Type, URL: body.URL, Path: body.Path}); err != nil {
		writeErr(w, err)
		return
	}
	s.recordActive() // routing the primary output may have switched the mode; persist it
	writeJSON(w, 200, s.display.Outputs())
}

// handleZoom sets the kiosk page zoom. `POST {"percent":125}` or `{"factor":1.25}`.
func (s *Server) handleZoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Percent *int     `json:"percent"`
		Factor  *float64 `json:"factor"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	var factor float64
	switch {
	case body.Factor != nil:
		factor = *body.Factor
	case body.Percent != nil:
		factor = float64(*body.Percent) / 100
	default:
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing percent or factor")})
		return
	}
	if err := s.display.Zoom(factor); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.display.Info())
}

// handleUpgrade kicks off (or re-checks) Debian upgrades. Both actions run in
// the background and return the current apt status immediately; the webUI polls
// /api/stats to watch progress.
//
//	POST /api/upgrade                 → run a conservative apt-get upgrade
//	POST /api/upgrade {"action":"check"} → refresh the pending-upgrade count
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	// Body is optional; ignore a decode error on an empty body.
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)

	if !s.apt.supported {
		writeErr(w, errAptUnsupported)
		return
	}
	switch body.Action {
	case "check":
		if st := s.apt.Status(); st.Upgrading || st.Checking {
			writeErr(w, errAptBusy)
			return
		}
		go s.apt.Check(true)
	case "", "upgrade":
		// Gate the heaviest workload the node can run so it can't stack onto the
		// fragile cold-boot window or an in-flight mode switch / Chromium
		// cold-start and trip the hardware watchdog.
		if up := readUptime(); up > 0 && up < 180 {
			writeErr(w, &apiError{code: 409, err: fmt.Errorf("node is still in the cold-boot window (%ds); try again shortly", up)})
			return
		}
		if s.sup.Busy() {
			writeErr(w, &apiError{code: 409, err: fmt.Errorf("a mode switch or foreground mode is active; try again when the kiosk is steady")})
			return
		}
		if err := s.apt.BeginUpgrade(); err != nil { // atomic claim; rejects if already busy
			writeErr(w, err)
			return
		}
		go s.apt.RunClaimedUpgrade()
	default:
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("unknown action %q (upgrade|check)", body.Action)})
		return
	}
	writeJSON(w, 202, s.apt.Status())
}

// handleDocument returns (GET) or sets (POST) the document (PDF/slides) viewer.
// `POST {"url"|"path":"…","auto_advance_s":0,"enabled":true}` — `path` is a
// relative path under -docs-dir (traversal/symlink rejected); the doc rides the
// web kiosk.
func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.content.DocInfo())
	case http.MethodPost:
		var body struct {
			URL          string `json:"url"`
			Path         string `json:"path"`
			AutoAdvanceS int    `json:"auto_advance_s"`
			Enabled      *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		src := body.URL
		if src == "" {
			src = body.Path
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		info, err := s.content.SetDocument(src, body.AutoAdvanceS, enabled)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, info)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// maxUploadBytes caps a single uploaded library file. Streaming to disk keeps
// even a large video off the heap on a low-RAM node; the cap just bounds a
// runaway upload from filling the SD card in one shot.
const maxUploadBytes = 2 << 30 // 2 GiB

func (s *Server) libReady(w http.ResponseWriter) bool {
	if s.library == nil || !s.library.enabled() {
		writeErr(w, &apiError{code: 501, err: fmt.Errorf("media library not configured (-media-dir)")})
		return false
	}
	return true
}

// handleLibrary lists a media-library folder. `GET /api/library?path=<rel>`.
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET")})
		return
	}
	if !s.libReady(w) {
		return
	}
	list, err := s.library.List(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, list)
}

// handleLibraryUpload streams multipart file part(s) into a folder.
// `POST /api/library/upload?path=<folder>` (multipart/form-data; each file part
// is saved under its filename). The target folder comes from the query so the
// stream needn't be buffered to find a form field.
func (s *Server) handleLibraryUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if !s.libReady(w) {
		return
	}
	dir := r.URL.Query().Get("path")
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("expected multipart/form-data upload")})
		return
	}
	var saved []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("bad multipart body: %w", err)})
			return
		}
		if part.FileName() == "" {
			part.Close() // a plain form field, not a file
			continue
		}
		name := part.FileName()
		n, serr := s.library.SaveFile(dir, name, part, maxUploadBytes)
		part.Close()
		if serr != nil {
			writeErr(w, &apiError{code: 400, err: serr})
			return
		}
		saved = append(saved, fmt.Sprintf("%s (%d bytes)", safeName(name), n))
	}
	if len(saved) == 0 {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("no files in the upload")})
		return
	}
	list, _ := s.library.List(dir)
	writeJSON(w, 200, map[string]any{"saved": saved, "listing": list})
}

// handleLibraryMkdir creates a subfolder. `POST {"path":"<parent>","name":"new"}`.
func (s *Server) handleLibraryMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if !s.libReady(w) {
		return
	}
	var body struct{ Path, Name string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.library.Mkdir(body.Path, body.Name); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	list, _ := s.library.List(body.Path)
	writeJSON(w, 200, map[string]any{"ok": true, "listing": list})
}

// handleLibraryRename renames/moves an item. `POST {"from":"<rel>","to":"<name-or-rel>"}`.
func (s *Server) handleLibraryRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if !s.libReady(w) {
		return
	}
	var body struct{ From, To string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.library.Rename(body.From, body.To); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleLibraryDelete removes a file, or a folder (recursive only when asked).
// `POST {"path":"<rel>","recursive":false}`.
func (s *Server) handleLibraryDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	if !s.libReady(w) {
		return
	}
	var body struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if err := s.library.Delete(body.Path, body.Recursive); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleMediaFile serves a library file with byte-range support (video seeking).
// Auth-gated, plus a loopback exemption so the kiosk/playlist player can fetch it.
func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or HEAD")})
		return
	}
	if s.library == nil || !s.library.enabled() {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/media/")
	if dec, err := url.PathUnescape(rel); err == nil {
		rel = dec
	}
	abs, err := s.library.Resolve(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// An uploaded file is served same-origin with the control UI, so a malicious
	// .html/.svg could otherwise script the API with the operator's cookie. nosniff
	// stops MIME confusion; the CSP sandbox loads it as an opaque, script-less
	// origin. Images/video/PDF (loaded as <img>/<video>/<embed> subresources by
	// /show, or opened directly) still render — they don't need script.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	http.ServeContent(w, r, filepath.Base(abs), fi.ModTime(), f) // sets type + honors Range
}

// handleDocFS serves a local document under -docs-dir (auth-gated). Path
// traversal, symlink escapes, absolute paths, and non-files are rejected.
func (s *Server) handleDocFS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or HEAD")})
		return
	}
	if s.cfg.DocsDir == "" {
		http.NotFound(w, r)
		return
	}
	rel, err := safeDocRel(strings.TrimPrefix(r.URL.Path, "/docfs/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	abs, err := safeDocPath(s.cfg.DocsDir, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Clean(abs))
}

// handleReload sets the periodic reload interval (`{"minutes":15}`, 0 disables)
// or reloads now (`{"now":true}`).
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Minutes *int `json:"minutes"`
		Now     bool `json:"now"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	if body.Now {
		if err := s.sup.ReloadWeb(); err != nil {
			writeErr(w, &apiError{code: 500, err: err})
			return
		}
	}
	if body.Minutes != nil {
		s.content.SetReload(*body.Minutes)
	}
	writeJSON(w, 200, s.content.Info())
}

// handleMessage overlays (or clears) a banner on the kiosk.
// `POST {"text":"…","seconds":30}` or `{"clear":true}`
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Text     string `json:"text"`
		Seconds  int    `json:"seconds"`
		Clear    bool   `json:"clear"`
		Size     int    `json:"size"`     // font px (0 = default 18)
		Position string `json:"position"` // top|bottom|center|top-left|top-right|bottom-left|bottom-right
		Color    string `json:"color"`    // text color (CSS token)
		Bg       string `json:"bg"`       // background color (CSS token)
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	var err error
	if body.Clear || body.Text == "" {
		err = s.sup.ClearMessage()
	} else {
		err = s.sup.ShowMessage(body.Text, body.Seconds, MsgStyle{
			Size: body.Size, Position: body.Position, Color: body.Color, Bg: body.Bg,
		})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handlePlymouth reports the boot-splash state (GET) or changes it (POST).
//
//	GET  /api/plymouth                          → PlymouthInfo
//	POST /api/plymouth {"enabled":true}         → show/hide the splash next boot
//	POST /api/plymouth {"message":"…"}          → set the status line (rebuilds initramfs)
//	POST /api/plymouth {"image_base64":"…"}     → set the splash image PNG (rebuilds initramfs)
//
// message/image changes rebuild the initramfs, so the POST can take tens of
// seconds on a slow node. All changes take effect at the next boot.
func (s *Server) handlePlymouth(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.plymouth.Info())
	case http.MethodPost:
		var body struct {
			Enabled     *bool  `json:"enabled"`
			Message     string `json:"message"`
			ImageBase64 string `json:"image_base64"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 12<<20)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		if body.Enabled != nil {
			if err := s.plymouth.SetEnabled(*body.Enabled); err != nil {
				writeErr(w, err)
				return
			}
		}
		if body.Message != "" {
			if err := s.plymouth.SetMessage(body.Message); err != nil {
				writeErr(w, err)
				return
			}
		}
		if body.ImageBase64 != "" {
			raw := body.ImageBase64
			if i := strings.IndexByte(raw, ','); strings.HasPrefix(raw, "data:") && i > 0 {
				raw = raw[i+1:] // strip a data: URL prefix if the UI sent one
			}
			png, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
			if err != nil {
				writeErr(w, &apiError{code: 400, err: fmt.Errorf("image_base64 is not valid base64: %w", err)})
				return
			}
			if err := s.plymouth.SetImage(png); err != nil {
				writeErr(w, err)
				return
			}
		}
		info := s.plymouth.Info()
		info.Note = "applies at the next boot"
		writeJSON(w, 200, info)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleCEC reports TV state (GET) or runs a CEC action (POST).
//
//	GET  /api/cec                           → CECInfo
//	POST /api/cec {"action":"on|off|active-source"}
func (s *Server) handleCEC(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.cec.Info())
	case http.MethodPost:
		var body struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
		var err error
		switch body.Action {
		case "on":
			err = s.cec.On()
		case "off":
			err = s.cec.Off()
		case "active-source", "source":
			err = s.cec.ActiveSource()
		case "volume-up":
			err = s.cec.VolumeUp()
		case "volume-down":
			err = s.cec.VolumeDown()
		case "mute":
			err = s.cec.Mute()
		default:
			err = &apiError{code: 400, err: fmt.Errorf("unknown cec action %q (on|off|active-source|volume-up|volume-down|mute)", body.Action)}
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, s.cec.Info())
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleScreen sleeps/wakes the attached display (node-api.md §2). It disables
// or enables the X output (the reliable lever where DPMS is absent) AND sends
// CEC standby/on for a CEC TV — so it works on a plain monitor (disp) or a real
// TV (disp2). For CEC-only TV power without touching the output, use /api/cec.
func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		On     *bool  `json:"on"`
		Output string `json:"output"` // optional; default = primary
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if body.On == nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing on (bool)")})
		return
	}
	if err := s.display.Screen(body.Output, *body.On); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s.display.Info())
}

// handleVNC reports VNC state (GET, also drives webUI feature detection) or
// pins/unpins the on-demand server (POST {"on":bool}). The actual live view is
// the noVNC viewer at /vnc bridged through /vnc/ws.
func (s *Server) handleVNC(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.vnc.Status())
	case http.MethodPost:
		var body struct {
			On *bool `json:"on"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		if body.On == nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("missing on (bool)")})
			return
		}
		if err := s.vnc.Pin(*body.On); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, s.vnc.Status())
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

// handleScreenshot returns a PNG of what's actually on the display. The default
// backend is the universal DRM/KMS capture (disp-kmsshot): it reads the active
// scanout below the compositor, so it works for EVERY mode — including the
// cog/KMS kiosk, the Wayland primary, and a bare console that the CDP (web-only)
// and scrot (X-only) fallbacks can't reach. drmModeGetFB2 needs CAP_SYS_ADMIN,
// not DRM master, and EGL detiles the vc4-tiled buffer — so the old "can't shoot
// the cog kiosk" 503 is gone. ?backend=kms|cdp|scrot|grim forces a source (grim = the Wayland analogue of scrot); ?w=<px>
// downscales.
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	backend := s.cfg.ScreenshotBackend
	if backend == "" {
		backend = "auto"
	}
	if q := r.URL.Query().Get("backend"); q != "" {
		backend = q
	}
	ms := s.sup.Status()

	var png []byte
	var err error

	// Primary path: the universal KMS scanout capture — correct for any mode,
	// including the cog/KMS kiosk where CDP would shoot the hidden X Chromium.
	if backend == "auto" || backend == "kms" {
		png, err = s.sup.CaptureKMS(r.Context())
		if err != nil {
			if backend == "kms" { // explicit choice — don't silently fall back
				writeErr(w, &apiError{code: 503, err: err})
				return
			}
			log.Printf("screenshot: KMS capture failed, falling back to mode-specific: %v", err)
			png = nil
		}
	}

	// Fallbacks for `auto` (or an explicit cdp/scrot). The CDP case stays gated on
	// Display != KMS: when the cog kiosk is foreground the suspended X Chromium may
	// still be CDP-attached, and shooting it would capture the wrong surface.
	if png == nil {
		cdpOK := backend == "auto" || backend == "cdp"
		scrotOK := backend == "auto" || backend == "scrot"
		grimOK := backend == "auto" || backend == "grim"
		switch {
		case cdpOK && ms.Type == ModeWeb && ms.Display != DisplayKMS && s.sup.chrome.Attached():
			png, err = s.sup.chrome.Screenshot() // CDP shot of the web kiosk
		case scrotOK && ms.Display == DisplayCompositor && ms.Type != ModeOff:
			png, err = s.sup.CaptureCompositor() // scrot (X surface)
		case grimOK && ms.Display == DisplayWayland && ms.Type != ModeOff:
			png, err = s.sup.CaptureWayland() // grim (labwc/Wayland surface)
		default:
			writeErr(w, &apiError{code: 503, err: fmt.Errorf("no screenshot source for mode %q (backend %q)", ms.Type, backend)})
			return
		}
		if err != nil {
			writeErr(w, &apiError{code: 503, err: err})
			return
		}
	}

	if ws := r.URL.Query().Get("w"); ws != "" {
		if maxW, err := strconv.Atoi(ws); err == nil && maxW > 0 {
			if scaled, err := downscalePNG(png, maxW); err == nil {
				png = scaled
			}
		}
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

// handleShowPage serves the embedded mixed-media playlist player.
func (s *Server) handleShowPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // player HTML changes on deploy; keep the kiosk fresh
	w.Write(showHTML)
}

// handlePlaylistMedia returns (GET) or sets (POST) the unified media playlist.
//
//	POST {"items":[{kind,src,title?,duration_s?}], "interval_s":8, "loop":true,
//	      "shuffle":false, "transition":"fade|none", "play":true}
//
// With play:true it also points the kiosk at the /show player — a real mode
// switch, so the playlist persists and is restored on reboot.
func (s *Server) handlePlaylistMedia(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// ?action=<slug> → that playlist action's items (what /show?action= fetches);
		// no param → the single global playlist.
		if slug := r.URL.Query().Get("action"); slug != "" {
			if pi, ok := s.actions.PlaylistInfo(slug); ok {
				writeJSON(w, 200, pi)
				return
			}
			writeErr(w, &apiError{code: 404, err: fmt.Errorf("no playlist action %q", slug)})
			return
		}
		writeJSON(w, 200, s.playlist.Info())
	case http.MethodPost:
		var body struct {
			Items      []PlaylistItem `json:"items"`
			IntervalS  int            `json:"interval_s"`
			Loop       *bool          `json:"loop"`
			Shuffle    bool           `json:"shuffle"`
			Transition string         `json:"transition"`
			Play       bool           `json:"play"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		loop := true
		if body.Loop != nil {
			loop = *body.Loop
		}
		info := s.playlist.Set(body.Items, body.IntervalS, loop, body.Shuffle, body.Transition)
		if body.Play {
			if len(info.Items) == 0 {
				writeErr(w, &apiError{code: 400, err: fmt.Errorf("playlist is empty")})
				return
			}
			// Play = point the kiosk at the /show player. Clear the CDP content
			// owner (the document viewer) so a lingering timer can't navigate away
			// from /show. Inherit the current compositor backend.
			s.content.DisableOwners()
			m := Mode{Type: ModeWeb, Params: map[string]any{"url": info.ShowURL}}
			if cur := s.sup.Status(); cur.Type == ModeWeb && cur.Display != "" {
				m.Display = cur.Display
			}
			if err := s.sup.Switch(m); err != nil {
				writeErr(w, err)
				return
			}
			s.recordActive()
		}
		writeJSON(w, 200, info)
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The panel shell changes on every deploy and the URL never does — don't let a
	// browser serve a stale cached copy (a deploy updated the node but the operator's
	// browser kept showing the old UI).
	w.Header().Set("Cache-Control", "no-store")
	w.Write(indexHTML)
}

// ---- helpers ----

func health(ms ModeStatus) string {
	switch ms.State {
	case stateRunning:
		return "ok"
	case stateStarting:
		return "degraded"
	case stateFailed:
		return "down"
	default: // stopped / off
		return "ok"
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := 500
	if ae, ok := err.(*apiError); ok {
		code = ae.status()
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
