package main

import "testing"

func TestDecodeThrottled(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		want string
	}{
		{"non-pi node", "", ""},
		{"all clear", "0x0", "OK"},
		{"bare zero", "0", "OK"},
		{"undervolt now", "0x1", "now: under-voltage"},
		{"undervolt since boot", "0x10000", "since boot: under-voltage"},
		{"freq capped since boot (disp)", "0x20000", "since boot: ARM frequency capped"},
		{"throttled since boot", "0x40000", "since boot: throttled"},
		{"soft temp since boot", "0x80000", "since boot: soft temperature limit"},
		// A fully-loaded Pi under a weak PSU: undervolt + throttled live, and the
		// same recorded since boot. Worst-first ordering, live before historical.
		{"live and historical", "0x50005", "now: under-voltage, throttled; since boot: under-voltage, throttled"},
		{"all four since boot", "0xF0000", "since boot: under-voltage, throttled, ARM frequency capped, soft temperature limit"},
		// Bits we don't model must fall back to the raw code, not lie.
		{"unknown high bit", "0x100000", "0x100000"},
		// Unparseable hex → parseHexU64 returns 0 → treated as all-clear.
		{"garbage", "nope", "OK"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeThrottled(tt.hex); got != tt.want {
				t.Errorf("decodeThrottled(%q) = %q, want %q", tt.hex, got, tt.want)
			}
		})
	}
}

// decodeThrottled and throttleUnderVolt must agree on what counts as under-voltage
// (bit 0 or bit 16), since the webUI drives its warning highlight off the bool.
func TestThrottleUnderVoltAgreesWithDecode(t *testing.T) {
	for _, hex := range []string{"0x1", "0x10000", "0x10001"} {
		if !throttleUnderVolt(hex) {
			t.Errorf("throttleUnderVolt(%q) = false, want true", hex)
		}
	}
	for _, hex := range []string{"", "0x0", "0x20000", "0x40000"} {
		if throttleUnderVolt(hex) {
			t.Errorf("throttleUnderVolt(%q) = true, want false", hex)
		}
	}
}
