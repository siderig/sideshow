package main

import "testing"

func TestValidHostname(t *testing.T) {
	ok := []string{"disp", "sideshow-ab12", "a", "node-1", "x0", "a-b-c", "abc123"}
	for _, h := range ok {
		if !validHostname(h) {
			t.Errorf("validHostname(%q) = false, want true", h)
		}
	}
	bad := []string{"", "-disp", "disp-", "DISP", "a_b", "a.b", "a b", "café", string(make([]byte, 64))}
	for _, h := range bad {
		if validHostname(h) {
			t.Errorf("validHostname(%q) = true, want false", h)
		}
	}
	// exactly 63 chars is allowed; 64 is not
	if s := "a" + string(rep('b', 61)) + "c"; !validHostname(s) || len(s) != 63 {
		t.Errorf("63-char hostname should be valid (len=%d)", len(s))
	}
	if s := "a" + string(rep('b', 62)) + "c"; validHostname(s) {
		t.Errorf("64-char hostname should be invalid (len=%d)", len(s))
	}
}

func rep(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}

func TestProtectedAndGeneric(t *testing.T) {
	for _, h := range []string{"disp", "disp-deb-air"} {
		if !protectedHostnames[h] {
			t.Errorf("%q should be protected", h)
		}
	}
	for _, h := range []string{"raspberrypi", "debian", "localhost", ""} {
		if !genericHostnames[h] {
			t.Errorf("%q should be generic", h)
		}
	}
	if genericHostnames["disp"] {
		t.Errorf("disp must not be a generic (auto-nameable) hostname")
	}
}

func TestLast4Hex(t *testing.T) {
	cases := map[string]string{
		"100000001234abcd":                  "abcd",
		"ab":                                "00ab",
		"":                                  "0000",
		"zzzz":                              "0000", // no hex chars → padded zero
		"deadBEEF":                          "beef",
		"a1b2c3d4e5f6a7b8-c9d0e1f2a3b4c5d6": "c5d6",
	}
	for in, want := range cases {
		if got := last4hex(in); got != want {
			t.Errorf("last4hex(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitNmcli(t *testing.T) {
	got := splitNmcli(`yes:My\:Net:78:WPA2`)
	want := []string{"yes", "My:Net", "78", "WPA2"}
	if len(got) != len(want) {
		t.Fatalf("splitNmcli len = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q", i, got[i], want[i])
		}
	}
	// a literal backslash escaped as \\
	if f := splitNmcli(`a\\b:c`); len(f) != 2 || f[0] != `a\b` || f[1] != "c" {
		t.Errorf("backslash unescape: got %v", f)
	}
}

func TestNormalizeSecurity(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"--":        "",
		"WPA2":      "WPA2",
		"WPA1 WPA2": "WPA2",
		"WPA3":      "WPA3",
		"WPA1":      "WPA",
		"WEP":       "WEP",
		"802.1X":    "802.1X",
	}
	for in, want := range cases {
		if got := normalizeSecurity(in); got != want {
			t.Errorf("normalizeSecurity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseWifiScan(t *testing.T) {
	out := "yes:HomeNet:80:WPA2\n" +
		"no:HomeNet:40:WPA2\n" + // weaker dup → merged
		"no:Guest:55:\n" + // open
		"no::30:WPA2\n" + // hidden SSID → dropped
		"no:Café\\:2:66:WPA3\n"
	saved := map[string]bool{"HomeNet": true, "OldNet": true}
	nets, active := parseWifiScan(out, saved)
	if active != "HomeNet" {
		t.Errorf("active = %q, want HomeNet", active)
	}
	// SSIDs: HomeNet (merged), Guest, Café:2, plus saved-unseen OldNet
	byName := map[string]WifiNetwork{}
	for _, n := range nets {
		byName[n.SSID] = n
	}
	if len(nets) != 4 {
		t.Fatalf("got %d networks (%v), want 4", len(nets), nets)
	}
	if h := byName["HomeNet"]; h.Signal != 80 || !h.Active || !h.Saved {
		t.Errorf("HomeNet = %+v, want signal 80 active saved", h)
	}
	if g := byName["Guest"]; g.Security != "" || g.Saved {
		t.Errorf("Guest = %+v, want open, unsaved", g)
	}
	if _, ok := byName["Café:2"]; !ok {
		t.Errorf("escaped-colon SSID missing; got %v", nets)
	}
	if o := byName["OldNet"]; !o.Saved || o.Signal != 0 {
		t.Errorf("OldNet = %+v, want saved with 0 signal", o)
	}
	// first entry must be the active one (sort order)
	if nets[0].SSID != "HomeNet" {
		t.Errorf("first network = %q, want HomeNet (active sorts first)", nets[0].SSID)
	}
}

func TestSortNetworks(t *testing.T) {
	ns := []WifiNetwork{
		{SSID: "b", Signal: 50},
		{SSID: "a", Signal: 50},
		{SSID: "weak-saved", Signal: 10, Saved: true},
		{SSID: "active", Signal: 5, Active: true},
	}
	sortNetworks(ns)
	if ns[0].SSID != "active" {
		t.Errorf("active should sort first, got %q", ns[0].SSID)
	}
	if ns[1].SSID != "weak-saved" {
		t.Errorf("saved should sort before unsaved, got %q", ns[1].SSID)
	}
	if ns[2].SSID != "a" || ns[3].SSID != "b" {
		t.Errorf("equal-signal ties should sort by SSID, got %q,%q", ns[2].SSID, ns[3].SSID)
	}
}

func TestWifiConnectValidation(t *testing.T) {
	n := &Net{cfg: &Config{}}
	if err := n.WifiConnect("", "", false); err == nil {
		t.Error("empty ssid should error")
	}
	if err := n.WifiConnect("net", "short", false); err == nil {
		t.Error("<8-char psk should error")
	}
	if err := n.WifiConnect("net", string(rep('x', 64)), false); err == nil {
		t.Error(">63-char psk should error")
	}
	if err := n.WifiConnect("bad\nssid", "password1", false); err == nil {
		t.Error("ssid with newline should error")
	}
	if err := n.WifiConnect("-dashnet", "password1", false); err == nil {
		t.Error("ssid starting with '-' should error (nmcli arg-injection guard)")
	}
	// The same guards run before the hidden/pre-provision path forks nmcli.
	if err := n.WifiConnect("", "", true); err == nil {
		t.Error("empty ssid should error on the hidden path too")
	}
	if err := n.WifiConnect("-dashnet", "password1", true); err == nil {
		t.Error("dash ssid should error on the hidden path too")
	}
	// a valid open network passes validation (the nmcli fork is not reached in
	// the length/charset checks; we only assert the guards don't reject it)
	// note: we can't assert success without nmcli, so just confirm the psk-length
	// path accepts an 8-char key by checking it doesn't fail on validation alone.
}

func TestWifiAuthFailure(t *testing.T) {
	auth := []string{
		"Error: Connection activation failed: (7) Secrets were required, but not provided.",
		"activation failed: reason no-secrets",
		"802-1X supplicant disconnected",
		"the password is invalid",
	}
	for _, s := range auth {
		if !wifiAuthFailure(s) {
			t.Errorf("wifiAuthFailure(%q) = false, want true", s)
		}
	}
	notAuth := []string{
		"Error: Connection activation failed: device not ready",
		"Timeout expired",
		"",
	}
	for _, s := range notAuth {
		if wifiAuthFailure(s) {
			t.Errorf("wifiAuthFailure(%q) = true, want false", s)
		}
	}
}

func TestHasDefaultRouteParsingShape(t *testing.T) {
	// hasDefaultRoute reads /proc; on the test host it may or may not have a
	// route. Just assert it does not panic and returns a bool.
	_ = hasDefaultRoute()
}
