package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

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
