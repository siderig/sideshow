package main

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	maxRebootsPerWindow = 3
	rebootWindow        = time.Hour
)

// Watchdog keeps an unattended kiosk healthy without anyone on site:
//   - network recovery → reload the page (a kiosk left on an error page after a
//     network blip refreshes itself once connectivity returns);
//   - CDP wedged (web up but the agent can't drive Chromium) → restart the mode;
//   - mode stuck DOWN for minutes → reboot the node (opt-in, -watchdog-reboot).
//
// Checks run every 30s after a boot grace period.
type Watchdog struct {
	cfg *Config
	sup *Supervisor
}

func NewWatchdog(cfg *Config, sup *Supervisor) *Watchdog { return &Watchdog{cfg: cfg, sup: sup} }

func (w *Watchdog) Start() {
	if !w.cfg.Watchdog {
		return
	}
	go w.loop()
}

func (w *Watchdog) loop() {
	time.Sleep(60 * time.Second) // let the cold boot settle
	online := w.netUp()
	var wedge, downStreak, wedgeRestarts int
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		up := w.netUp()
		if up && !online {
			log.Printf("[watchdog] network recovered → reloading kiosk")
			if err := w.sup.ReloadWeb(); err != nil {
				log.Printf("[watchdog] reload failed: %v", err)
			}
		}
		online = up

		st := w.sup.Status()

		// CDP wedged: web running but the agent can't reach Chromium. Escalate
		// cheap→expensive: first re-attach to the still-running Chromium (its debug
		// port often opens just after the cold-start attach window on the Pi — a
		// re-attach needs no relaunch and no screen flash); only hard-restart the
		// mode if a re-attach doesn't take. The framebuffer web backend (web +
		// display=kms, cog/WPE) has no CDP, so a missing attach is expected there —
		// exclude it so the watchdog never restarts/reboots a healthy cog kiosk.
		if up && st.Type == ModeWeb && st.Display != DisplayKMS && st.State == stateRunning && !w.sup.CDPAttached() {
			wedge++
			switch {
			case wedge >= 4: // ~2min: re-attach didn't help → relaunch Chromium.
				log.Printf("[watchdog] kiosk CDP wedged ~2min → restarting mode")
				if err := w.sup.RestartMode(); err != nil {
					log.Printf("[watchdog] restart-mode failed: %v", err)
				}
				wedge = 0
				wedgeRestarts++
				// A relaunch that still doesn't restore CDP, repeated, means the
				// kiosk process is alive but undriveable — health stays "ok", so the
				// mode-down reboot path below never fires. Escalate to a reboot here
				// (same opt-in flag + persisted ceiling) so an unattended node isn't
				// stuck flashing a frozen kiosk forever.
				if w.cfg.WatchdogReboot && wedgeRestarts >= 3 { // ~3 restarts, ~6min
					if w.allowReboot() {
						log.Printf("[watchdog] CDP still wedged after %d restarts → rebooting node (-watchdog-reboot)", wedgeRestarts)
						if out, err := runShort(30*time.Second, "systemctl", "reboot"); err != nil {
							log.Printf("[watchdog] reboot failed: %v: %s", err, out)
						}
					} else {
						log.Printf("[watchdog] CDP wedged but reboot ceiling reached (%d in %s); staying down for an operator", maxRebootsPerWindow, rebootWindow)
					}
					wedgeRestarts = 0
				}
			case wedge >= 2: // ~1min: try a cheap re-attach to the live kiosk first.
				if err := w.sup.ReattachWeb(); err != nil {
					log.Printf("[watchdog] CDP re-attach failed: %v", err)
				} else if w.sup.CDPAttached() {
					log.Printf("[watchdog] re-attached CDP to the running kiosk (no relaunch)")
					wedge = 0
				}
			}
		} else {
			wedge = 0
			wedgeRestarts = 0 // CDP healthy again (or not a web kiosk) → forget the restart streak
		}

		// Mode stuck down (crash-loop give-up): reboot to recover, if allowed.
		if health(st) == "down" {
			downStreak++
		} else {
			downStreak = 0
			w.clearLedger() // healthy again → forget past reboots
		}
		if w.cfg.WatchdogReboot && downStreak >= 10 { // ~5 min down
			if w.allowReboot() {
				log.Printf("[watchdog] mode down ~5min → rebooting node (-watchdog-reboot)")
				if out, err := runShort(30*time.Second, "systemctl", "reboot"); err != nil {
					log.Printf("[watchdog] reboot failed: %v: %s", err, out)
				}
			} else {
				log.Printf("[watchdog] mode down but reboot ceiling reached (%d in %s); staying down for an operator (health/webUI still reachable)", maxRebootsPerWindow, rebootWindow)
			}
			downStreak = 0
		}
	}
}

func (w *Watchdog) ledgerPath() string {
	if w.cfg.StateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(w.cfg.StateFile), "watchdog-reboots.json")
}

// allowReboot enforces a persistent ceiling so a node whose mode can never come
// up doesn't reboot-loop forever (flash wear + an unusable screen). It records
// reboot timestamps across reboots; once maxRebootsPerWindow have happened
// within rebootWindow it refuses and parks in 'failed' for a human.
func (w *Watchdog) allowReboot() bool {
	p := w.ledgerPath()
	var ts []int64
	if p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &ts)
		}
	}
	cutoff := time.Now().Add(-rebootWindow).Unix()
	kept := ts[:0]
	for _, t := range ts {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxRebootsPerWindow {
		w.saveLedger(kept)
		return false
	}
	kept = append(kept, time.Now().Unix())
	w.saveLedger(kept)
	return true
}

func (w *Watchdog) saveLedger(ts []int64) {
	p := w.ledgerPath()
	if p == "" {
		return
	}
	b, _ := json.Marshal(ts)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, b, 0o644)
}

func (w *Watchdog) clearLedger() {
	if p := w.ledgerPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			_ = os.Remove(p)
		}
	}
}

func (w *Watchdog) netUp() bool {
	c, err := net.DialTimeout("tcp", w.cfg.WatchdogProbe, 4*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
