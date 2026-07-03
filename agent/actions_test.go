package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestActions(t *testing.T) *Actions {
	t.Helper()
	cfg := &Config{StateFile: filepath.Join(t.TempDir(), "display.json")}
	return NewActions(cfg)
}

func testWebMode(url string) Mode {
	return Mode{Type: ModeWeb, Params: map[string]any{"url": url}}
}

func TestActionUpsertAssignsSlugIDIndex(t *testing.T) {
	a := newTestActions(t)
	got, err := a.Upsert(Action{Name: "Lobby Page", Mode: testWebMode("https://example.com")})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got.Slug != "lobby-page" {
		t.Errorf("slug = %q, want lobby-page", got.Slug)
	}
	if got.ID == "" {
		t.Error("id not assigned")
	}
	if got.Index != 0 {
		t.Errorf("index = %d, want 0", got.Index)
	}
}

func TestActionUpsertValidatesMode(t *testing.T) {
	a := newTestActions(t)
	if _, err := a.Upsert(Action{Name: "Bad", Mode: Mode{Type: ModeWeb}}); err == nil {
		t.Fatal("expected error for web mode without url")
	}
	if _, err := a.Upsert(Action{Mode: testWebMode("https://x")}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestActionSlugCollision(t *testing.T) {
	a := newTestActions(t)
	if _, err := a.Upsert(Action{Name: "News", Mode: testWebMode("https://a")}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	dup, err := a.Upsert(Action{Name: "News", Mode: testWebMode("https://b")})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if dup.Slug != "news-2" {
		t.Errorf("derived slug = %q, want news-2", dup.Slug)
	}
	if _, err := a.Upsert(Action{Name: "Other", Slug: "news", Mode: testWebMode("https://c")}); err == nil {
		t.Error("expected error for an explicit duplicate slug")
	}
}

func TestActionUpdateInPlace(t *testing.T) {
	a := newTestActions(t)
	got, _ := a.Upsert(Action{Name: "Lobby", Mode: testWebMode("https://a")})
	got.Name = "Lobby 2"
	got.Mode = testWebMode("https://b")
	up, err := a.Upsert(got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.ID != got.ID {
		t.Errorf("id changed on update: %q vs %q", up.ID, got.ID)
	}
	if n := len(a.List()); n != 1 {
		t.Errorf("want 1 action after in-place update, got %d", n)
	}
	if up.Mode.str("url") != "https://b" {
		t.Errorf("url not updated: %q", up.Mode.str("url"))
	}
}

func TestActionPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{StateFile: filepath.Join(dir, "display.json")}
	a := NewActions(cfg)
	if _, err := a.Upsert(Action{Name: "Keep", Mode: testWebMode("https://x")}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	b := NewActions(cfg)
	if n := len(b.List()); n != 1 {
		t.Fatalf("want 1 after reload, got %d", n)
	}
	if got, ok := b.Get("keep"); !ok || got.Name != "Keep" {
		t.Errorf("get by slug failed: %+v ok=%v", got, ok)
	}
}

func TestActionReorder(t *testing.T) {
	a := newTestActions(t)
	a.Upsert(Action{Name: "A", Mode: testWebMode("https://a")})
	a.Upsert(Action{Name: "B", Mode: testWebMode("https://b")})
	a.Upsert(Action{Name: "C", Mode: testWebMode("https://c")})
	a.Reorder([]string{"c", "a", "b"})
	list := a.List()
	if list[0].Slug != "c" || list[1].Slug != "a" || list[2].Slug != "b" {
		t.Errorf("order = %q,%q,%q", list[0].Slug, list[1].Slug, list[2].Slug)
	}
	for i := range list {
		if list[i].Index != i {
			t.Errorf("index[%d] = %d, want %d", i, list[i].Index, i)
		}
	}
}

func TestActionCurrentSlug(t *testing.T) {
	a := newTestActions(t)
	a.Upsert(Action{Name: "Lobby", Mode: testWebMode("https://lobby")})
	// Same url even with a different dark setting still matches (label-based).
	cur := Mode{Type: ModeWeb, Display: DisplayCompositor, Params: map[string]any{"url": "https://lobby", "dark": false}}
	if got := a.CurrentSlug(cur); got != "lobby" {
		t.Errorf("CurrentSlug = %q, want lobby", got)
	}
	if got := a.CurrentSlug(testWebMode("https://other")); got != "" {
		t.Errorf("CurrentSlug for unknown = %q, want empty", got)
	}
}

func TestActionOrderAfterDeleteThenAdd(t *testing.T) {
	a := newTestActions(t)
	for _, n := range []string{"A", "B", "C", "D", "E"} {
		if _, err := a.Upsert(Action{Name: n, Mode: testWebMode("https://" + n)}); err != nil {
			t.Fatalf("upsert %s: %v", n, err)
		}
	}
	a.Delete("a")
	a.Delete("b")
	if _, err := a.Upsert(Action{Name: "F", Mode: testWebMode("https://F")}); err != nil {
		t.Fatalf("upsert F: %v", err)
	}
	want := []string{"c", "d", "e", "f"} // F must sort LAST, not collide with a survivor's index
	list := a.List()
	if len(list) != len(want) {
		t.Fatalf("len = %d, want %d", len(list), len(want))
	}
	for i := range want {
		if list[i].Slug != want[i] {
			t.Errorf("position %d = %q, want %q (order %v)", i, list[i].Slug, want[i], slugsOf(list))
		}
	}
}

func slugsOf(list []Action) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.Slug
	}
	return out
}

func TestPlaylistActionBuildsShowMode(t *testing.T) {
	a := newTestActions(t)
	got, err := a.Upsert(Action{Name: "Photo Wall", Playlist: &ActionPlaylist{
		Items:     []PlaylistItem{{Kind: "image", Src: "https://x/a.jpg"}},
		IntervalS: 5,
	}})
	if err != nil {
		t.Fatalf("upsert playlist action: %v", err)
	}
	if got.Slug != "photo-wall" {
		t.Errorf("slug = %q, want photo-wall", got.Slug)
	}
	if got.Mode.Type != ModeWeb || !strings.Contains(got.Mode.str("url"), "/show?action=photo-wall") {
		t.Errorf("derived mode = %+v, want web → /show?action=photo-wall", got.Mode)
	}
	pi, ok := a.PlaylistInfo("photo-wall")
	if !ok {
		t.Fatal("PlaylistInfo(photo-wall) not found")
	}
	if len(pi.Items) != 1 || pi.IntervalS != 5 || !strings.Contains(pi.ShowURL, "/show?action=photo-wall") {
		t.Errorf("playlist info = %+v", pi)
	}
	if _, ok := a.PlaylistInfo("nope"); ok {
		t.Error("unknown slug should not resolve")
	}
	if _, err := a.Upsert(Action{Name: "Empty", Playlist: &ActionPlaylist{}}); err == nil {
		t.Error("an empty playlist action should be rejected")
	}
}

func TestActionSeedFromCustomModes(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{StateFile: filepath.Join(dir, "display.json")}
	a := NewActions(cfg)
	a.Seed([]CustomMode{{Name: "htop", Command: "htop", Display: DisplayConsole}})
	list := a.List()
	if len(list) != 1 {
		t.Fatalf("want 1 seeded action, got %d", len(list))
	}
	if list[0].Slug != "htop" || list[0].Mode.Type != ModeApp {
		t.Errorf("bad seed: %+v", list[0])
	}
	// A second construction sees the saved file → no re-seed.
	b := NewActions(cfg)
	b.Seed([]CustomMode{{Name: "extra", Command: "top"}})
	if n := len(b.List()); n != 1 {
		t.Errorf("re-seed happened: got %d actions, want 1", n)
	}
}
