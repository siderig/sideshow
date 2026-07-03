package main

import (
	"path/filepath"
	"testing"
)

func TestLocalInputPolicyPersists(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{StateFile: filepath.Join(dir, "display.json")}
	s := NewState(cfg)
	if s.LocalInputSet() {
		t.Fatal("a fresh node should have no saved local-input policy")
	}
	if !s.LocalInputAllowed(true) {
		t.Error("an unset policy should honor the default (true)")
	}
	if s.LocalInputAllowed(false) {
		t.Error("an unset policy should honor the default (false)")
	}
	s.SetLocalInput(false)
	if !s.LocalInputSet() {
		t.Error("policy should read as set after SetLocalInput")
	}
	if s.LocalInputAllowed(true) {
		t.Error("a saved policy (false) should override the default")
	}
	s2 := NewState(cfg)
	if !s2.LocalInputSet() || s2.LocalInputAllowed(true) {
		t.Error("local-input policy did not persist across reload")
	}
}
