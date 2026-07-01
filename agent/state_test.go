package main

import (
	"path/filepath"
	"testing"
)

func newTestState(t *testing.T) (*State, *Config) {
	t.Helper()
	cfg := &Config{StateFile: filepath.Join(t.TempDir(), "display.json")}
	return NewState(cfg), cfg
}

func stWeb(url string) Mode {
	return Mode{Type: ModeWeb, Params: map[string]any{"url": url}}
}

func TestStateRestoreEmpty(t *testing.T) {
	st, _ := newTestState(t)
	if _, ok := st.Restore(); ok {
		t.Fatal("fresh state should not restore a mode")
	}
}

func TestStateRecordAndRestore(t *testing.T) {
	st, _ := newTestState(t)
	st.RecordMode(stWeb("https://a.example"))
	m, ok := st.Restore()
	if !ok {
		t.Fatal("expected a restored mode")
	}
	if m.Type != ModeWeb || m.str("url") != "https://a.example" {
		t.Fatalf("restored wrong mode: %+v", m)
	}
	if got := st.Info().History; len(got) != 1 {
		t.Fatalf("history len = %d, want 1", len(got))
	}
}

func TestStateHistoryDedupMovesToFront(t *testing.T) {
	st, _ := newTestState(t)
	st.RecordMode(stWeb("https://a.example"))
	st.RecordMode(stWeb("https://b.example"))
	st.RecordMode(stWeb("https://a.example")) // re-show a → back to front, no dup
	h := st.Info().History
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2 (deduped)", len(h))
	}
	if h[0].Mode.str("url") != "https://a.example" {
		t.Fatalf("most-recent = %q, want a.example", h[0].Mode.str("url"))
	}
}

func TestStateHistoryCap(t *testing.T) {
	st, _ := newTestState(t)
	for i := 0; i < maxHistory+10; i++ {
		st.RecordMode(stWeb("https://x.example/" + string(rune('a'+i%26)) + string(rune('a'+i/26))))
	}
	if got := len(st.Info().History); got != maxHistory {
		t.Fatalf("history len = %d, want cap %d", got, maxHistory)
	}
}

func TestStateOffNotInHistoryButActive(t *testing.T) {
	st, _ := newTestState(t)
	st.RecordMode(stWeb("https://a.example"))
	st.RecordMode(Mode{Type: ModeOff})
	if got := len(st.Info().History); got != 1 {
		t.Fatalf("off should not be added to history; len = %d, want 1", got)
	}
	m, ok := st.Restore()
	if !ok || m.Type != ModeOff {
		t.Fatalf("active should be off after recording off: %+v ok=%v", m, ok)
	}
}

func TestStatePersistsAcrossReload(t *testing.T) {
	st, cfg := newTestState(t)
	st.RecordMode(stWeb("https://keep.example"))
	st.SetSetupComplete(true)

	reloaded := NewState(cfg)
	m, ok := reloaded.Restore()
	if !ok || m.str("url") != "https://keep.example" {
		t.Fatalf("active mode did not persist: %+v ok=%v", m, ok)
	}
	if !reloaded.SetupComplete() {
		t.Fatal("setup-complete flag did not persist")
	}
	if len(reloaded.Info().History) != 1 {
		t.Fatalf("history did not persist: %d", len(reloaded.Info().History))
	}
}

func TestCustomModeCRUDAndPersist(t *testing.T) {
	st, cfg := newTestState(t)
	cm, err := st.UpsertCustom(CustomMode{Name: "Viewer", Command: "/bin/v", Display: "compositor"})
	if err != nil || cm.ID == "" {
		t.Fatalf("upsert: err=%v id=%q", err, cm.ID)
	}
	cm.Name = "Viewer 2"
	if _, err := st.UpsertCustom(cm); err != nil { // update by id
		t.Fatalf("update: %v", err)
	}
	got := st.Info().CustomModes
	if len(got) != 1 || got[0].Name != "Viewer 2" {
		t.Fatalf("update did not replace: %+v", got)
	}
	if _, err := st.UpsertCustom(CustomMode{Name: "", Command: "x"}); err == nil {
		t.Fatal("a nameless custom mode should be rejected")
	}
	if len(NewState(cfg).Info().CustomModes) != 1 { // persisted?
		t.Fatal("custom mode did not persist across reload")
	}
	if _, ok := st.GetCustom(cm.ID); !ok {
		t.Fatal("GetCustom should find it")
	}
	if !st.DeleteCustom(cm.ID) || len(st.Info().CustomModes) != 0 {
		t.Fatal("delete failed")
	}
}

func TestCustomRootGate(t *testing.T) {
	appMode := func(display string) Mode {
		return Mode{Type: ModeApp, Display: display, Params: map[string]any{"argv": []any{"/bin/x"}}}
	}
	// Without -allow-custom-root, BOTH root surfaces are refused...
	if _, _, err := modeCommand(&Config{}, appMode(DisplayKMS)); err == nil {
		t.Error("app+kms must be gated without -allow-custom-root")
	}
	if _, _, err := modeCommand(&Config{WaylandRoot: true}, appMode(DisplayWayland)); err == nil {
		t.Error("app+wayland under -wayland-root must be gated without -allow-custom-root (the HIGH bypass)")
	}
	// ...but the non-root surfaces (seat user) are NOT gated.
	if _, _, err := modeCommand(&Config{}, appMode(DisplayWayland)); err != nil {
		t.Errorf("app+wayland on the default seatd path should not be gated: %v", err)
	}
	if _, _, err := modeCommand(&Config{}, appMode(DisplayCompositor)); err != nil {
		t.Errorf("app+compositor should not be gated: %v", err)
	}
	// With the flag, the root surface is allowed through the gate.
	if _, _, err := modeCommand(&Config{AllowCustomRoot: true}, appMode(DisplayKMS)); err != nil {
		t.Errorf("app+kms with -allow-custom-root should pass the gate: %v", err)
	}
}

func TestCustomModeRejectsEnvOnRootSurfaces(t *testing.T) {
	st, _ := newTestState(t)
	if _, err := st.UpsertCustom(CustomMode{Name: "x", Command: "/bin/x", Display: "kms", Env: map[string]string{"K": "V"}}); err == nil {
		t.Error("env on a kms custom mode should be rejected (it is silently ignored otherwise)")
	}
	if _, err := st.UpsertCustom(CustomMode{Name: "x", Command: "/bin/x", Display: "compositor", Env: map[string]string{"K": "V"}}); err != nil {
		t.Errorf("env on a compositor custom mode should be accepted: %v", err)
	}
}

func TestCustomModeBuildsAppMode(t *testing.T) {
	cm := CustomMode{Command: "/bin/x", Args: []string{"-a", "b"}, Display: "console", Env: map[string]string{"K": "V"}}
	m := cm.mode()
	if m.Type != ModeApp || m.Display != "console" {
		t.Fatalf("wrong mode: %+v", m)
	}
	if av := m.argv(); len(av) != 3 || av[0] != "/bin/x" || av[2] != "b" {
		t.Fatalf("argv: %v", av)
	}
	env, ok := m.Params["env"].(map[string]any)
	if !ok || env["K"] != "V" {
		t.Fatalf("env not carried: %v", m.Params["env"])
	}
}

func TestStateDeleteAndClearHistory(t *testing.T) {
	st, _ := newTestState(t)
	st.RecordMode(stWeb("https://a.example"))
	st.RecordMode(stWeb("https://b.example"))
	id := st.Info().History[0].ID // b.example (most recent)
	if !st.DeleteHistory(id) {
		t.Fatal("delete should report removal")
	}
	if st.DeleteHistory(id) {
		t.Fatal("second delete of same id should report nothing removed")
	}
	if len(st.Info().History) != 1 {
		t.Fatalf("history len = %d, want 1 after delete", len(st.Info().History))
	}
	st.ClearHistory()
	if len(st.Info().History) != 0 {
		t.Fatal("history should be empty after clear")
	}
	// clearing history must not disturb the active mode
	if _, ok := st.Restore(); !ok {
		t.Fatal("active mode should survive a history clear")
	}
}

