package main

import "testing"

func TestValidAuthKey(t *testing.T) {
	ok := []string{"password", "12345678", "a-Str0ng_Key!", string(rep('x', 128))}
	for _, k := range ok {
		if err := validAuthKey(k); err != nil {
			t.Errorf("validAuthKey(%q) = %v, want nil", k, err)
		}
	}
	bad := map[string]string{
		"":                     "empty",
		"short":                "<8 chars",
		string(rep('x', 129)):  ">128 chars",
		"has space":            "contains a space",
		"tab\tkey":             "contains a tab",
		"newline\n":            "contains a newline",
		"café-passphrase":      "non-ASCII (é)",
	}
	for k, why := range bad {
		if err := validAuthKey(k); err == nil {
			t.Errorf("validAuthKey(%q) = nil, want error (%s)", k, why)
		}
	}
}

// TestAuthKeyRotation covers the live-key path the setup wizard / Settings use:
// the value authed() checks must follow SetAuthKey immediately, and an unseeded
// Config must fall back to the AuthKey field (so tests + pre-resolve code work).
func TestAuthKeyRotation(t *testing.T) {
	cfg := &Config{}
	if cfg.AuthKeyValue() != "" {
		t.Errorf("a zero Config should report an empty key, got %q", cfg.AuthKeyValue())
	}
	cfg.AuthKey = "boot-seed-key" // the boot field, before resolve() seeds the atomic
	if cfg.AuthKeyValue() != "boot-seed-key" {
		t.Errorf("unseeded AuthKeyValue should fall back to the field, got %q", cfg.AuthKeyValue())
	}
	cfg.SetAuthKey("rotated-key-123")
	if got := cfg.AuthKeyValue(); got != "rotated-key-123" {
		t.Errorf("after SetAuthKey, AuthKeyValue = %q, want rotated-key-123", got)
	}
	// The boot field is now stale but must never be consulted again.
	if cfg.AuthKey == cfg.AuthKeyValue() {
		t.Error("expected the live key to diverge from the stale boot field after rotation")
	}
	// A server built on this config authenticates against the ROTATED key.
	s := &Server{cfg: cfg}
	if !s.authEnabled() {
		t.Error("authEnabled should be true with a key set")
	}
	if !ctEq("rotated-key-123", s.cfg.AuthKeyValue()) {
		t.Error("authed comparisons must use the rotated key")
	}
}
