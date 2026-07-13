package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SSHKeys manages the SSH authorized_keys that grant shell access to the node,
// installed from the control UI — the authed Settings panel and, so a fresh
// headless node can be reached at all, the pre-auth first-run wizard. Keys go to
// ROOT's authorized_keys: root login is what lets the operator read the node's
// self-minted /etc/sideshow/agent.key and drive systemd. Installing the first key
// also enables sshd (flashed images ship openssh-server installed but disabled).
//
// SECURITY: exposing "add key" pre-auth (the wizard, while !SetupComplete) lets a
// client on the LAN during the setup window plant its own root key — the same
// persistent-access-surviving-setup risk the tailnet/TLS endpoints deliberately do
// NOT open. It is confined to the setup window and rests on the setup model's "the
// LAN is trusted during onboarding" assumption; the wizard UI warns accordingly.
// Removing a key is always authed (never part of the pre-auth surface).
type SSHKeys struct {
	cfg *Config
	mu  sync.Mutex // serialize read-modify-write of authorized_keys
}

// SSHKey is one installed public key, for display. The public blob is not secret,
// but the UI shows type + fingerprint + comment rather than the raw base64.
type SSHKey struct {
	Type        string `json:"type"`              // ssh-ed25519, ssh-rsa, ecdsa-sha2-nistp256, …
	Fingerprint string `json:"fingerprint"`       // SHA256:… (OpenSSH format)
	Comment     string `json:"comment,omitempty"` // trailing comment, if any
}

// SSHInfo is the GET /api/ssh-keys payload.
type SSHInfo struct {
	Installed bool     `json:"installed"`   // openssh-server present on this node
	Active    bool     `json:"active"`      // the ssh service is running
	Keys      []SSHKey `json:"keys"`        // installed authorized keys
	Target    string   `json:"target"`      // whose authorized_keys (e.g. "root")
}

func NewSSHKeys(cfg *Config) *SSHKeys { return &SSHKeys{cfg: cfg} }

func (s *SSHKeys) path() string {
	if s.cfg != nil && s.cfg.AuthorizedKeysFile != "" {
		return s.cfg.AuthorizedKeysFile
	}
	return "/root/.ssh/authorized_keys"
}

// sshKeyTypes is the set of algorithm names accepted as the FIRST field of a key
// line. Requiring a known type here rejects an options prefix (command="…", no-pty,
// …) outright, so no authorized_keys options can be smuggled in.
var sshKeyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ssh-rsa":                            true,
	"ssh-dss":                            true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// parseSSHPublicKey validates a single authorized_keys public-key line and returns
// its display form plus a canonical "type base64 [comment]" line safe to write. It
// rejects multi-line input, options prefixes, and malformed base64 — the first
// field MUST be a known key type and the base64 blob's embedded algorithm name MUST
// match it — so nothing but a real public key ever reaches authorized_keys. Pure.
func parseSSHPublicKey(line string) (SSHKey, string, error) {
	if strings.ContainsAny(line, "\x00\n\r") {
		return SSHKey{}, "", fmt.Errorf("a key must be a single line")
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return SSHKey{}, "", fmt.Errorf("not an SSH public key (expected: type base64 [comment])")
	}
	ktype, b64 := fields[0], fields[1]
	if !sshKeyTypes[ktype] {
		return SSHKey{}, "", fmt.Errorf("unsupported or malformed key: %q is not an SSH key type (paste the .pub line, no options)", ktype)
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return SSHKey{}, "", fmt.Errorf("invalid key data (not valid base64)")
	}
	if alg, ok := sshBlobAlgorithm(blob); !ok || alg != ktype {
		return SSHKey{}, "", fmt.Errorf("invalid key data (does not match %s)", ktype)
	}
	comment := strings.TrimSpace(strings.Join(fields[2:], " "))
	canonical := ktype + " " + b64
	if comment != "" {
		canonical += " " + comment
	}
	return SSHKey{Type: ktype, Fingerprint: sshFingerprint(blob), Comment: comment}, canonical, nil
}

// sshBlobAlgorithm reads the leading SSH string (uint32 length + bytes) of a
// public-key blob — the algorithm name — bounding the length so junk can't OOM us.
func sshBlobAlgorithm(blob []byte) (string, bool) {
	if len(blob) < 4 {
		return "", false
	}
	n := int(blob[0])<<24 | int(blob[1])<<16 | int(blob[2])<<8 | int(blob[3])
	if n <= 0 || n > 64 || 4+n > len(blob) {
		return "", false
	}
	return string(blob[4 : 4+n]), true
}

// sshFingerprint returns the OpenSSH SHA256 fingerprint of a key blob
// ("SHA256:<base64-no-padding>").
func sshFingerprint(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// Info is the on-demand GET /api/ssh-keys payload (reads the file + forks
// systemctl for liveness — not on the snapshot hot path).
func (s *SSHKeys) Info() SSHInfo {
	keys, _ := s.list()
	return SSHInfo{
		Installed: sshdInstalled(),
		Active:    sshdActive(),
		Keys:      keys,
		Target:    sshTarget(s.path()),
	}
}

func (s *SSHKeys) list() ([]SSHKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parseFile()
}

// parseFile returns the valid public keys in authorized_keys (unknown/comment
// lines are ignored for display but preserved on write).
func (s *SSHKeys) parseFile() ([]SSHKey, error) {
	b, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return []SSHKey{}, nil
		}
		return nil, err
	}
	keys := []SSHKey{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, _, e := parseSSHPublicKey(line); e == nil {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// Add validates a public key and appends it to authorized_keys (idempotent by
// fingerprint), creating the file 0600 under a 0700 dir, then enables sshd.
func (s *SSHKeys) Add(raw string) (SSHKey, error) {
	key, canonical, err := parseSSHPublicKey(raw)
	if err != nil {
		return SSHKey{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path())
	if err != nil && !os.IsNotExist(err) {
		return SSHKey{}, err
	}
	content := string(b)
	for _, line := range strings.Split(content, "\n") {
		if k, _, e := parseSSHPublicKey(line); e == nil && k.Fingerprint == key.Fingerprint {
			return key, nil // already installed → idempotent, no duplicate line
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += canonical + "\n"
	if err := s.write(content); err != nil {
		return SSHKey{}, err
	}
	ensureSSHD()
	log.Printf("[ssh] installed authorized key %s (%s)", key.Fingerprint, key.Comment)
	return key, nil
}

// Remove deletes the key with the given fingerprint, preserving every other line
// (including any manually-added keys or comments). Reports whether it was present.
func (s *SSHKeys) Remove(fingerprint string) (bool, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return false, fmt.Errorf("fingerprint required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	kept := make([]string, 0)
	removed := false
	for _, line := range strings.Split(string(b), "\n") {
		if k, _, e := parseSSHPublicKey(line); e == nil && k.Fingerprint == fingerprint {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	if err := s.write(strings.Join(kept, "\n")); err != nil {
		return false, err
	}
	log.Printf("[ssh] removed authorized key %s", fingerprint)
	return true, nil
}

// write atomically replaces authorized_keys (0600) under a 0700 dir — a torn
// authorized_keys would lock the operator out, so temp-then-rename.
func (s *SSHKeys) write(content string) error {
	p := s.path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// sshTarget names the account whose authorized_keys `path` is, for the UI hint
// (e.g. "/root/.ssh/authorized_keys" → "root").
func sshTarget(path string) string {
	if strings.HasPrefix(path, "/root/") {
		return "root"
	}
	// /home/<user>/.ssh/authorized_keys → <user>
	if rest, ok := strings.CutPrefix(path, "/home/"); ok {
		if i := strings.IndexByte(rest, '/'); i > 0 {
			return rest[:i]
		}
	}
	return ""
}

// sshdInstalled reports whether an SSH server binary is present (so the UI can warn
// when installing a key would be inert).
func sshdInstalled() bool {
	for _, p := range []string{"/usr/sbin/sshd", "/usr/bin/sshd"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return lookPath("sshd")
}

// sshdActive reports whether the SSH service is running (Debian unit "ssh", alias
// "sshd").
func sshdActive() bool {
	if !lookPath("systemctl") {
		return false
	}
	for _, unit := range []string{"ssh", "sshd"} {
		if out, _ := runShort(5*time.Second, "systemctl", "is-active", unit); strings.TrimSpace(out) == "active" {
			return true
		}
	}
	return false
}

// ensureSSHD best-effort enables + starts the SSH daemon so installing a key
// actually makes the node reachable. Flashed images ship openssh-server disabled; a
// node already running sshd is unaffected. Never fatal.
func ensureSSHD() {
	if !lookPath("systemctl") {
		return
	}
	for _, unit := range []string{"ssh", "sshd"} {
		out, err := runShort(15*time.Second, "systemctl", "enable", "--now", unit)
		if err == nil {
			log.Printf("[ssh] enabled %s", unit)
			return
		}
		low := strings.ToLower(out)
		if !strings.Contains(low, "not found") && !strings.Contains(low, "no such") {
			log.Printf("[ssh] could not enable %s: %s", unit, firstLine(out))
			return // a real failure (not just the wrong unit name) → stop
		}
	}
	log.Printf("[ssh] no ssh service to enable (is openssh-server installed?)")
}
