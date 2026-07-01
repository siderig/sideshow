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
