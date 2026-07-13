package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sshBlob builds a well-formed SSH public-key blob (uint32-length-prefixed strings)
// for the given algorithm + a fixed-size key body filled with `fill`, so tests use
// real structure — and distinct keys (distinct fingerprints) — without a crypto dep.
func sshBlob(alg string, keyLen int, fill byte) []byte {
	var out []byte
	put := func(b []byte) {
		n := len(b)
		out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		out = append(out, b...)
	}
	put([]byte(alg))
	body := make([]byte, keyLen)
	for i := range body {
		body[i] = fill
	}
	put(body)
	return out
}

func ed25519Line(comment string, fill byte) string {
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(sshBlob("ssh-ed25519", 32, fill))
	if comment != "" {
		line += " " + comment
	}
	return line
}

func TestParseSSHPublicKey(t *testing.T) {
	key, canonical, err := parseSSHPublicKey(ed25519Line("alice@laptop", 0))
	if err != nil {
		t.Fatalf("valid ed25519 key rejected: %v", err)
	}
	if key.Type != "ssh-ed25519" || key.Comment != "alice@laptop" {
		t.Errorf("parsed = %+v, want type ssh-ed25519 comment alice@laptop", key)
	}
	if !strings.HasPrefix(key.Fingerprint, "SHA256:") || len(key.Fingerprint) < 20 {
		t.Errorf("fingerprint = %q, want a SHA256:… value", key.Fingerprint)
	}
	if !strings.HasPrefix(canonical, "ssh-ed25519 ") || !strings.HasSuffix(canonical, " alice@laptop") {
		t.Errorf("canonical = %q, want normalized type/base64/comment", canonical)
	}
	// A key with no comment round-trips without a trailing space.
	if _, c2, err := parseSSHPublicKey(ed25519Line("", 0)); err != nil || strings.HasSuffix(c2, " ") {
		t.Errorf("no-comment canonical = %q err=%v", c2, err)
	}

	b64 := base64.StdEncoding.EncodeToString(sshBlob("ssh-ed25519", 32, 0))
	bad := map[string]string{
		"empty":            "",
		"one field":        "ssh-ed25519",
		"options prefix":   `command="rm -rf /" ` + ed25519Line("", 0), // options must be rejected
		"no-pty option":    "no-pty " + ed25519Line("", 0),
		"unknown type":     "ssh-magic " + b64,
		"bad base64":       "ssh-ed25519 not-base64!!",
		"algo mismatch":    "ssh-rsa " + b64, // blob says ed25519, line claims rsa
		"embedded newline": "ssh-ed25519 " + b64 + "\nssh-ed25519 " + b64,
		"carriage return":  "ssh-ed25519 " + b64 + "\r",
	}
	for name, line := range bad {
		if _, _, err := parseSSHPublicKey(line); err == nil {
			t.Errorf("%s: expected rejection, got none for %q", name, line)
		}
	}
}

func TestSSHFingerprintStable(t *testing.T) {
	blob := sshBlob("ssh-ed25519", 32, 7)
	if a, b := sshFingerprint(blob), sshFingerprint(blob); a != b {
		t.Errorf("fingerprint not deterministic: %q vs %q", a, b)
	}
	if strings.HasSuffix(sshFingerprint(blob), "=") {
		t.Error("fingerprint should be unpadded base64")
	}
	// Distinct key bodies must give distinct fingerprints.
	if sshFingerprint(sshBlob("ssh-ed25519", 32, 1)) == sshFingerprint(sshBlob("ssh-ed25519", 32, 2)) {
		t.Error("different keys collided on fingerprint")
	}
}

func TestSSHTarget(t *testing.T) {
	cases := map[string]string{
		"/root/.ssh/authorized_keys":          "root",
		"/home/sideshow/.ssh/authorized_keys": "sideshow",
		"/etc/somewhere/keys":                 "",
	}
	for path, want := range cases {
		if got := sshTarget(path); got != want {
			t.Errorf("sshTarget(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSSHKeysAddListRemove(t *testing.T) {
	dir := t.TempDir()
	akf := filepath.Join(dir, ".ssh", "authorized_keys")
	// A pre-existing, manually-added key must survive add/remove of others.
	if err := os.MkdirAll(filepath.Dir(akf), 0o700); err != nil {
		t.Fatal(err)
	}
	manual := ed25519Line("preexisting@host", 1)
	if err := os.WriteFile(akf, []byte(manual+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSSHKeys(&Config{AuthorizedKeysFile: akf})

	added, err := s.Add(ed25519Line("bob@desktop", 2))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	keys, _ := s.list()
	if len(keys) != 2 {
		t.Fatalf("after add: %d keys, want 2 (preexisting + bob)", len(keys))
	}
	// Idempotent: re-adding the same key (same body) does not duplicate it.
	if _, err := s.Add(ed25519Line("bob-again", 2)); err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	if keys, _ := s.list(); len(keys) != 2 {
		t.Errorf("re-add duplicated the key: %d keys, want 2", len(keys))
	}
	// File perms must be 0600 (a world-readable authorized_keys is ignored by sshd).
	if fi, err := os.Stat(akf); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys perms = %v (err %v), want 0600", fi.Mode().Perm(), err)
	}
	// Remove the added key by fingerprint; the preexisting one stays.
	ok, err := s.Remove(added.Fingerprint)
	if err != nil || !ok {
		t.Fatalf("Remove: ok=%v err=%v", ok, err)
	}
	keys, _ = s.list()
	if len(keys) != 1 || keys[0].Comment != "preexisting@host" {
		t.Errorf("after remove: %+v, want only the preexisting key", keys)
	}
	// Removing an absent fingerprint reports not-found without erroring.
	if ok, err := s.Remove("SHA256:doesnotexist"); ok || err != nil {
		t.Errorf("Remove(absent) = ok=%v err=%v, want false/nil", ok, err)
	}
	// A rejected key never touches the file.
	if _, err := s.Add("garbage not a key"); err == nil {
		t.Error("Add(garbage) should error")
	}
	if keys, _ := s.list(); len(keys) != 1 {
		t.Errorf("a rejected add changed the file: %d keys", len(keys))
	}
}

func TestSSHKeysMissingFile(t *testing.T) {
	// A node with no authorized_keys yet → empty list, no error.
	s := NewSSHKeys(&Config{AuthorizedKeysFile: filepath.Join(t.TempDir(), "nope", "authorized_keys")})
	if keys, err := s.list(); err != nil || len(keys) != 0 {
		t.Errorf("list(missing) = %v, %v; want [], nil", keys, err)
	}
}
