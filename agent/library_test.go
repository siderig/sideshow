package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLib(t *testing.T) *Library {
	t.Helper()
	return NewLibrary(&Config{MediaDir: t.TempDir()})
}

func TestLibrarySaveAndList(t *testing.T) {
	l := newTestLib(t)
	if _, err := l.SaveFile("", "intro.mp4", strings.NewReader("video-bytes"), 1<<20); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := l.SaveFile("", "photo.jpg", strings.NewReader("img"), 1<<20); err != nil {
		t.Fatalf("save: %v", err)
	}
	list, err := l.List("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(list.Entries))
	}
	kinds := map[string]string{}
	for _, e := range list.Entries {
		kinds[e.Name] = e.Kind
	}
	if kinds["intro.mp4"] != "video" || kinds["photo.jpg"] != "image" {
		t.Fatalf("wrong kinds: %+v", kinds)
	}
}

func TestLibraryMkdirRenameDelete(t *testing.T) {
	l := newTestLib(t)
	if err := l.Mkdir("", "lobby"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := l.SaveFile("lobby", "a.png", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatalf("save in subdir: %v", err)
	}
	// rename in place (destination is a full root-relative path)
	if err := l.Rename("lobby/a.png", "lobby/b.png"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := l.Resolve("lobby/b.png"); err != nil {
		t.Fatalf("resolve renamed: %v", err)
	}
	// move to root
	if err := l.Rename("lobby/b.png", "b.png"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := l.Resolve("b.png"); err != nil {
		t.Fatalf("resolve moved: %v", err)
	}
	// deleting a non-empty dir without recurse fails; empty dir ok
	if err := l.Delete("lobby", false); err != nil {
		t.Fatalf("delete now-empty dir: %v", err)
	}
	if err := l.Delete("b.png", false); err != nil {
		t.Fatalf("delete file: %v", err)
	}
	if _, err := l.Resolve("b.png"); err == nil {
		t.Fatal("deleted file should not resolve")
	}
}

func TestLibraryDeleteNonEmptyDirNeedsRecurse(t *testing.T) {
	l := newTestLib(t)
	l.Mkdir("", "d")
	l.SaveFile("d", "f.txt", strings.NewReader("x"), 1<<20)
	if err := l.Delete("d", false); err == nil {
		t.Fatal("deleting a non-empty dir without recurse should fail")
	}
	if err := l.Delete("d", true); err != nil {
		t.Fatalf("recursive delete: %v", err)
	}
}

func TestLibraryRejectsTraversal(t *testing.T) {
	l := newTestLib(t)
	for _, p := range []string{"../", "..", "../etc", "a/../../b"} {
		if _, err := l.List(p); err == nil {
			t.Errorf("List(%q) should be rejected", p)
		}
	}
	if err := l.Mkdir("..", "x"); err == nil {
		t.Error("mkdir into parent should be rejected")
	}
	if err := l.Delete("../secret", false); err == nil {
		t.Error("delete via traversal should be rejected")
	}
}

func TestLibrarySaveFileNameCannotEscape(t *testing.T) {
	l := newTestLib(t)
	// a filename full of traversal collapses to its base — it must land INSIDE the
	// library, never at ../../etc/passwd.
	if _, err := l.SaveFile("", "../../etc/passwd", strings.NewReader("pwned"), 1<<20); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := l.Resolve("passwd"); err != nil {
		t.Fatalf("file should have landed as passwd inside the root: %v", err)
	}
	// and the real /etc must be untouched (belt-and-suspenders: the root is a temp dir)
	if strings.Contains(l.root, "etc") {
		t.Skip("temp root oddly under etc")
	}
}

func TestLibrarySymlinkEscapeRejected(t *testing.T) {
	l := newTestLib(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a symlink inside the library pointing outside must not let Resolve escape.
	if err := os.Symlink(outside, filepath.Join(l.root, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := l.Resolve("link/secret"); err == nil {
		t.Fatal("resolving through a symlink out of the root must be rejected")
	}
}

func TestLibraryRenameThroughSymlinkParentRejected(t *testing.T) {
	l := newTestLib(t)
	outside := t.TempDir()
	if _, err := l.SaveFile("", "src.txt", strings.NewReader("x"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.root, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	// The destination leaf does not exist yet, but its parent ("link") points
	// outside the root — the rename must be rejected, not follow the symlink.
	if err := l.Rename("src.txt", "link/escaped.txt"); err == nil {
		t.Fatal("rename through a symlinked parent must be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); err == nil {
		t.Fatal("a file escaped the media library via the symlinked parent")
	}
}

func TestLibrarySafeNameRejectsDotfiles(t *testing.T) {
	for _, n := range []string{".env", ".htaccess", ".", "..", ""} {
		if safeName(n) != "" {
			t.Errorf("safeName(%q) should be rejected, got %q", n, safeName(n))
		}
	}
	if safeName("photo.jpg") != "photo.jpg" {
		t.Error("a normal filename should pass safeName")
	}
	if _, err := newTestLib(t).SaveFile("", ".secret", strings.NewReader("x"), 1<<20); err == nil {
		t.Fatal("uploading a dotfile should be rejected")
	}
}

func TestLibraryUploadCap(t *testing.T) {
	l := newTestLib(t)
	// 100 bytes with a 10-byte cap → rejected, and no partial file left behind.
	_, err := l.SaveFile("", "big.bin", strings.NewReader(strings.Repeat("x", 100)), 10)
	if err == nil {
		t.Fatal("oversized upload should be rejected")
	}
	if _, rerr := l.Resolve("big.bin"); rerr == nil {
		t.Fatal("a rejected upload must not leave a partial file")
	}
}

func TestLibKind(t *testing.T) {
	cases := map[string]string{"a.JPG": "image", "b.mp4": "video", "c.MP3": "audio", "d.pdf": "doc", "e.txt": "other"}
	for name, want := range cases {
		if got := libKind(name); got != want {
			t.Errorf("libKind(%q)=%q want %q", name, got, want)
		}
	}
}
