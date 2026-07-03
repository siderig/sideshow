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

// throttleBits pairs a "currently active" mask (bits 0-3) with its human label;
// the matching "has occurred since boot" bit sits 16 places higher (bits 16-19).
// Order is worst-first so the decoded summary leads with the most serious cause.
var throttleBits = []struct {
	bit  uint64
	name string
}{
	{0x1, "under-voltage"},
	{0x4, "throttled"},
	{0x2, "ARM frequency capped"},
	{0x8, "soft temperature limit"},
}

// decodeThrottled turns a vcgencmd get_throttled bitmask (e.g. "0x20000") into a
// short human summary for the webUI's Power stat, so an operator sees words
// instead of a hex code. "" in → "" out (a non-Pi node with no vcgencmd); "0x0"
// → "OK". Live conditions (bits 0-3) are separated from the "has occurred since
// boot" bits (16-19) so a past blip like 0x20000 reads as history, not a live
// fault. Unknown bits fall back to the raw hex rather than silently dropping.
// Ref: https://www.raspberrypi.com/documentation/computers/os.html#get_throttled
func decodeThrottled(hex string) string {
	if hex == "" {
		return ""
	}
	v := parseHexU64(hex)
	if v == 0 {
		return "OK"
	}
	var now, past []string
	known := uint64(0)
	for _, b := range throttleBits {
		if v&b.bit != 0 {
			now = append(now, b.name)
		}
		if v&(b.bit<<16) != 0 {
			past = append(past, b.name)
		}
		known |= b.bit | (b.bit << 16)
	}
	var parts []string
	if len(now) > 0 {
		parts = append(parts, "now: "+strings.Join(now, ", "))
	}
	if len(past) > 0 {
		parts = append(parts, "since boot: "+strings.Join(past, ", "))
	}
	if v&^known != 0 || len(parts) == 0 {
		// Bits we don't recognise — surface the raw code rather than lie.
		return hex
	}
	return strings.Join(parts, "; ")
}
