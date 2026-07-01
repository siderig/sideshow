package main

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// AptStatus is the upgrade picture shown in GET /api/stats and the webUI.
type AptStatus struct {
	Supported  bool     `json:"supported"`          // apt-get present (a Debian node)
	Available  int      `json:"available"`          // packages with a pending upgrade
	Packages   []string `json:"packages,omitempty"` // their names (capped)
	CheckedAt  string   `json:"checked_at,omitempty"`
	Checking   bool     `json:"checking"`
	Upgrading  bool     `json:"upgrading"`
	LastResult string   `json:"last_result,omitempty"` // "ok" | "failed"
	LogTail    string   `json:"log_tail,omitempty"`    // tail of the last upgrade run
}

const aptPkgCap = 60 // cap the package-name list so /api/stats stays small

// Apt tracks pending Debian upgrades and runs an upgrade on request. The check
// (apt-get -s upgrade) is slow on a Pi, so it runs on a background cadence and
// callers read the cached result; only one check or upgrade runs at a time.
type Apt struct {
	supported bool

	mu         sync.Mutex
	available  int
	packages   []string
	checkedAt  time.Time
	checking   bool
	upgrading  bool
	lastResult string
	logTail    string
}

func NewApt() *Apt {
	_, err := exec.LookPath("apt-get")
	return &Apt{supported: err == nil}
}

// Start kicks off the background check cadence: a quick simulate-only pass 60s
// after boot (no apt-get update — avoids network/load during the fragile
// cold-boot window), then a full update+simulate every 3h.
func (a *Apt) Start() {
	if !a.supported {
		return
	}
	go func() {
		time.Sleep(60 * time.Second)
		a.Check(false)
		t := time.NewTicker(3 * time.Hour)
		defer t.Stop()
		for range t.C {
			a.Check(true)
		}
	}()
}

func (a *Apt) Status() AptStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := AptStatus{
		Supported:  a.supported,
		Available:  a.available,
		Packages:   a.packages,
		Checking:   a.checking,
		Upgrading:  a.upgrading,
		LastResult: a.lastResult,
		LogTail:    a.logTail,
	}
	if !a.checkedAt.IsZero() {
		st.CheckedAt = a.checkedAt.UTC().Format(time.RFC3339)
	}
	return st
}

// Check refreshes the pending-upgrade count. With doUpdate it first refreshes
// the package lists (apt-get update — network + a little load). Skipped if a
// check or upgrade is already running. Safe to call from a goroutine.
func (a *Apt) Check(doUpdate bool) {
	if !a.supported {
		return
	}
	a.mu.Lock()
	if a.checking || a.upgrading {
		a.mu.Unlock()
		return
	}
	a.checking = true
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.checking = false
		a.mu.Unlock()
	}()

	if doUpdate {
		// Best-effort; a failed/offline update just means a staler count.
		if _, err := aptCmd(180*time.Second, "update", "-qq"); err != nil {
			log.Printf("[apt] update failed (using cached lists): %v", err)
		}
	}

	out, err := aptCmd(150*time.Second, "-s", "-o", "Debug::NoLocking=true", "upgrade")
	if err != nil {
		log.Printf("[apt] simulate upgrade failed: %v", err)
		return
	}
	count, pkgs := parseAptSimulate(out)

	a.mu.Lock()
	a.available = count
	a.packages = pkgs
	a.checkedAt = time.Now()
	a.mu.Unlock()
}

// BeginUpgrade atomically claims the upgrade slot so the busy check and the
// start are not racy (two POSTs can't both "win"). Returns errAptUnsupported on
// a non-apt node or errAptBusy if a check/upgrade is already running. On success
// the caller MUST run RunClaimedUpgrade (in a goroutine) to do the work and
// release the slot.
func (a *Apt) BeginUpgrade() error {
	if !a.supported {
		return errAptUnsupported
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.upgrading || a.checking {
		return errAptBusy
	}
	a.upgrading = true
	a.logTail = "starting apt-get upgrade…\n"
	a.lastResult = ""
	return nil
}

// RunClaimedUpgrade performs a conservative `apt-get upgrade` (never
// dist-upgrade: it won't remove packages or pull a new kernel meta on its own)
// after BeginUpgrade has claimed the slot. It runs apt under nice + ionice so
// the multi-minute apt+dpkg load can't starve PID1 into the 1-minute hardware
// watchdog on a marginal Pi, and passes a minimal env (not the agent's root env)
// to maintainer scripts. Always releases the slot; refreshes the count after.
func (a *Apt) RunClaimedUpgrade() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nice", "-n", "19", "ionice", "-c3", "apt-get",
		"-y",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"upgrade")
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DEBIAN_FRONTEND=noninteractive",
	}
	out, err := cmd.CombinedOutput()

	a.mu.Lock()
	a.upgrading = false
	a.logTail = tailString(string(out), 8192)
	if err != nil {
		a.lastResult = "failed"
		log.Printf("[apt] upgrade failed: %v", err)
	} else {
		a.lastResult = "ok"
		log.Printf("[apt] upgrade completed")
	}
	a.mu.Unlock()

	a.Check(false) // refresh the pending count
}

// aptCmd runs apt-get under nice + ionice (idle priority) so even the check's
// update/simulate can't dominate the CPU/IO on a marginal node.
func aptCmd(timeout time.Duration, args ...string) (string, error) {
	full := append([]string{"-n", "19", "ionice", "-c3", "apt-get"}, args...)
	return runShort(timeout, "nice", full...)
}

var (
	errAptUnsupported = &apiError{code: 501, err: errString("apt-get not available on this node")}
	errAptBusy        = &apiError{code: 409, err: errString("an apt check or upgrade is already running")}
)

// errString is a tiny error wrapper so the sentinel apiErrors above can be
// package-level vars (errors.New is fine too, but this keeps them inline).
type errString string

func (e errString) Error() string { return string(e) }

// parseAptSimulate counts the "Inst <pkg> …" lines from `apt-get -s upgrade`
// and returns the package names (capped).
func parseAptSimulate(out string) (int, []string) {
	count := 0
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		count++
		if len(pkgs) < aptPkgCap {
			f := strings.Fields(line)
			if len(f) >= 2 {
				pkgs = append(pkgs, f[1])
			}
		}
	}
	return count, pkgs
}

// tailString returns at most the last n bytes of s, prefixed with an ellipsis
// when truncated, clipped to a rune boundary.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < 200 {
		s = s[i+1:] // start at a line boundary if one is close
	}
	return "…\n" + s
}
