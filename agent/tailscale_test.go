package main

import "testing"

func TestTsHostname(t *testing.T) {
	cases := map[string]string{
		"disp":         "disp",
		"disp-deb-air": "disp-deb-air",
		"Lobby TV":     "lobbytv",
		"node_01":      "node01",
		"UPPER":        "upper",
		"a.b.c":        "a-b-c",
		"--edge--":     "edge",
		"":             "sideshow-node",
		"!!!":          "sideshow-node",
	}
	for in, want := range cases {
		if got := tsHostname(in); got != want {
			t.Errorf("tsHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentPort(t *testing.T) {
	cases := map[string]string{
		":80":          "80",
		":8080":        "8080",
		"127.0.0.1:80": "80",
		"0.0.0.0:9000": "9000",
		"":             "80", // no port → default
		"nonsense":     "80",
	}
	for in, want := range cases {
		if got := agentPort(in); got != want {
			t.Errorf("agentPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTsOverlayURL(t *testing.T) {
	if got := tsOverlayURL("disp.tail-abc.ts.net", nil, true); got != "https://disp.tail-abc.ts.net" {
		t.Errorf("serve on → https, got %q", got)
	}
	if got := tsOverlayURL("disp.tail-abc.ts.net", nil, false); got != "http://disp.tail-abc.ts.net" {
		t.Errorf("serve off → http (still WireGuard-encrypted), got %q", got)
	}
	if got := tsOverlayURL("", []string{"100.64.0.1", "fd7a::1"}, false); got != "http://100.64.0.1" {
		t.Errorf("no DNS name → fall back to first tailnet IP, got %q", got)
	}
	if got := tsOverlayURL("", nil, true); got != "" {
		t.Errorf("no name/ip → empty, got %q", got)
	}
}

func TestParseTailscaleStatus(t *testing.T) {
	// Joined: BackendState=Running, Self present, CurrentTailnet present.
	joined := `{
		"BackendState": "Running",
		"Self": {"DNSName": "disp-deb-air.tail-abcd.ts.net.", "HostName": "disp-deb-air", "TailscaleIPs": ["100.101.102.103", "fd7a:1::1"]},
		"CurrentTailnet": {"Name": "example.com", "MagicDNSSuffix": "tail-abcd.ts.net"}
	}`
	info := parseTailscaleStatus(joined, true)
	if !info.Installed || !info.Running || !info.LoggedIn {
		t.Fatalf("joined: installed/running/logged_in = %v/%v/%v", info.Installed, info.Running, info.LoggedIn)
	}
	if info.DNSName != "disp-deb-air.tail-abcd.ts.net" { // trailing dot stripped
		t.Errorf("DNSName = %q, want the trailing dot stripped", info.DNSName)
	}
	if info.Tailnet != "example.com" {
		t.Errorf("Tailnet = %q, want example.com", info.Tailnet)
	}
	if len(info.IPs) != 2 || info.IPs[0] != "100.101.102.103" {
		t.Errorf("IPs = %v", info.IPs)
	}
	if info.URL != "https://disp-deb-air.tail-abcd.ts.net" { // serve=true → https
		t.Errorf("URL = %q, want https (serve on)", info.URL)
	}

	// Logged out: valid JSON, NeedsLogin, no tailnet.
	out := parseTailscaleStatus(`{"BackendState": "NeedsLogin", "Self": {"HostName": "disp"}}`, false)
	if !out.Running || out.LoggedIn {
		t.Errorf("logged out: running=%v logged_in=%v (want running, not logged in)", out.Running, out.LoggedIn)
	}
	if out.Note == "" {
		t.Errorf("logged out should carry a 'not joined' note")
	}

	// Daemon down / garbage output: not parseable → not running, note carries a hint.
	down := parseTailscaleStatus("failed to connect to local tailscaled", false)
	if down.Running {
		t.Errorf("unparseable output should be Running=false")
	}
	if down.Note == "" {
		t.Errorf("unparseable output should carry a note")
	}
	if !down.Installed {
		t.Errorf("parse assumes the CLI is installed")
	}
}
