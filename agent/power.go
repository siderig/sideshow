package main

import (
	"fmt"
	"log"
	"time"
)

// Power handles node power actions. Standby is a true low-power idle — it stops
// whatever owns the screen AND powers the display off (distinct from `off`,
// which leaves the screen lit, and from sleep, which leaves the kiosk running).
// Reboot/shutdown fire after a short delay so the HTTP response returns first.
type Power struct {
	cfg     *Config
	sup     *Supervisor
	display *Display
}

func NewPower(cfg *Config, sup *Supervisor, display *Display) *Power {
	return &Power{cfg: cfg, sup: sup, display: display}
}

// Standby powers the display off and stops the on-screen mode. The display is
// powered off FIRST so that if that fails (e.g. no xrandr) the kiosk is left
// visible rather than torn down onto a still-lit blank screen.
func (p *Power) Standby() error {
	if err := p.display.Screen("", false); err != nil {
		return err
	}
	return p.sup.Switch(Mode{Type: ModeOff})
}

// Reboot reboots the node. It returns immediately; the reboot fires shortly
// after so the caller gets its 200 first. The agent (Restart=always,
// WantedBy=graphical.target) comes back into the kiosk on boot.
func (p *Power) Reboot() {
	go func() {
		time.Sleep(800 * time.Millisecond)
		log.Printf("[power] rebooting node")
		if out, err := runShort(30*time.Second, "systemctl", "reboot"); err != nil {
			log.Printf("[power] reboot failed: %v: %s", err, out)
		}
	}()
}

// Shutdown powers the node off (gated by -allow-shutdown). ⚠️ A Pi has no
// Wake-on-LAN / RTC-wake by default, so a powered-off node needs someone on site
// to power it back on — hence the gate.
func (p *Power) Shutdown() error {
	if !p.cfg.AllowShutdown {
		return &apiError{code: 403, err: fmt.Errorf("shutdown is disabled on this node (set -allow-shutdown to enable)")}
	}
	go func() {
		time.Sleep(800 * time.Millisecond)
		log.Printf("[power] powering node off")
		if out, err := runShort(30*time.Second, "systemctl", "poweroff"); err != nil {
			log.Printf("[power] poweroff failed: %v: %s", err, out)
		}
	}()
	return nil
}
