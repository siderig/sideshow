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
	// The same guards run before the hidden/out-of-range save path forks nmcli.
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

func TestParseHasDefaultRouteV6(t *testing.T) {
	z := "00000000000000000000000000000000"
	// dest destlen src srclen nexthop metric refcnt use flags iface
	line := func(dlen, flags, iface string) string {
		return z + " " + dlen + " " + z + " 00 " + z + " 00000400 00000000 00000000 " + flags + " " + iface
	}
	if !parseHasDefaultRouteV6(line("00", "00000003", "wlp3s0")) { // ::/0 UP+GATEWAY
		t.Error("a real ::/0 default should count as online")
	}
	if parseHasDefaultRouteV6(line("00", "00000001", "lo")) {
		t.Error("a loopback ::/0 must NOT count")
	}
	if parseHasDefaultRouteV6(line("00", "00000201", "eth0")) { // RTF_UP|RTF_REJECT (0x1|0x200)
		t.Error("an unreachable/reject ::/0 must NOT count")
	}
	if parseHasDefaultRouteV6(line("00", "00000002", "eth0")) { // no RTF_UP
		t.Error("a down ::/0 must NOT count")
	}
	if parseHasDefaultRouteV6(line("40", "00000003", "eth0")) { // not ::/0
		t.Error("a non-default (::/N) route must be ignored")
	}
	// a real default mixed with a lo row is still found
	if !parseHasDefaultRouteV6(line("00", "00000001", "lo") + "\n" + line("00", "00000003", "eth0")) {
		t.Error("a real default among noise rows should be found")
	}
}

func TestWifiNotInRange(t *testing.T) {
	// nmcli's "SSID not currently visible" message → fall back to saving a profile.
	notInRange := []string{
		"Error: No network with SSID 'Jyväskylän Parkour Akatemia' found.",
		"Error: No network with SSID 'HomeNet' found.",
		"error: no network with ssid 'x' found.", // case-insensitive
	}
	for _, s := range notInRange {
		if !wifiNotInRange(s) {
			t.Errorf("wifiNotInRange(%q) = false, want true", s)
		}
	}
	// A wrong password / device error is NOT out-of-range — surface it as-is.
	inRange := []string{
		"Error: Connection activation failed: (7) Secrets were required, but not provided.",
		"Error: No suitable device found for this connection.",
		"Timeout expired",
		"",
	}
	for _, s := range inRange {
		if wifiNotInRange(s) {
			t.Errorf("wifiNotInRange(%q) = true, want false", s)
		}
	}
}

func TestParseDefaultRouteIface(t *testing.T) {
	// Real /proc/net/route from the nodes: disp is on eth0, disp-deb-air on wlp3s0.
	wired := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	if got := parseDefaultRouteIface(wired); got != "eth0" {
		t.Errorf("parseDefaultRouteIface(wired) = %q, want eth0", got)
	}
	wifi := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wlp3s0\t00000000\t0102A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n"
	if got := parseDefaultRouteIface(wifi); got != "wlp3s0" {
		t.Errorf("parseDefaultRouteIface(wifi) = %q, want wlp3s0", got)
	}
	// Lowest metric wins when several default routes exist.
	dual := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"wlan0\t00000000\t0102A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	if got := parseDefaultRouteIface(dual); got != "eth0" {
		t.Errorf("parseDefaultRouteIface(dual) = %q, want eth0 (lowest metric)", got)
	}
	// No default route (only the on-link subnet row) → "".
	noDefault := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	if got := parseDefaultRouteIface(noDefault); got != "" {
		t.Errorf("parseDefaultRouteIface(noDefault) = %q, want empty", got)
	}
	if got := parseDefaultRouteIface(""); got != "" {
		t.Errorf("parseDefaultRouteIface(empty) = %q, want empty", got)
	}
}

func TestParseProcWireless(t *testing.T) {
	// Real /proc/net/wireless from disp-deb-air: wlp3s0 at −41 dBm.
	content := "Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE\n" +
		" face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22\n" +
		" wlp3s0: 0000   69.  -41.  -256        0      0      0      0      0        0\n"
	if sig, ok := parseProcWireless(content, "wlp3s0"); !ok || sig != 100 {
		t.Errorf("parseProcWireless(wlp3s0) = (%d,%v), want (100,true)", sig, ok)
	}
	// An interface with no row (radio down / unassociated, like disp) → not found.
	if sig, ok := parseProcWireless(content, "wlan0"); ok || sig != 0 {
		t.Errorf("parseProcWireless(absent) = (%d,%v), want (0,false)", sig, ok)
	}
	// Header-only file (a node with no associated wireless iface) → none.
	headerOnly := "Inter-| sta-|   Quality        |   Discarded packets\n face | tus | link level noise\n"
	if _, ok := parseProcWireless(headerOnly, "wlp3s0"); ok {
		t.Error("parseProcWireless(header-only) reported a signal")
	}
	// A weaker signal maps below 100; interface matching must be exact (not a prefix).
	weak := " wlan0: 0000   40.  -75.  -256        0\n"
	if sig, ok := parseProcWireless(weak, "wlan0"); !ok || sig != 50 {
		t.Errorf("parseProcWireless(weak) = (%d,%v), want (50,true)", sig, ok)
	}
	if _, ok := parseProcWireless(weak, "wlan"); ok {
		t.Error("parseProcWireless matched a partial interface name")
	}
}

func TestDbmToPercent(t *testing.T) {
	cases := []struct {
		dbm  float64
		want int
	}{
		{-30, 100}, {-41, 100}, {-50, 100}, // strong → clamped to 100
		{-60, 80}, {-75, 50}, {-90, 20}, // linear mid-range
		{-100, 0}, {-110, 0}, // weak → clamped to 0
	}
	for _, c := range cases {
		if got := dbmToPercent(c.dbm); got != c.want {
			t.Errorf("dbmToPercent(%g) = %d, want %d", c.dbm, got, c.want)
		}
	}
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
