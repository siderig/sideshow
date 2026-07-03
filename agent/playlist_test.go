package main

import (
	"path/filepath"
	"testing"
)

func newTestPlaylist(t *testing.T) (*Playlist, *Config) {
	t.Helper()
	cfg := &Config{StateFile: filepath.Join(t.TempDir(), "display.json")}
	return NewPlaylist(cfg), cfg
}

func TestPlaylistSanitizeDropsBadItems(t *testing.T) {
	p, _ := newTestPlaylist(t)
	info := p.Set([]PlaylistItem{
		{Kind: "image", Src: "lobby/a.jpg"},        // ok (library path)
		{Kind: "video", Src: "https://x/v.mp4"},    // ok (url)
		{Kind: "bad", Src: "x"},                    // bad kind → drop
		{Kind: "image", Src: ""},                   // empty src → drop
		{Kind: "doc", Src: "../etc/passwd"},        // traversal → drop
		{Kind: "image", Src: "file:///etc/shadow"}, // other scheme → drop
	}, 5, true, false, "fade")
	if len(info.Items) != 2 {
		t.Fatalf("want 2 valid items, got %d: %+v", len(info.Items), info.Items)
	}
	for _, it := range info.Items {
		if it.ID == "" {
			t.Fatalf("sanitized item missing id: %+v", it)
		}
	}
	if info.IntervalS != 5 || !info.Loop || info.Transition != "fade" {
		t.Fatalf("settings not applied: %+v", info)
	}
}

func TestPlaylistStripsMediaPrefix(t *testing.T) {
	p, _ := newTestPlaylist(t)
	info := p.Set([]PlaylistItem{{Kind: "image", Src: "/media/lobby/a.jpg"}}, 5, true, false, "fade")
	if len(info.Items) != 1 || info.Items[0].Src != "lobby/a.jpg" {
		t.Fatalf("expected /media/ prefix stripped: %+v", info.Items)
	}
}

func TestPlaylistPersists(t *testing.T) {
	p, cfg := newTestPlaylist(t)
	p.Set([]PlaylistItem{{Kind: "image", Src: "a.png"}}, 7, false, true, "none")
	got := NewPlaylist(cfg).Info()
	if len(got.Items) != 1 || got.IntervalS != 7 || got.Loop || !got.Shuffle || got.Transition != "none" {
		t.Fatalf("playlist did not persist correctly: %+v", got)
	}
}
