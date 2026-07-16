package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func newTestXServer(cfg *Config) *XServer {
	return &XServer{cfg: cfg, ctx: context.Background()}
}

// The cursor-hider must launch as an ordinary seat-user X client (DISPLAY +
// XAUTHORITY pointed at the agent's server) so it can hide the pointer the kiosk
// otherwise parks on screen.
func TestBuildCursorHide(t *testing.T) {
	x := newTestXServer(&Config{
		Display:       ":0",
		XAuthority:    "/home/seat/.Xauthority",
		Home:          "/home/seat",
		CursorHideCmd: "unclutter-xfixes --timeout 1 --start-hidden",
	})
	cmd := x.buildCursorHide()

	wantArgs := []string{"unclutter-xfixes", "--timeout", "1", "--start-hidden"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("cursor argv = %v, want %v", cmd.Args, wantArgs)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"DISPLAY=:0", "XAUTHORITY=/home/seat/.Xauthority", "HOME=/home/seat"} {
		if !strings.Contains(env, want) {
			t.Errorf("cursor env missing %q; got %v", want, cmd.Env)
		}
	}
}

// The buildWM refactor onto the shared seat-client builder must preserve the exact
// argv it produced before (binary + Fields-split args).
func TestBuildWMArgsUnchanged(t *testing.T) {
	x := newTestXServer(&Config{
		Display:    ":0",
		XAuthority: "/home/seat/.Xauthority",
		Home:       "/home/seat",
		WMCmd:      "matchbox-window-manager",
		WMArgs:     "-use_titlebar no",
	})
	cmd := x.buildWM()

	wantArgs := []string{"matchbox-window-manager", "-use_titlebar", "no"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("wm argv = %v, want %v", cmd.Args, wantArgs)
	}
}

// TestNoDPMSConf guards the kiosk-never-blanks contract: the agent-owned Xorg
// drop-in must zero every X power timer. A modern Xorg (Debian trixie on the Pi)
// enables DPMS by default (~10 min); dropping any one of these lines would let an
// idle kiosk auto-blank with no schedule or command.
func TestNoDPMSConf(t *testing.T) {
	if !strings.Contains(nodpmsConf, `Section "ServerFlags"`) {
		t.Fatalf("nodpmsConf must be a ServerFlags section:\n%s", nodpmsConf)
	}
	for _, timer := range []string{"BlankTime", "StandbyTime", "SuspendTime", "OffTime"} {
		if !strings.Contains(nodpmsConf, `Option "`+timer+`"`) {
			t.Errorf("nodpmsConf missing %s timer — an idle kiosk could auto-blank", timer)
		}
	}
	// Every timer must be set to 0 (disabled), nothing left nonzero.
	if n := strings.Count(nodpmsConf, `"0"`); n != 4 {
		t.Errorf("want 4 zeroed timers, found %d occurrences of \"0\"", n)
	}
}
