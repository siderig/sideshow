package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// runNmcli runs nmcli under LC_ALL=C so its status and error messages are always
// the C-locale English the output classifiers match (wifiNotInRange,
// wifiAuthFailure, normalizeSecurity) — regardless of the node's system locale, on
// which a Finnish/other translation would silently defeat the substring matching.
// SSID/PSK data is byte-passthrough and unaffected by the locale.
func runNmcli(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nmcli", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
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
