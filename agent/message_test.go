package main

import (
	"strings"
	"testing"
)

func TestSafeColor(t *testing.T) {
	cases := []struct{ in, def, want string }{
		{"#fff", "X", "#fff"},
		{"#11223344", "X", "#11223344"},
		{"red", "X", "red"},
		{"rgb(1,2,3)", "X", "rgb(1,2,3)"},
		{"rgba(1,2,3,.5)", "X", "rgba(1,2,3,.5)"},
		{"", "DEF", "DEF"},
		{"red;}body{display:none", "DEF", "DEF"}, // injection attempt → default
		{"</style>", "DEF", "DEF"},
		{"url(x)", "DEF", "DEF"},
		{"#zz", "DEF", "DEF"},
	}
	for _, c := range cases {
		if got := safeColor(c.in, c.def); got != c.want {
			t.Errorf("safeColor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMessageCSS(t *testing.T) {
	// Defaults.
	def := messageCSS(MsgStyle{})
	for _, want := range []string{"position:fixed", "font:600 18px", "color:#e6e9ef", "background:#151922", "top:0"} {
		if !strings.Contains(def, want) {
			t.Errorf("default css missing %q: %s", want, def)
		}
	}
	// Position + size + colors.
	got := messageCSS(MsgStyle{Size: 40, Position: "center", Color: "#fff", Bg: "#000"})
	for _, want := range []string{"font:600 40px", "transform:translate(-50%,-50%)", "color:#fff", "background:#000"} {
		if !strings.Contains(got, want) {
			t.Errorf("css missing %q: %s", want, got)
		}
	}
	// Size clamping.
	if !strings.Contains(messageCSS(MsgStyle{Size: 9}), "font:600 10px") {
		t.Error("size below 10 should clamp to 10")
	}
	if !strings.Contains(messageCSS(MsgStyle{Size: 9999}), "font:600 200px") {
		t.Error("size above 200 should clamp to 200")
	}
	// A malicious color falls back, so the style can't be broken out of.
	bad := messageCSS(MsgStyle{Color: "red;}body{display:none"})
	if strings.Contains(bad, "display:none") {
		t.Errorf("injection leaked into css: %s", bad)
	}
}

func TestIsPNG(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0}
	if !isPNG(png) {
		t.Error("valid PNG signature not recognized")
	}
	if isPNG([]byte("GIF89a")) {
		t.Error("GIF wrongly recognized as PNG")
	}
	if isPNG([]byte{0x89}) {
		t.Error("short buffer wrongly recognized as PNG")
	}
}

func TestParseWlrOutputs(t *testing.T) {
	sample := `HDMI-A-1 "Acme 27"
  Make: Acme
  Enabled: yes
  Modes:
    1920x1080 px
DP-1 "Dell"
  Enabled: yes
`
	got := parseWlrOutputs(sample)
	if len(got) != 2 || got[0] != "HDMI-A-1" || got[1] != "DP-1" {
		t.Fatalf("parseWlrOutputs = %v, want [HDMI-A-1 DP-1]", got)
	}
}
