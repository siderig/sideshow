package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CEC controls a TV over HDMI-CEC via the kernel CEC API (cec-ctl from
// v4l-utils) on /dev/cecN — no libcec. Validated against a Philips TPM171E on
// disp2's vc4-hdmi adapter. The adapter must claim a logical address once
// (cec-ctl --playback); that config persists in the kernel until reset, so we
// (re)assert it lazily before each command.
type CEC struct {
	device    string
	osdName   string
	supported bool

	mu         sync.Mutex // serializes cec-ctl ops and guards the cached state
	configured bool
	phys       string // our physical address, e.g. "2.0.0.0" (for --active-source)
	tvName     string
	vendor     string
	power      string
	idAt       time.Time // when tvName/vendor were last fetched
	powerAt    time.Time // when power was last fetched
}

// CECInfo is the JSON shape for GET /api/cec.
type CECInfo struct {
	Available  bool   `json:"available"`
	Device     string `json:"device,omitempty"`
	Configured bool   `json:"configured"`
	TVName     string `json:"tv_name,omitempty"`
	Vendor     string `json:"vendor,omitempty"`
	Power      string `json:"power,omitempty"` // on | standby | to-on | to-standby | unknown
	Phys       string `json:"phys,omitempty"`
}

func NewCEC(cfg *Config) *CEC {
	c := &CEC{device: cfg.CECDevice, osdName: cfg.CECName}
	if c.osdName == "" {
		c.osdName = "sideshow"
	}
	_, lookErr := exec.LookPath("cec-ctl")
	_, statErr := os.Stat(c.device)
	c.supported = lookErr == nil && statErr == nil
	return c
}

// Start claims the logical address and fetches the TV identity in the
// background so the first webUI poll already has a name/power to show.
func (c *CEC) Start() {
	if !c.supported {
		return
	}
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.ensureConfiguredLocked()
		c.refreshIdentityLocked()
		c.refreshPowerLocked()
	}()
}

// Info returns the cached TV state, lazily refreshing power (short TTL) and
// identity (long TTL) so the webUI poll doesn't spam the CEC bus.
func (c *CEC) Info() CECInfo {
	if !c.supported {
		return CECInfo{Available: false, Device: c.device}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureConfiguredLocked()
	if time.Since(c.powerAt) > 4*time.Second {
		c.refreshPowerLocked()
	}
	if c.tvName == "" || time.Since(c.idAt) > 60*time.Second {
		c.refreshIdentityLocked()
	}
	return CECInfo{
		Available:  true,
		Device:     c.device,
		Configured: c.configured,
		TVName:     c.tvName,
		Vendor:     c.vendor,
		Power:      orUnknown(c.power),
		Phys:       c.phys,
	}
}

// On wakes the TV (image-view-on) and makes the Pi the active source so the TV
// switches to our HDMI input.
func (c *CEC) On() error {
	return c.do(func() error {
		if _, err := c.ctl(6*time.Second, "--to", "0", "--image-view-on"); err != nil {
			return err
		}
		c.activeSourceLocked()
		c.power, c.powerAt = "to-on", time.Now()
		return nil
	})
}

// Off puts the TV into standby.
func (c *CEC) Off() error {
	return c.do(func() error {
		if _, err := c.ctl(6*time.Second, "--to", "0", "--standby"); err != nil {
			return err
		}
		c.power, c.powerAt = "to-standby", time.Now()
		return nil
	})
}

// ActiveSource broadcasts that the Pi is the active source (switches the TV's
// input to us without changing its power state).
func (c *CEC) ActiveSource() error {
	return c.do(func() error { return c.activeSourceLocked() })
}

// userControl sends a CEC remote-key press+release to the TV (LA 0) — used for
// volume up/down/mute, which a TV with built-in speakers (or an ARC amp) honors.
func (c *CEC) userControl(uiCmd string) error {
	return c.do(func() error {
		if _, err := c.ctl(6*time.Second, "--to", "0", "--user-control-pressed", "ui-cmd="+uiCmd); err != nil {
			return err
		}
		_, err := c.ctl(4*time.Second, "--to", "0", "--user-control-released")
		return err
	})
}

func (c *CEC) VolumeUp() error   { return c.userControl("volume-up") }
func (c *CEC) VolumeDown() error { return c.userControl("volume-down") }
func (c *CEC) Mute() error       { return c.userControl("mute") }

// StartMonitor runs `cec-ctl --monitor` in the background and updates the cached
// TV power state when the TV is turned on/off by its own remote — closing the
// feedback loop (we otherwise only ever *send*). Best-effort; restarts if the
// monitor process exits.
func (c *CEC) StartMonitor() {
	if !c.supported {
		return
	}
	go func() {
		for {
			c.runMonitor()
			time.Sleep(5 * time.Second)
		}
	}()
}

func (c *CEC) runMonitor() {
	cmd := exec.Command("cec-ctl", "-d", c.device, "--monitor")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		log.Printf("[cec] monitor start: %v", err)
		return
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		// Only react to traffic involving the TV (logical address 0).
		if !strings.Contains(line, "from TV (0)") && !strings.Contains(line, "to TV (0)") {
			continue
		}
		switch {
		case strings.Contains(line, "pwr-state:"):
			rest := line[strings.Index(line, "pwr-state:")+len("pwr-state:"):]
			if v := firstField(strings.TrimSpace(rest)); v != "" {
				c.setPower(v)
			}
		case strings.Contains(line, "STANDBY"):
			c.setPower("standby")
		case strings.Contains(line, "IMAGE_VIEW_ON"), strings.Contains(line, "TEXT_VIEW_ON"), strings.Contains(line, "ACTIVE_SOURCE"):
			c.setPower("on")
		}
	}
	cmd.Wait()
}

func (c *CEC) setPower(p string) {
	c.mu.Lock()
	changed := c.power != p
	c.power = p
	c.powerAt = time.Now()
	c.mu.Unlock()
	if changed {
		log.Printf("[cec] TV power → %s (observed on the bus)", p)
	}
}

func (c *CEC) activeSourceLocked() error {
	phys := c.phys
	if !validPhys(phys) {
		phys = "2.0.0.0" // sane default for a single-hop HDMI input
	}
	_, err := c.ctl(6*time.Second, "--active-source", "phys-addr="+phys)
	return err
}

// do runs fn under the lock after ensuring the adapter is configured, mapping a
// "not supported" node to a clean 501.
func (c *CEC) do(fn func() error) error {
	if !c.supported {
		return &apiError{code: 501, err: fmt.Errorf("CEC not available on this node (no cec-ctl or %s)", c.device)}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureConfiguredLocked()
	if err := fn(); err != nil {
		return &apiError{code: 502, err: fmt.Errorf("cec: %w", err)}
	}
	return nil
}

// ensureConfiguredLocked claims a Playback logical address if the adapter has
// none, and records our physical address. Cheap to re-check (a query); only
// claims when the LA mask is zero (fresh boot / after a reset).
func (c *CEC) ensureConfiguredLocked() {
	out, err := c.ctl(6 * time.Second)
	if err == nil && parseHexU64(fieldLine(out, "Logical Address Mask")) != 0 {
		c.configured = true
		if p := fieldLine(out, "Physical Address"); validPhys(p) {
			c.phys = p
		}
		return
	}
	// Not configured (or query failed) — claim a Playback logical address.
	out, err = c.ctl(8*time.Second, "--playback", "--osd-name", c.osdName)
	if err != nil {
		c.configured = false
		return
	}
	c.configured = parseHexU64(fieldLine(out, "Logical Address Mask")) != 0
	if p := fieldLine(out, "Physical Address"); validPhys(p) {
		c.phys = p
	}
}

// validPhys reports whether a CEC physical address is a real one — "f.f.f.f"
// (no EDID / HDMI unplugged) and "0.0.0.0" (the TV's own / unset) must NOT be
// cached or used as our --active-source address.
func validPhys(p string) bool {
	return p != "" && p != "f.f.f.f" && p != "0.0.0.0"
}

func (c *CEC) refreshPowerLocked() {
	out, err := c.ctl(6*time.Second, "--to", "0", "--give-device-power-status")
	if err != nil {
		return
	}
	if v := fieldLine(out, "pwr-state"); v != "" {
		c.power = firstField(v) // "on (0x00)" -> "on"
	}
	c.powerAt = time.Now()
}

func (c *CEC) refreshIdentityLocked() {
	if out, err := c.ctl(6*time.Second, "--to", "0", "--give-osd-name"); err == nil {
		if n := fieldLine(out, "name"); n != "" {
			c.tvName = n
		}
	}
	if out, err := c.ctl(6*time.Second, "--to", "0", "--give-device-vendor-id"); err == nil {
		if v := fieldLine(out, "vendor-id"); v != "" {
			c.vendor = vendorName(v) // "0x00903e (Philips)" -> "Philips"
		}
	}
	c.idAt = time.Now()
}

// ctl runs cec-ctl against our device with the given args.
func (c *CEC) ctl(timeout time.Duration, args ...string) (string, error) {
	full := append([]string{"-d", c.device}, args...)
	return runShort(timeout, "cec-ctl", full...)
}

// ---- cec-ctl output parsing ----

// fieldLine returns the value after a "<prefix> ... : <value>" line (the leading
// label and the colon stripped), matching cec-ctl's indented key/value output.
func fieldLine(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, prefix) {
			rest := strings.TrimSpace(strings.TrimPrefix(t, prefix))
			rest = strings.TrimSpace(strings.TrimPrefix(rest, ":"))
			return rest
		}
	}
	return ""
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}

// vendorName pulls the human name out of "0x00903e (Philips)"; falls back to the
// raw hex when no parenthesized name is present.
func vendorName(s string) string {
	if i := strings.IndexByte(s, '('); i >= 0 {
		if j := strings.IndexByte(s[i:], ')'); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return firstField(s)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
