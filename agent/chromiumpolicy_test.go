package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChromiumPolicyJSON(t *testing.T) {
	// Both defaults on.
	b, ok := chromiumPolicyJSON(&Config{DisableTranslate: true, CookieExtension: "edibdbjcniadpccecjdfdjjppcpchdlm"})
	if !ok {
		t.Fatal("expected a policy document")
	}
	s := string(b)
	if !strings.Contains(s, `"TranslateEnabled": false`) {
		t.Errorf("missing TranslateEnabled:false: %s", s)
	}
	if !strings.Contains(s, "edibdbjcniadpccecjdfdjjppcpchdlm;https://clients2.google.com/service/update2/crx") {
		t.Errorf("missing cookie extension forcelist entry: %s", s)
	}

	// No cookie extension → no forcelist, still disables translate.
	b2, ok2 := chromiumPolicyJSON(&Config{DisableTranslate: true, CookieExtension: ""})
	if !ok2 || strings.Contains(string(b2), "ExtensionInstallForcelist") {
		t.Errorf("empty cookie ext should omit forcelist: %s", b2)
	}

	// Both off → nothing to enforce.
	if _, ok3 := chromiumPolicyJSON(&Config{}); ok3 {
		t.Error("both knobs off should yield no policy")
	}
}

func TestEnsureChromiumPolicyWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "managed") // not pre-created → MkdirAll path
	cfg := &Config{ChromiumPolicyDir: dir, DisableTranslate: true, CookieExtension: "abc"}
	EnsureChromiumPolicy(cfg)
	b, err := os.ReadFile(filepath.Join(dir, "sideshow.json"))
	if err != nil {
		t.Fatalf("policy file not written: %v", err)
	}
	if !strings.Contains(string(b), `"TranslateEnabled": false`) {
		t.Errorf("policy content wrong: %s", b)
	}
}
