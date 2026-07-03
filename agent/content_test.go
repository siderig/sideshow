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

func TestSetDocumentValidation(t *testing.T) {
	c := testContent(t)

	// An http(s) URL passes through unchanged.
	info, err := c.SetDocument("https://ex/deck.pdf", 0, true)
	if err != nil {
		t.Fatalf("SetDocument(http): %v", err)
	}
	if info.Src != "https://ex/deck.pdf" || !info.Enabled {
		t.Errorf("http document: %+v", info)
	}

	// A traversal path is rejected.
	if _, err := c.SetDocument("../../etc/passwd", 0, true); err == nil {
		t.Error("traversal document path should error")
	}

	// A non-http(s) scheme is rejected.
	if _, err := c.SetDocument("file:///etc/passwd", 0, true); err == nil {
		t.Error("file:// document src should error")
	}

	// Disabling with an empty src is fine.
	if _, err := c.SetDocument("", 0, false); err != nil {
		t.Errorf("disabling document should not error: %v", err)
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

func TestDocumentAndReloadPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{StateFile: filepath.Join(dir, "display.json"), DocsDir: dir}
	c := NewContent(cfg, testSupervisor(t))

	if _, err := c.SetDocument("https://ex/deck.pdf", 12, true); err != nil {
		t.Fatalf("SetDocument: %v", err)
	}
	c.SetReload(15)

	// Re-load from the same dir.
	c2 := NewContent(cfg, testSupervisor(t))
	got := c2.DocInfo()
	if !got.Enabled || got.Src != "https://ex/deck.pdf" || got.AutoAdvanceS != 12 {
		t.Errorf("document did not survive reload: %+v", got)
	}
	if c2.Info().ReloadMin != 15 {
		t.Errorf("reload did not survive reload: %d", c2.Info().ReloadMin)
	}
}

// DisableOwners clears the document page-owner so a direct navigation / mode
// switch takes the screen without a timer re-asserting the document.
func TestDisableOwnersClearsDocument(t *testing.T) {
	c := testContent(t)
	if _, err := c.SetDocument("https://ex/deck.pdf", 0, true); err != nil {
		t.Fatalf("SetDocument: %v", err)
	}
	if !c.DocInfo().Enabled {
		t.Fatal("document should be enabled")
	}
	c.DisableOwners()
	if c.DocInfo().Enabled {
		t.Error("DisableOwners should clear the document owner")
	}
}
