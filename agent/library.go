package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Library is the node's uploadable media store: a directory tree under -media-dir
// holding images, videos, audio, and documents the operator uploads and arranges
// into playlists. It is the file-manager backend (list/upload/mkdir/rename/delete)
// and the source for /media/<path> serving. All paths are hardened against
// traversal + symlink escape via the same guards the document viewer uses.
type Library struct {
	root string
}

// LibEntry is one file or folder in a listing.
type LibEntry struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // dir | image | video | audio | doc | other
	Size  int64  `json:"size,omitempty"`
	MTime string `json:"mtime"`
	IsDir bool   `json:"is_dir"`
}

// LibListing is the JSON for GET /api/library.
type LibListing struct {
	Path    string     `json:"path"` // the listed folder, relative to the root ("" = root)
	Entries []LibEntry `json:"entries"`
}

func NewLibrary(cfg *Config) *Library {
	l := &Library{root: cfg.MediaDir}
	if l.root != "" {
		if err := os.MkdirAll(l.root, 0o755); err != nil {
			l.root = "" // disabled if we can't create it
		}
	}
	return l
}

func (l *Library) enabled() bool { return l.root != "" }

// libKind classifies a filename by extension for the UI (icon + how a playlist
// should render it).
func libKind(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg", ".avif":
		return "image"
	case ".mp4", ".webm", ".mkv", ".mov", ".avi", ".m4v", ".ogv":
		return "video"
	case ".mp3", ".m4a", ".aac", ".ogg", ".oga", ".wav", ".flac", ".opus":
		return "audio"
	case ".pdf":
		return "doc"
	default:
		return "other"
	}
}

// safeName reduces a caller-supplied file/folder name to a single, safe path
// component: no directory separators, no traversal, no leading dot. Returns ""
// when nothing usable remains.
func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "/", "")
	name = strings.ReplaceAll(name, string(filepath.Separator), "")
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return "" // no empty, no traversal, no hidden dotfiles
	}
	return name
}

// resolve maps a root-relative library path to its absolute path, guaranteed to
// stay under the (symlink-resolved) root. It does NOT require the target to exist
// — a rename/upload destination resolves too — because it joins the cleaned
// relative path (traversal already rejected by safeDocRel) onto the resolved
// root, then, if the target exists, follows its symlinks and re-checks
// containment (defeating a symlink that points outside). "" / "." = the root.
func (l *Library) resolve(rel string) (string, error) {
	if !l.enabled() {
		return "", fmt.Errorf("media library not configured")
	}
	realRoot, err := filepath.EvalSymlinks(l.root)
	if err != nil {
		realRoot = filepath.Clean(l.root)
	}
	rel = strings.Trim(strings.TrimSpace(rel), "/")
	if rel == "" || rel == "." {
		return realRoot, nil
	}
	clean, err := safeDocRel(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(realRoot, clean)
	// Verify containment against symlinks. If the target exists, resolve it fully;
	// otherwise (a rename/upload destination) resolve the deepest EXISTING ancestor
	// and check that — so a symlinked PARENT directory can't let a not-yet-existing
	// leaf escape the root.
	check := abs
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		check = real
	} else {
		anc := abs
		for {
			parent := filepath.Dir(anc)
			if parent == anc {
				break // reached the filesystem root
			}
			if real, err := filepath.EvalSymlinks(parent); err == nil {
				check = filepath.Join(real, abs[len(parent):]) // resolved ancestor + the remaining tail
				break
			}
			anc = parent
		}
	}
	if check != realRoot && !strings.HasPrefix(check, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the media library")
	}
	return abs, nil
}

// dirAbs resolves rel and requires it to be an existing directory.
func (l *Library) dirAbs(rel string) (string, error) {
	abs, err := l.resolve(rel)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("no such folder: %s", rel)
	}
	return abs, nil
}

// itemAbs resolves a file-or-folder path that need not exist (a create/move dest).
func (l *Library) itemAbs(rel string) (string, error) { return l.resolve(rel) }

// List returns the entries of a folder (dirs first, then name), never escaping
// the root.
func (l *Library) List(rel string) (LibListing, error) {
	abs, err := l.dirAbs(rel)
	if err != nil {
		return LibListing{}, err
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return LibListing{}, err
	}
	out := LibListing{Path: strings.Trim(rel, "/"), Entries: []LibEntry{}}
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := LibEntry{Name: de.Name(), IsDir: de.IsDir(), MTime: info.ModTime().UTC().Format(time.RFC3339)}
		if de.IsDir() {
			e.Kind = "dir"
		} else {
			e.Kind = libKind(de.Name())
			e.Size = info.Size()
		}
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].IsDir != out.Entries[j].IsDir {
			return out.Entries[i].IsDir // dirs first
		}
		return strings.ToLower(out.Entries[i].Name) < strings.ToLower(out.Entries[j].Name)
	})
	return out, nil
}

// SaveFile streams an uploaded file into folder dirRel under name, capped at max
// bytes. Streaming (io.Copy) keeps a large video off the heap on a low-RAM node.
// Returns the bytes written; a truncation at the cap is reported as an error and
// the partial file is removed.
func (l *Library) SaveFile(dirRel, name string, r io.Reader, max int64) (int64, error) {
	dir, err := l.dirAbs(dirRel)
	if err != nil {
		return 0, err
	}
	name = safeName(name)
	if name == "" {
		return 0, fmt.Errorf("invalid file name")
	}
	dst := filepath.Join(dir, name)
	tmp := dst + ".uploading"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	// Read one byte past the cap so a file exactly at the cap succeeds but a
	// larger one is detected and rejected.
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if closeErr != nil {
		os.Remove(tmp)
		return 0, closeErr
	}
	if n > max {
		os.Remove(tmp)
		return 0, fmt.Errorf("file exceeds the %d MiB upload limit", max/(1<<20))
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// Mkdir creates a subfolder `name` inside dirRel.
func (l *Library) Mkdir(dirRel, name string) error {
	dir, err := l.dirAbs(dirRel)
	if err != nil {
		return err
	}
	name = safeName(name)
	if name == "" {
		return fmt.Errorf("invalid folder name")
	}
	return os.Mkdir(filepath.Join(dir, name), 0o755)
}

// Rename moves/renames an item. `from` and `to` are BOTH root-relative paths, so
// the caller controls the destination unambiguously: a rename in place is the
// same folder + a new leaf ("lobby/a.png" → "lobby/b.png"); a move to the root is
// just "b.png". Both are traversal/symlink-checked and must stay under the root.
func (l *Library) Rename(from, to string) error {
	src, err := l.itemAbs(from)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("no such item: %s", from)
	}
	dst, err := l.itemAbs(to)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("target already exists")
	}
	return os.Rename(src, dst)
}

// Delete removes a file, or a folder (recursively only when recurse is set).
func (l *Library) Delete(rel string, recurse bool) error {
	if strings.Trim(strings.TrimSpace(rel), "/") == "" {
		return fmt.Errorf("cannot delete the library root")
	}
	abs, err := l.itemAbs(rel)
	if err != nil {
		return err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("no such item: %s", rel)
	}
	if fi.IsDir() && recurse {
		return os.RemoveAll(abs)
	}
	return os.Remove(abs) // fails on a non-empty dir unless recurse
}

// Resolve maps a relative library path to an absolute file path for serving,
// verifying it is an existing regular file under the root.
func (l *Library) Resolve(rel string) (string, error) {
	abs, err := l.itemAbs(rel)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		return "", fmt.Errorf("no such file: %s", rel)
	}
	return abs, nil
}
