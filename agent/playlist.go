package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Playlist is the unified mixed-media playlist: an ordered list of images,
// videos, audio, and documents (from the media library or http(s) URLs) that the
// kiosk plays by pointing at the agent-served /show player page. Unlike the
// CDP-injected slideshow, /show is self-contained — it fetches this config and
// advances client-side (on an interval, or when a video/audio ends), so mixed
// media "one after another" works without any agent-side ticker. Persisted in
// playlist.json next to the other node state.
type Playlist struct {
	path      string
	localBase string

	mu         sync.Mutex
	items      []PlaylistItem
	intervalS  int
	loop       bool
	shuffle    bool
	transition string

	saveMu sync.Mutex // serialize save() so concurrent writers can't tear the file
}

// PlaylistItem is one entry. Src is either an http(s) URL or a media-library
// relative path (resolved by /show to /media/<src>). Kind drives how /show
// renders + advances it. DurationS overrides the default interval for this item
// (0 = use the playlist interval; videos/audio always advance on their end event).
type PlaylistItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // image | video | audio | doc
	Src       string `json:"src"`
	Title     string `json:"title,omitempty"`
	DurationS int    `json:"duration_s,omitempty"`
}

// PlaylistInfo is the JSON for GET /api/playlist-media (and what /show fetches).
type PlaylistInfo struct {
	Items      []PlaylistItem `json:"items"`
	IntervalS  int            `json:"interval_s"`
	Loop       bool           `json:"loop"`
	Shuffle    bool           `json:"shuffle"`
	Transition string         `json:"transition"`
	ShowURL    string         `json:"show_url"` // loopback URL to point the kiosk at
}

type persistedPlaylist struct {
	Items      []PlaylistItem `json:"items"`
	IntervalS  int            `json:"interval_s"`
	Loop       bool           `json:"loop"`
	Shuffle    bool           `json:"shuffle"`
	Transition string         `json:"transition"`
}

// maxPlaylistItems bounds a playlist so a huge submission can't exhaust a weak
// node (the /show page holds the list + preloads the next item).
const maxPlaylistItems = 500

func NewPlaylist(cfg *Config) *Playlist {
	p := &Playlist{intervalS: 8, loop: true, transition: "fade", localBase: localAgentBase(cfg.Addr)}
	if cfg.StateFile != "" {
		p.path = filepath.Join(filepath.Dir(cfg.StateFile), "playlist.json")
	}
	p.load()
	return p
}

func (p *Playlist) load() {
	if p.path == "" {
		return
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var pp persistedPlaylist
	if json.Unmarshal(b, &pp) != nil {
		return
	}
	p.items = sanitizeItems(pp.Items)
	if pp.IntervalS > 0 {
		p.intervalS = pp.IntervalS
	}
	p.loop = pp.Loop
	p.shuffle = pp.Shuffle
	p.transition = normTransition(pp.Transition)
}

func (p *Playlist) save() {
	if p.path == "" {
		return
	}
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	pp := persistedPlaylist{Items: p.items, IntervalS: p.intervalS, Loop: p.loop, Shuffle: p.shuffle, Transition: p.transition}
	p.mu.Unlock()
	b, _ := json.MarshalIndent(pp, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		log.Printf("[playlist] state dir: %v", err)
		return
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[playlist] save: %v", err)
		return
	}
	if err := os.Rename(tmp, p.path); err != nil {
		log.Printf("[playlist] save rename: %v", err)
	}
}

var playlistKinds = map[string]bool{"image": true, "video": true, "audio": true, "doc": true}

// sanitizeItems drops malformed entries and assigns a stable id (derived from
// kind+src) to any item missing one, so the UI can key + reorder reliably.
func sanitizeItems(in []PlaylistItem) []PlaylistItem {
	out := make([]PlaylistItem, 0, len(in))
	for _, it := range in {
		it.Kind = strings.ToLower(strings.TrimSpace(it.Kind))
		it.Src = strings.TrimSpace(it.Src)
		if it.Src == "" || !playlistKinds[it.Kind] {
			continue
		}
		// A library src must be a safe relative path (no scheme, no traversal); an
		// http(s) URL passes through. Anything else (file://, ../) is rejected.
		if !strings.HasPrefix(it.Src, "http://") && !strings.HasPrefix(it.Src, "https://") {
			if strings.Contains(it.Src, "://") {
				continue
			}
			if _, err := safeDocRel(strings.TrimPrefix(it.Src, "/media/")); err != nil {
				continue
			}
			it.Src = strings.TrimPrefix(it.Src, "/media/")
		}
		if it.ID == "" {
			it.ID = histID(it.Kind + ":" + it.Src)
		}
		if it.DurationS < 0 {
			it.DurationS = 0
		}
		out = append(out, it)
		if len(out) >= maxPlaylistItems {
			break
		}
	}
	return out
}

// Set replaces the playlist config and persists it. Returns the stored info.
func (p *Playlist) Set(items []PlaylistItem, intervalS int, loop, shuffle bool, transition string) PlaylistInfo {
	clean := sanitizeItems(items)
	if intervalS < 1 {
		intervalS = 8
	}
	p.mu.Lock()
	p.items = clean
	p.intervalS = intervalS
	p.loop = loop
	p.shuffle = shuffle
	p.transition = normTransition(transition)
	p.mu.Unlock()
	p.save()
	return p.Info()
}

func (p *Playlist) Info() PlaylistInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	items := make([]PlaylistItem, 0, len(p.items)) // non-nil → JSON [] not null
	items = append(items, p.items...)
	return PlaylistInfo{
		Items:      items,
		IntervalS:  p.intervalS,
		Loop:       p.loop,
		Shuffle:    p.shuffle,
		Transition: p.transition,
		ShowURL:    p.ShowURL(),
	}
}

func (p *Playlist) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.items) }

// PlaylistSummary is the cheap playlist block for /api/snapshot (the full item
// list comes from GET /api/playlist-media on demand).
type PlaylistSummary struct {
	Count   int    `json:"count"`
	ShowURL string `json:"show_url"`
}

func (p *Playlist) Summary() PlaylistSummary {
	return PlaylistSummary{Count: p.count(), ShowURL: p.ShowURL()}
}

// ShowURL is the loopback URL the kiosk is pointed at to play the list.
func (p *Playlist) ShowURL() string { return p.localBase + "/show" }
