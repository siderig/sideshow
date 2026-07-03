package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State persists the node's *active mode* and a short history of what has been
// shown, so a reboot or agent restart returns to the same screen instead of the
// hardcoded boot URL. It also carries the one-time setup-complete flag the
// first-run wizard sets. Persisted next to the display/content state in
// state.json.
//
// The supervisor itself stays stateless-on-restart (it owns the screen and comes
// back clean); State is the thin layer above it that remembers the operator's
// last intent and replays it at boot. RecordMode is called after every
// successful mode switch; Restore is consulted once at startup.
type State struct {
	cfg  *Config
	path string

	mu            sync.Mutex
	active        *Mode
	setupComplete bool
	history       []HistoryItem
	customModes   []CustomMode
	localInput    *bool // node-global: does the local keyboard/mouse drive the kiosk? (nil = unset → default)

	saveMu sync.Mutex // serializes save() so concurrent writers can't tear the file
}

// HistoryItem is one entry in the "things this node has shown" list. Mode holds
// enough to relaunch it verbatim (POST /api/mode); ID is a stable hash of the
// item's label so re-showing the same thing moves it to the front rather than
// piling up duplicates.
type HistoryItem struct {
	ID    string `json:"id"`
	At    string `json:"at"` // RFC3339 UTC
	Type  string `json:"type"`
	Label string `json:"label"`
	Mode  Mode   `json:"mode"`
}

// CustomMode is a saved, named launcher the operator defines: an arbitrary
// program run as an `app` mode with a chosen surface. display=kms (the
// framebuffer) runs as root and is gated by -allow-custom-root.
type CustomMode struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Display string            `json:"display"` // compositor | console | kms | wayland
	Env     map[string]string `json:"env,omitempty"`
}

// mode builds the app-mode the supervisor runs for this custom launcher.
func (cm CustomMode) mode() Mode {
	argv := append([]string{cm.Command}, cm.Args...)
	params := map[string]any{"argv": argv}
	if len(cm.Env) > 0 {
		env := make(map[string]any, len(cm.Env))
		for k, v := range cm.Env {
			env[k] = v
		}
		params["env"] = env
	}
	return Mode{Type: ModeApp, Display: cm.Display, Params: params}
}

// StateInfo is the JSON for GET /api/state and the `state` block of /api/snapshot.
type StateInfo struct {
	Active        *Mode         `json:"active,omitempty"`
	SetupComplete bool          `json:"setup_complete"`
	History       []HistoryItem `json:"history"`
	CustomModes   []CustomMode  `json:"custom_modes"`
}

type persistedState struct {
	Active        *Mode         `json:"active,omitempty"`
	SetupComplete bool          `json:"setup_complete"`
	History       []HistoryItem `json:"history,omitempty"`
	CustomModes   []CustomMode  `json:"custom_modes,omitempty"`
	LocalInput    *bool         `json:"local_input,omitempty"`
}

// maxHistory bounds the remembered-shows list (most-recent first).
const maxHistory = 50

func NewState(cfg *Config) *State {
	s := &State{cfg: cfg}
	if cfg.StateFile != "" {
		s.path = filepath.Join(filepath.Dir(cfg.StateFile), "state.json")
	}
	s.load()
	return s
}

func (s *State) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return // no state yet → defaults
	}
	var p persistedState
	if json.Unmarshal(b, &p) != nil {
		return
	}
	s.active = p.Active
	s.setupComplete = p.SetupComplete
	s.history = p.History
	s.customModes = p.CustomModes
	s.localInput = p.LocalInput
}

func (s *State) save() {
	if s.path == "" {
		return
	}
	s.saveMu.Lock()         // serialize the whole snapshot→write so concurrent saves
	defer s.saveMu.Unlock() // can't tear the file or clobber each other's update
	s.mu.Lock()
	p := persistedState{
		Active:        s.active,
		SetupComplete: s.setupComplete,
		History:       append([]HistoryItem(nil), s.history...),
		CustomModes:   append([]CustomMode(nil), s.customModes...),
		LocalInput:    s.localInput,
	}
	s.mu.Unlock()
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("[state] state dir: %v", err)
		return
	}
	// Atomic replace: write a temp then rename, so a crash or a racing reader
	// never sees a half-written file.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[state] save: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[state] save rename: %v", err)
	}
}

// histID is a stable id for a history entry, derived from its label so the same
// URL/app collapses to one moved-to-front entry instead of accumulating.
func histID(label string) string {
	h := fnv.New64a()
	h.Write([]byte(label))
	return fmt.Sprintf("%016x", h.Sum64())
}

// RecordMode remembers a mode as the active one and (unless it is off/empty)
// prepends it to the history, de-duplicating by label and capping the list.
// Called after every successful mode switch — boot and API alike — so the
// persisted "active" always matches what is really on screen.
func (s *State) RecordMode(m Mode) {
	cp := Mode{Type: m.Type, Display: m.Display, Params: copyParams(m.Params)}
	s.mu.Lock()
	s.active = &cp
	if m.Type != ModeOff && m.Type != "" {
		item := HistoryItem{
			ID:    histID(m.label()),
			At:    time.Now().UTC().Format(time.RFC3339),
			Type:  m.Type,
			Label: m.label(),
			Mode:  cp,
		}
		filtered := make([]HistoryItem, 0, len(s.history)+1)
		filtered = append(filtered, item)
		for _, h := range s.history {
			if h.ID != item.ID {
				filtered = append(filtered, h)
			}
		}
		if len(filtered) > maxHistory {
			filtered = filtered[:maxHistory]
		}
		s.history = filtered
	}
	s.mu.Unlock()
	s.save()
}

// Restore returns the mode to enter at boot (a value copy), or ok=false when no
// active mode was persisted (a fresh node → the caller falls back to -start-mode).
func (s *State) Restore() (Mode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.Type == "" {
		return Mode{}, false
	}
	return Mode{Type: s.active.Type, Display: s.active.Display, Params: copyParams(s.active.Params)}, true
}

// DeleteHistory removes one history entry by id; returns whether it was present.
func (s *State) DeleteHistory(id string) bool {
	s.mu.Lock()
	before := len(s.history)
	filtered := s.history[:0]
	for _, h := range s.history {
		if h.ID != id {
			filtered = append(filtered, h)
		}
	}
	s.history = filtered
	removed := len(s.history) != before
	s.mu.Unlock()
	if removed {
		s.save()
	}
	return removed
}

// ClearHistory empties the history (the active mode is untouched).
func (s *State) ClearHistory() {
	s.mu.Lock()
	s.history = nil
	s.mu.Unlock()
	s.save()
}

// SetSetupComplete records (and persists) whether the first-run wizard finished.
func (s *State) SetSetupComplete(v bool) {
	s.mu.Lock()
	s.setupComplete = v
	s.mu.Unlock()
	s.save()
}

func (s *State) SetupComplete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupComplete
}

// LocalInputSet reports whether an explicit local-input policy has been saved
// (vs. still falling back to the -no-local-input boot default).
func (s *State) LocalInputSet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localInput != nil
}

// LocalInputAllowed reports whether the local keyboard/mouse may drive the kiosk,
// falling back to def when no explicit policy has been saved.
func (s *State) LocalInputAllowed(def bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localInput == nil {
		return def
	}
	return *s.localInput
}

// SetLocalInput saves the local-input policy — the source of truth the agent
// applies (udev/Xorg rules) at boot and the /api/input toggle mutates.
func (s *State) SetLocalInput(allowed bool) {
	s.mu.Lock()
	v := allowed
	s.localInput = &v
	s.mu.Unlock()
	s.save()
}

func (s *State) Info() StateInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var act *Mode
	if s.active != nil {
		a := Mode{Type: s.active.Type, Display: s.active.Display, Params: copyParams(s.active.Params)}
		act = &a
	}
	hist := make([]HistoryItem, 0, len(s.history)) // non-nil → JSON [] not null
	hist = append(hist, s.history...)
	cms := make([]CustomMode, 0, len(s.customModes))
	cms = append(cms, s.customModes...)
	return StateInfo{
		Active:        act,
		SetupComplete: s.setupComplete,
		History:       hist,
		CustomModes:   cms,
	}
}

// UpsertCustom adds or updates a saved custom mode, assigning an id to a new one.
func (s *State) UpsertCustom(cm CustomMode) (CustomMode, error) {
	cm.Name = strings.TrimSpace(cm.Name)
	cm.Command = strings.TrimSpace(cm.Command)
	if cm.Name == "" || cm.Command == "" {
		return CustomMode{}, fmt.Errorf("a custom mode needs a name and a command")
	}
	if cm.Display == "" {
		cm.Display = DisplayCompositor
	}
	// Env is only wired into the compositor/console child environment; the
	// framebuffer + Wayland launchers build a fixed env, so accepting env there
	// would silently do nothing. Reject it rather than mislead the operator.
	if len(cm.Env) > 0 && cm.Display != DisplayCompositor && cm.Display != DisplayConsole {
		return CustomMode{}, fmt.Errorf("environment variables only apply to GUI (X11) or console custom modes")
	}
	s.mu.Lock()
	if cm.ID == "" {
		cm.ID = histID(fmt.Sprintf("%s|%s|%d", cm.Name, cm.Command, time.Now().UnixNano()))
		s.customModes = append(s.customModes, cm)
	} else {
		found := false
		for i := range s.customModes {
			if s.customModes[i].ID == cm.ID {
				s.customModes[i] = cm
				found = true
				break
			}
		}
		if !found {
			s.customModes = append(s.customModes, cm)
		}
	}
	s.mu.Unlock()
	s.save()
	return cm, nil
}

// DeleteCustom removes a saved custom mode by id; returns whether it existed.
func (s *State) DeleteCustom(id string) bool {
	s.mu.Lock()
	before := len(s.customModes)
	out := s.customModes[:0]
	for _, c := range s.customModes {
		if c.ID != id {
			out = append(out, c)
		}
	}
	s.customModes = out
	removed := len(s.customModes) != before
	s.mu.Unlock()
	if removed {
		s.save()
	}
	return removed
}

// GetCustom looks up a saved custom mode by id.
func (s *State) GetCustom(id string) (CustomMode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.customModes {
		if c.ID == id {
			return c, true
		}
	}
	return CustomMode{}, false
}
