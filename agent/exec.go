package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// runNmcli runs nmcli under the C.UTF-8 locale so its status and error messages
// are always the C-locale English the output classifiers match (wifiNotInRange,
// wifiAuthFailure, normalizeSecurity) — regardless of the node's system locale, on
// which a Finnish/other translation would silently defeat the substring matching —
// while keeping a UTF-8 charset so non-ASCII SSIDs print as their real bytes.
// Plain LC_ALL=C would make nmcli substitute every non-ASCII SSID character with
// '?' on output (e.g. "Jyväskylän" → "Jyv?skyl?n"), mangling the name shown in the
// UI and, worse, the name the Forget/Connect round-trip sends back.
func runNmcli(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nmcli", args...)
	cmd.Env = nmcliEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// nmcliEnv forces the C.UTF-8 locale (English, untranslated messages + a UTF-8
// charset). Any inherited LANG/LANGUAGE/LC_* is stripped first so neither the
// node's system locale nor a LANGUAGE= translation can leak back in — gettext
// still honors LANGUAGE under C.UTF-8 (it only ignores it for exactly C/POSIX).
func nmcliEnv() []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if k == "LANG" || k == "LANGUAGE" || strings.HasPrefix(k, "LC_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "LC_ALL=C.UTF-8", "LANGUAGE=")
}

// runShort runs an external command bounded by a timeout and returns its trimmed
// combined output. Used for the agent's node-control shell-outs (vcgencmd,
// xrandr, cec-ctl, apt) — all expected to answer quickly; a wedged one is killed
// at the deadline rather than hanging an HTTP handler.
func runShort(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// parseHexU64 parses a "0x…"/bare-hex string to uint64 (0 on error).
func parseHexU64(s string) uint64 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0
	}
	return v
}
