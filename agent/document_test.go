package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDocSrc(t *testing.T) {
	docs := t.TempDir()
	// A real file inside the docs dir.
	if err := os.WriteFile(filepath.Join(docs, "deck.pdf"), []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file with a space in its name (must be URL-escaped).
	if err := os.WriteFile(filepath.Join(docs, "my deck.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A nested file.
	if err := os.MkdirAll(filepath.Join(docs, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "sub", "n.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink that escapes the docs dir.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.pdf")
	if err := os.WriteFile(secret, []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(secret, filepath.Join(docs, "escape.pdf"))

	httpOK := func(src string) {
		t.Helper()
		got, err := validateDocSrc(docs, src)
		if err != nil || got != src {
			t.Errorf("validateDocSrc(%q) = (%q,%v), want passthrough", src, got, err)
		}
	}
	httpOK("http://ex/a.pdf")
	httpOK("https://ex/a.pdf")

	// Allowed relative path → /docfs/<rel>.
	if got, err := validateDocSrc(docs, "deck.pdf"); err != nil || got != "/docfs/deck.pdf" {
		t.Errorf("deck.pdf = (%q,%v), want /docfs/deck.pdf", got, err)
	}
	// Space encoded.
	if got, err := validateDocSrc(docs, "my deck.pdf"); err != nil || !strings.Contains(got, "%20") {
		t.Errorf("space file = (%q,%v), want %%20-encoded", got, err)
	}
	if got, err := validateDocSrc(docs, "sub/n.pdf"); err != nil || got != "/docfs/sub/n.pdf" {
		t.Errorf("nested = (%q,%v), want /docfs/sub/n.pdf", got, err)
	}

	// Rejections.
	reject := func(name, src string) {
		t.Helper()
		if _, err := validateDocSrc(docs, src); err == nil {
			t.Errorf("%s: validateDocSrc(%q) should error", name, src)
		}
	}
	reject("traversal", "../etc/passwd")
	reject("nested traversal", "sub/../../etc/passwd")
	reject("absolute", "/etc/passwd")
	reject("file scheme", "file:///etc/passwd")
	reject("ftp scheme", "ftp://ex/a")
	reject("missing", "nope.pdf")
	reject("symlink escape", "escape.pdf")

	// Empty docsDir + a path (not a URL) → reject.
	if _, err := validateDocSrc("", "deck.pdf"); err == nil {
		t.Error("empty docsDir with a path should error")
	}
	// Empty src → ("",nil).
	if got, err := validateDocSrc(docs, ""); err != nil || got != "" {
		t.Errorf("empty src = (%q,%v), want (\"\",nil)", got, err)
	}
}

func TestSafeDocRel(t *testing.T) {
	if _, err := safeDocRel("a/b.pdf"); err != nil {
		t.Errorf("a/b.pdf should be allowed: %v", err)
	}
	if got, _ := safeDocRel("/docfs/a/b.pdf"); got != "a/b.pdf" {
		t.Errorf("safeDocRel should strip /docfs/ prefix, got %q", got)
	}
	for _, bad := range []string{"", "/abs", "../x", "a/../../x", ".."} {
		if _, err := safeDocRel(bad); err == nil {
			t.Errorf("safeDocRel(%q) should error", bad)
		}
	}
}

func TestWithPDFViewerParams(t *testing.T) {
	got := withPDFViewerParams("/docfs/a.pdf")
	if !strings.Contains(got, "#") || !strings.Contains(got, "toolbar=0") {
		t.Errorf("withPDFViewerParams = %q, want a viewer hash", got)
	}
	// An existing fragment is never clobbered.
	if got := withPDFViewerParams("http://ex/a.pdf#page=3"); got != "http://ex/a.pdf#page=3" {
		t.Errorf("existing fragment clobbered: %q", got)
	}
	if got := withPDFViewerParams(""); got != "" {
		t.Errorf("empty in → empty out, got %q", got)
	}
}
