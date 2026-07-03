package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Action is a saved, named, fireable launcher: a Mode the operator has named and
// given a stable url-safe slug plus an ordering index. It generalizes the older
// app-only CustomMode to wrap ANY mode (web/app/media/receiver/off). Firing an
// action is just sup.Switch(action.Mode) — the same path POST /api/mode takes —
// so it inherits all the supervisor's arbitration + validation. Persisted in
// actions.json next to the other node state; fired by slug via
// POST /api/action/<slug> (a Stream-Deck-friendly one-liner).
type Action struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Index int    `json:"index"`
	Name  string `json:"name"`
	Mode  Mode   `json:"mode"`
	// Playlist, when set, makes this a named mixed-media playlist: the action's Mode
	// is auto-derived to a web kiosk pointed at the /show player for this slug
	// (/show?action=<slug>), and /show fetches these items. This is what lets a node
	// hold several playlists ("Morning news", "Photo wall") as separate actions,
	// unlike the single global playlist.
	Playlist *ActionPlaylist `json:"playlist,omitempty"`
}

// ActionPlaylist is the self-contained playlist an action of that kind carries.
type ActionPlaylist struct {
	Items      []PlaylistItem `json:"items"`
	IntervalS  int            `json:"interval_s"`
	Loop       bool           `json:"loop"`
	Shuffle    bool           `json:"shuffle"`
	Transition string         `json:"transition"`
}

// Actions is the persisted collection of saved actions. It mirrors the hardened
// persistence discipline of Playlist/State: a data mutex for the slice plus a
// separate saveMu that serializes the whole snapshot→marshal→atomic-rename write,
// so a crash or a racing reader never sees a torn file.
type Actions struct {
	path      string
	localBase string // agent loopback base (e.g. http://127.0.0.1:80) for playlist show URLs

	mu       sync.Mutex
	items    []Action
	fileSeen bool // actions.json existed at load → never auto-seed from custom modes

	saveMu sync.Mutex
}

// ActionsInfo is the JSON for GET /api/actions and the snapshot `actions` block.
type ActionsInfo struct {
	Actions []Action `json:"actions"`
}

type persistedActions struct {
	Items []Action `json:"items"`
}

// maxActions bounds the collection so a runaway client can't grow the file
// unboundedly on a weak node.
const maxActions = 200

func NewActions(cfg *Config) *Actions {
	a := &Actions{localBase: localAgentBase(cfg.Addr)}
	if cfg.StateFile != "" {
		a.path = filepath.Join(filepath.Dir(cfg.StateFile), "actions.json")
	}
	a.load()
	return a
}

// playlistActionMode is the web mode a playlist action runs: the /show player
// pointed at this slug. Display is left unset so the fire path inherits the node's
// current web backend (X11 vs Wayland).
func playlistActionMode(localBase, slug string) Mode {
	return Mode{Type: ModeWeb, Params: map[string]any{"url": localBase + "/show?action=" + slug}}
}

func (a *Actions) load() {
	if a.path == "" {
		return
	}
	b, err := os.ReadFile(a.path)
	if err != nil {
		return // no file yet → empty; Seed may migrate legacy custom modes
	}
	a.fileSeen = true
	var p persistedActions
	if json.Unmarshal(b, &p) != nil {
		return
	}
	a.mu.Lock()
	a.items = normalizeActions(p.Items, a.localBase)
	a.mu.Unlock()
}

func (a *Actions) save() {
	if a.path == "" {
		return
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	p := persistedActions{Items: append([]Action(nil), a.items...)}
	a.mu.Unlock()
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		log.Printf("[actions] state dir: %v", err)
		return
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[actions] save: %v", err)
		return
	}
	if err := os.Rename(tmp, a.path); err != nil {
		log.Printf("[actions] save rename: %v", err)
	}
}

var slugStripRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify normalizes a string to a url-safe [a-z0-9-] slug: lowercase, runs of
// other characters collapsed to a single '-', leading/trailing '-' trimmed.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugStripRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// uniqueSlug returns slug, or slug-2 / slug-3 / … if slug is already taken.
func uniqueSlug(slug string, taken map[string]bool) string {
	if !taken[slug] {
		return slug
	}
	for n := 2; ; n++ {
		cand := fmt.Sprintf("%s-%d", slug, n)
		if !taken[cand] {
			return cand
		}
	}
}

// normalizeActions repairs a loaded/seeded slice: every action gets a non-empty
// unique slug, a stable id, and its slice position becomes its index. It does NOT
// drop actions whose Mode is currently invalid (the operator may fix them, or a
// capability may become available); validity is enforced on Upsert and at fire.
func normalizeActions(in []Action, localBase string) []Action {
	out := make([]Action, 0, len(in))
	seen := map[string]bool{}
	for i, a := range in {
		a.Name = strings.TrimSpace(a.Name)
		a.Mode.normalize()
		slug := slugify(a.Slug)
		if slug == "" {
			slug = slugify(a.Name)
		}
		if slug == "" {
			slug = "action"
		}
		a.Slug = uniqueSlug(slug, seen)
		seen[a.Slug] = true
		if a.Playlist != nil { // re-derive the playlist action's items + show-URL mode
			a.Playlist.Items = sanitizeItems(a.Playlist.Items)
			if a.Playlist.IntervalS < 1 {
				a.Playlist.IntervalS = 8
			}
			a.Playlist.Transition = normTransition(a.Playlist.Transition)
			a.Mode = playlistActionMode(localBase, a.Slug)
		}
		if a.ID == "" {
			a.ID = histID(fmt.Sprintf("%s|%d", a.Slug, i))
		}
		a.Index = i
		out = append(out, a)
		if len(out) >= maxActions {
			break
		}
	}
	return out
}

// Upsert creates or updates an action. A new action (empty ID) is appended and
// assigned an id, a unique slug, and an index at the end; an existing one (its ID
// matches) is updated in place, keeping its index. The Mode is validated
// (Mode.validate) so an un-fireable action can never be saved. An operator-typed
// slug that collides with a different action is an error; a slug derived from the
// name is made unique automatically.
func (a *Actions) Upsert(in Action) (Action, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Action{}, fmt.Errorf("an action needs a name")
	}
	isPlaylist := in.Playlist != nil
	if isPlaylist {
		in.Playlist.Items = sanitizeItems(in.Playlist.Items)
		if len(in.Playlist.Items) == 0 {
			return Action{}, fmt.Errorf("a playlist action needs at least one item")
		}
		if in.Playlist.IntervalS < 1 {
			in.Playlist.IntervalS = 8
		}
		in.Playlist.Transition = normTransition(in.Playlist.Transition)
	} else {
		in.Mode.normalize()
		if err := in.Mode.validate(); err != nil {
			return Action{}, err
		}
	}
	userSlug := slugify(in.Slug) != ""
	slug := slugify(in.Slug)
	if slug == "" {
		slug = slugify(in.Name)
	}
	if slug == "" {
		slug = "action"
	}

	a.mu.Lock()
	idx := -1
	if in.ID != "" {
		for i := range a.items {
			if a.items[i].ID == in.ID {
				idx = i
				break
			}
		}
	}
	taken := map[string]bool{}
	for i := range a.items {
		if i == idx {
			continue
		}
		taken[a.items[i].Slug] = true
	}
	if taken[slug] {
		if userSlug {
			a.mu.Unlock()
			return Action{}, fmt.Errorf("slug %q is already used by another action", slug)
		}
		slug = uniqueSlug(slug, taken)
	}
	in.Slug = slug
	if isPlaylist { // the Mode is derived from the final slug (the /show URL)
		in.Mode = playlistActionMode(a.localBase, slug)
	}

	if idx >= 0 {
		in.ID = a.items[idx].ID
		in.Index = a.items[idx].Index // ordering is owned by Reorder, not Upsert
		a.items[idx] = in
	} else {
		if len(a.items) >= maxActions {
			a.mu.Unlock()
			return Action{}, fmt.Errorf("too many actions (max %d)", maxActions)
		}
		in.ID = histID(fmt.Sprintf("%s|%d", slug, time.Now().UnixNano()))
		// Index = one past the current MAX, not len(items): Delete leaves gaps
		// (it doesn't renumber), so len() could collide with a surviving index and
		// mis-sort the new action. max+1 keeps a new action last regardless of gaps.
		maxIdx := -1
		for i := range a.items {
			if a.items[i].Index > maxIdx {
				maxIdx = a.items[i].Index
			}
		}
		in.Index = maxIdx + 1
		a.items = append(a.items, in)
	}
	a.mu.Unlock()
	a.save()
	return in, nil
}

// Delete removes an action by slug or id; returns whether it existed.
func (a *Actions) Delete(key string) bool {
	if key == "" {
		return false
	}
	a.mu.Lock()
	before := len(a.items)
	out := a.items[:0]
	for _, it := range a.items {
		if it.Slug == key || it.ID == key {
			continue
		}
		out = append(out, it)
	}
	a.items = out
	removed := len(a.items) != before
	a.mu.Unlock()
	if removed {
		a.save()
	}
	return removed
}

// Get looks up an action by slug (preferred) or id.
func (a *Actions) Get(key string) (Action, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, it := range a.items {
		if it.Slug == key {
			return it, true
		}
	}
	for _, it := range a.items {
		if it.ID == key {
			return it, true
		}
	}
	return Action{}, false
}

// Reorder sets the display order from a list of slugs (or ids): listed actions
// take positions 0..n-1 in the given order, any omitted keep their relative order
// after them. Indices are then renumbered compactly.
func (a *Actions) Reorder(order []string) {
	pos := make(map[string]int, len(order))
	for i, key := range order {
		pos[key] = i
	}
	a.mu.Lock()
	next := len(order)
	for i := range a.items {
		if p, ok := pos[a.items[i].Slug]; ok {
			a.items[i].Index = p
		} else if p, ok := pos[a.items[i].ID]; ok {
			a.items[i].Index = p
		} else {
			a.items[i].Index = next
			next++
		}
	}
	sort.SliceStable(a.items, func(i, j int) bool { return a.items[i].Index < a.items[j].Index })
	for i := range a.items {
		a.items[i].Index = i
	}
	a.mu.Unlock()
	a.save()
}

// List returns a copy of the actions sorted by index (the display order).
func (a *Actions) List() []Action {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Action, len(a.items)) // non-nil → JSON [] not null
	copy(out, a.items)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func (a *Actions) Info() ActionsInfo { return ActionsInfo{Actions: a.List()} }

// PlaylistInfo returns the playlist config for a playlist action by slug — what
// the /show player fetches via GET /api/playlist-media?action=<slug>.
func (a *Actions) PlaylistInfo(slug string) (PlaylistInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, it := range a.items {
		if it.Slug == slug && it.Playlist != nil {
			items := make([]PlaylistItem, 0, len(it.Playlist.Items))
			items = append(items, it.Playlist.Items...)
			return PlaylistInfo{
				Items:      items,
				IntervalS:  it.Playlist.IntervalS,
				Loop:       it.Playlist.Loop,
				Shuffle:    it.Playlist.Shuffle,
				Transition: it.Playlist.Transition,
				ShowURL:    a.localBase + "/show?action=" + slug,
			}, true
		}
	}
	return PlaylistInfo{}, false
}

// CurrentSlug returns the slug of the saved action matching the mode now on
// screen, or "" if none matches. It compares by type + identity label (the url /
// argv / host that names the mode), so it still highlights the live action after
// an in-place theme/dark change. Prefers an exact display match, then a looser
// same-identity match (e.g. a web action shown on either compositor backend).
func (a *Actions) CurrentSlug(cur Mode) string {
	if cur.Type == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, it := range a.items {
		if it.Mode.Type == cur.Type && it.Mode.Display == cur.Display && it.Mode.label() == cur.label() {
			return it.Slug
		}
	}
	for _, it := range a.items {
		if it.Mode.Type == cur.Type && it.Mode.label() == cur.label() {
			return it.Slug
		}
	}
	return ""
}

// Seed populates the collection from the legacy app-only custom modes on FIRST
// run only (no actions.json existed). Each custom mode becomes an app-type action
// with a slug derived from its name. Idempotent: a node that already has an
// actions.json — even an empty one the operator cleared — is never re-seeded.
func (a *Actions) Seed(customs []CustomMode) {
	a.mu.Lock()
	skip := a.fileSeen || len(a.items) > 0
	a.mu.Unlock()
	if skip || len(customs) == 0 {
		return
	}
	n := 0
	for _, cm := range customs {
		if _, err := a.Upsert(Action{Name: cm.Name, Slug: slugify(cm.Name), Mode: cm.mode()}); err == nil {
			n++
		}
	}
	if n > 0 {
		log.Printf("[actions] migrated %d custom mode(s) into actions.json", n)
	}
}
