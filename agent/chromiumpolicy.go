package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// chromiumUpdateURL is the Chrome Web Store update service used by the
// force-install policy to fetch an extension by ID.
const chromiumUpdateURL = "https://clients2.google.com/service/update2/crx"

// chromiumPolicyJSON builds the kiosk Chromium managed-policy document from cfg,
// returning (json, true) when there is anything to enforce. Two defaults shape
// the kiosk's behaviour:
//   - TranslateEnabled:false — the kiosk must never pop the "translate this page?"
//     bar (the --disable-features=Translate flag doesn't suppress the newer
//     partial-translation bubble; the policy does).
//   - ExtensionInstallforcelist — force-install a cookie-consent extension so
//     cookie banners are auto-dismissed (signage shouldn't show consent dialogs).
func chromiumPolicyJSON(cfg *Config) ([]byte, bool) {
	pol := map[string]any{}
	if cfg.DisableTranslate {
		pol["TranslateEnabled"] = false
	}
	if cfg.CookieExtension != "" {
		pol["ExtensionInstallForcelist"] = []string{cfg.CookieExtension + ";" + chromiumUpdateURL}
	}
	if len(pol) == 0 {
		return nil, false
	}
	b, _ := json.MarshalIndent(pol, "", "  ")
	return append(b, '\n'), true
}

// EnsureChromiumPolicy writes the managed-policy file that shapes the kiosk
// Chromium's defaults. Chromium reads <policy-dir>/*.json (default
// /etc/chromium/policies/managed) at every launch; the X and Wayland kiosks share
// the binary, so one file covers both. Idempotent (rewrites only on change) and
// best-effort: a non-root agent or a write error just logs and continues — the
// kiosk still runs, just without the policy. Call before the initial mode switch
// so the first Chromium launch already sees it.
func EnsureChromiumPolicy(cfg *Config) {
	if cfg.ChromiumPolicyDir == "" {
		return
	}
	want, ok := chromiumPolicyJSON(cfg)
	if !ok {
		return // nothing to enforce (both knobs off)
	}
	path := filepath.Join(cfg.ChromiumPolicyDir, "sideshow.json")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(want) {
		return // already current
	}
	if err := os.MkdirAll(cfg.ChromiumPolicyDir, 0o755); err != nil {
		log.Printf("[chromium-policy] mkdir %s: %v (translate/cookie defaults not applied)", cfg.ChromiumPolicyDir, err)
		return
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		log.Printf("[chromium-policy] write %s: %v", path, err)
		return
	}
	log.Printf("[chromium-policy] wrote %s (translate-disabled=%v cookie-ext=%q)", path, cfg.DisableTranslate, cfg.CookieExtension)
}
