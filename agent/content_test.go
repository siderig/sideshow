package main

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func testContent(t *testing.T) *Content {
	t.Helper()
	dir := t.TempDir()
	return NewContent(&Config{StateFile: filepath.Join(dir, "display.json"), DocsDir: dir}, testSupervisor(t))
}

func TestSetSlideshowValidation(t *testing.T) {
	c := testContent(t)

	// Enabling with no valid images → 400.
	if _, err := c.SetSlideshow([]string{"not-a-url", "relative/path"}, 6, "", "", true); err == nil {
		t.Error("enabling slideshow with no valid images should error")
	}
	if _, err := c.SetSlideshow(nil, 6, "", "", true); err == nil {
		t.Error("enabling slideshow with empty images should error")
	}
	// Disabling with empty images is fine.
	if _, err := c.SetSlideshow(nil, 6, "", "", false); err != nil {
		t.Errorf("disabling slideshow should not error: %v", err)
	}

	// Normalization: interval<=0 → default, fit COVER→cover, junk transition→fade.
	// Local paths are rejected (http(s) only): "/srv/a.png" is dropped.
	info, err := c.SetSlideshow([]string{"/srv/a.png", "https://ex/b.png"}, 0, "COVER", "wobble", true)
	if err != nil {
		t.Fatalf("SetSlideshow: %v", err)
	}
	if info.IntervalS != defaultSlideInterval {
		t.Errorf("interval = %d, want default %d", info.IntervalS, defaultSlideInterval)
	}
	if info.Fit != "cover" {
		t.Errorf("fit = %q, want cover", info.Fit)
	}
	if info.Transition != "fade" {
		t.Errorf("transition = %q, want fade", info.Transition)
	}
	if len(info.Images) != 1 || info.Images[0] != "https://ex/b.png" {
		t.Errorf("images = %v, want only the http(s) URL kept (local path rejected)", info.Images)
	}
}

// TestOutputContentConcurrentSaveNoRace guards the fix for the fatal "concurrent
// map read and map write" in Content.save(): with no display wired, every
// SetOutputContent takes the persist-only (deferred) path, so we can hammer the
// outputContent map alongside save() under -race.
func TestOutputContentConcurrentSaveNoRace(t *testing.T) {
	c := testContent(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 60; j++ {
				_ = c.SetOutputContent(fmt.Sprintf("OUT-%d", i), OutputContent{Type: "web", URL: "https://e/x"})
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 120; j++ {
				c.save()
			}
		}()
	}
	wg.Wait()
}

func TestLocalAgentBaseAndAbsDocURL(t *testing.T) {
	cases := map[string]string{
		":80":          "http://127.0.0.1:80",
		"0.0.0.0:8080": "http://127.0.0.1:8080",
		"80":           "http://127.0.0.1:80", // no colon → default port
	}
	for in, want := range cases {
		if got := localAgentBase(in); got != want {
			t.Errorf("localAgentBase(%q) = %q, want %q", in, got, want)
		}
	}
	// A local /docfs path becomes an absolute loopback URL the kiosk can fetch; an
	// http(s) source is left untouched.
	c := testContent(t) // localBase derived from an empty Addr → :80
	if got := c.absDocURL("/docfs/a.pdf"); got != "http://127.0.0.1:80/docfs/a.pdf" {
		t.Errorf("absDocURL(/docfs/a.pdf) = %q", got)
	}
	if got := c.absDocURL("https://x/y.pdf"); got != "https://x/y.pdf" {
		t.Errorf("absDocURL(http) = %q, want unchanged", got)
	}
}

func TestSlideshowPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{StateFile: filepath.Join(dir, "display.json"), DocsDir: dir}
	c := NewContent(cfg, testSupervisor(t))

	if _, err := c.SetSlideshow([]string{"https://ex/a.png"}, 9, "cover", "none", true); err != nil {
		t.Fatalf("SetSlideshow: %v", err)
	}
	c.SetReload(15) // coexists with the slideshow block

	// Re-load from the same dir.
	c2 := NewContent(cfg, testSupervisor(t))
	got := c2.SlideshowInfo()
	if !got.Enabled || got.IntervalS != 9 || got.Fit != "cover" || got.Transition != "none" {
		t.Errorf("slideshow did not survive reload: %+v", got)
	}
	if len(got.Images) != 1 || got.Images[0] != "https://ex/a.png" {
		t.Errorf("images did not survive reload: %v", got.Images)
	}
	if c2.Info().ReloadMin != 15 {
		t.Errorf("reload did not survive reload: %d", c2.Info().ReloadMin)
	}
}

// Enabling slideshow/document should disable the playlist and vice versa, so a
// single page owner is enforced.
func TestContentMutualExclusion(t *testing.T) {
	c := testContent(t)
	if err := c.SetPlaylist([]string{"https://ex/1", "https://ex/2"}, 30, true); err != nil {
		t.Fatalf("SetPlaylist: %v", err)
	}
	if !c.Info().Enabled {
		t.Fatal("playlist should be enabled")
	}
	if _, err := c.SetSlideshow([]string{"https://ex/a.png"}, 6, "", "", true); err != nil {
		t.Fatalf("SetSlideshow: %v", err)
	}
	if c.Info().Enabled {
		t.Error("enabling slideshow should disable the playlist")
	}
	if !c.SlideshowInfo().Enabled {
		t.Error("slideshow should be enabled")
	}
	// Re-enabling the playlist disables the slideshow.
	if err := c.SetPlaylist([]string{"https://ex/1", "https://ex/2"}, 30, true); err != nil {
		t.Fatalf("SetPlaylist: %v", err)
	}
	if c.SlideshowInfo().Enabled {
		t.Error("re-enabling the playlist should disable the slideshow")
	}
}
