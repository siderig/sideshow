package main

import (
	"strings"
	"time"
)

// readThrottled returns the Pi's vcgencmd get_throttled bitmask as a hex string
// (e.g. "0x0"), or "" if vcgencmd is absent (non-Pi nodes).
func readThrottled() string {
	out, err := runShort(5*time.Second, "vcgencmd", "get_throttled")
	if err != nil {
		return ""
	}
	// Output is "throttled=0x0".
	if i := strings.IndexByte(out, '='); i >= 0 {
		return strings.TrimSpace(out[i+1:])
	}
	return ""
}

// throttleUnderVolt reports whether the throttle mask shows under-voltage either
// right now (bit 0) or at any point since boot (bit 16) — the cheap-PSU warning
// that bit the Pi 3B class of node.
func throttleUnderVolt(hex string) bool {
	if hex == "" {
		return false
	}
	v := parseHexU64(hex)
	return v&0x1 != 0 || v&0x10000 != 0
}
