package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Memory is the runtime/webUI half of the install-time `disp-deploy.sh memcap`:
// it reads total RAM + the agent service's cgroup-v2 memory limits and can apply
// or clear a kiosk memory cap live. A cap is persisted to the SAME systemd
// drop-in the deploy script uses (one source of truth, survives reboot) and
// applied immediately with `systemctl set-property --runtime` so the kiosk isn't
// restarted. Capping is opt-in: a capable machine has no cap, so a heavy page or
// WebGL scene runs unconstrained. Only meaningful when the memory cgroup
// controller is enabled (kernel cmdline cgroup_enable=memory); Status reports
// supported=false otherwise.
type Memory struct {
	cfg *Config
}

func NewMemory(cfg *Config) *Memory { return &Memory{cfg: cfg} }

// memDropin is the persisted cap file — the same one disp-deploy.sh memcap writes.
const memDropin = "/etc/systemd/system/sideshow-agent.service.d/10-memory-lowmem.conf"

// memFloorMB is the smallest MemoryMax the API will set, so a webUI fat-finger
// can't pick a cap that instantly OOM-kills the kiosk.
const memFloorMB = 128

// MemoryStatus is the JSON for GET /api/memory (drives the webUI control).
type MemoryStatus struct {
	Supported  bool   `json:"supported"` // memory cgroup controller present
	RAMTotalMB int    `json:"ram_total_mb"`
	CurrentMB  int    `json:"current_mb"`            // memory.current of the kiosk cgroup
	Capped     bool   `json:"capped"`                // a finite MemoryMax is set
	MaxMB      int    `json:"max_mb,omitempty"`      // 0 = unlimited
	HighMB     int    `json:"high_mb,omitempty"`     // 0 = unlimited
	SwapMaxMB  int    `json:"swap_max_mb,omitempty"` // 0 = unlimited / no swap controller
	Note       string `json:"note,omitempty"`
}

// cgDir returns the agent service's cgroup-v2 directory under /sys/fs/cgroup,
// read from /proc/self/cgroup ("0::/system.slice/sideshow-agent.service").
func (m *Memory) cgDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "0::") { // cgroup v2 unified hierarchy
			return "/sys/fs/cgroup" + strings.TrimPrefix(line, "0::")
		}
	}
	return ""
}

// readCgMB reads a cgroup memory.* file and returns its value in MB; "max" (or
// an empty/garbage value) → 0 meaning unlimited. ok is false only when the file
// is missing (controller absent).
func readCgMB(path string) (mb int, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "max" || s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, true
	}
	return int(n / (1024 * 1024)), true
}

// ramTotalMB reads MemTotal from /proc/meminfo (kB → MB).
func ramTotalMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			if f := strings.Fields(line); len(f) >= 2 {
				kb, _ := strconv.Atoi(f[1])
				return kb / 1024
			}
		}
	}
	return 0
}

// Status reports total RAM, current usage, and the live cgroup limits.
func (m *Memory) Status() MemoryStatus {
	st := MemoryStatus{RAMTotalMB: ramTotalMB()}
	dir := m.cgDir()
	if dir == "" {
		st.Note = "cgroup path unknown (not running under cgroup v2?)"
		return st
	}
	maxMB, ok := readCgMB(dir + "/memory.max")
	if !ok {
		st.Note = "memory cgroup controller not enabled — add cgroup_enable=memory cgroup_memory=1 to the kernel cmdline and reboot"
		return st
	}
	st.Supported = true
	st.MaxMB = maxMB
	st.Capped = maxMB > 0
	if hi, ok := readCgMB(dir + "/memory.high"); ok {
		st.HighMB = hi
	}
	if sw, ok := readCgMB(dir + "/memory.swap.max"); ok {
		st.SwapMaxMB = sw
	}
	if cur, ok := readCgMB(dir + "/memory.current"); ok {
		st.CurrentMB = cur
	}
	if !st.Capped {
		st.Note = "unconstrained (no cap)"
	}
	return st
}

// mbOrInfinity formats an MB value as a systemd size, or "infinity" for <=0.
func mbOrInfinity(mb int) string {
	if mb <= 0 {
		return "infinity"
	}
	return strconv.Itoa(mb) + "M"
}

// Apply persists and live-applies a cap. maxMB is required (>= memFloorMB);
// highMB/swapMB of 0 mean "unset" (→ infinity). Values are clamped to RAM and
// ordered (high <= max) so the webUI can't wedge the kiosk.
func (m *Memory) Apply(highMB, maxMB, swapMB int) error {
	if maxMB < memFloorMB {
		return &apiError{code: 400, err: fmt.Errorf("max_mb must be >= %d", memFloorMB)}
	}
	if highMB < 0 || swapMB < 0 {
		return &apiError{code: 400, err: fmt.Errorf("high_mb and swap_max_mb must be >= 0 (0 = unset)")}
	}
	if ram := ramTotalMB(); ram > 0 && maxMB > ram {
		maxMB = ram
	}
	if highMB > maxMB { // 0 stays 0 (unset)
		highMB = maxMB
	}
	high, max, swap := mbOrInfinity(highMB), mbOrInfinity(maxMB), mbOrInfinity(swapMB)
	if err := writeMemDropin(high, max, swap); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("persist cap: %w", err)}
	}
	return applyMemLive(high, max, swap)
}

// Clear removes the cap entirely (unconstrained).
func (m *Memory) Clear() error {
	if err := os.Remove(memDropin); err != nil && !os.IsNotExist(err) {
		return &apiError{code: 500, err: fmt.Errorf("remove cap drop-in: %w", err)}
	}
	return applyMemLive("infinity", "infinity", "infinity")
}

// writeMemDropin persists the cap as the shared systemd drop-in.
func writeMemDropin(high, max, swap string) error {
	if err := os.MkdirAll(filepath.Dir(memDropin), 0o755); err != nil {
		return err
	}
	content := "# Memory cap for a low-end node — written by the agent (POST /api/memory);\n" +
		"# the same drop-in `disp-deploy.sh memcap` uses. Deleted when the cap is cleared.\n" +
		"[Service]\nMemoryHigh=" + high + "\nMemoryMax=" + max + "\nMemorySwapMax=" + swap + "\n"
	return os.WriteFile(memDropin, []byte(content), 0o644)
}

// applyMemLive makes systemd aware of the drop-in (daemon-reload) and applies the
// values to the live cgroup without restarting the service (set-property --runtime;
// the drop-in carries the value across a reboot).
func applyMemLive(high, max, swap string) error {
	if out, err := runShort(15*time.Second, "systemctl", "daemon-reload"); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("daemon-reload: %v: %s", err, out)}
	}
	if out, err := runShort(15*time.Second, "systemctl", "set-property", "--runtime",
		"sideshow-agent.service", "MemoryHigh="+high, "MemoryMax="+max, "MemorySwapMax="+swap); err != nil {
		return &apiError{code: 500, err: fmt.Errorf("set-property: %v: %s", err, out)}
	}
	return nil
}

// handleMemory backs GET/POST /api/memory. GET returns the status; POST applies
// a cap ({"high_mb","max_mb","swap_max_mb"}) or clears it ({"off":true}). It's
// gated by the same key as every other control surface.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.mem.Status())
	case http.MethodPost:
		var body struct {
			Off    bool `json:"off"`
			HighMB int  `json:"high_mb"`
			MaxMB  int  `json:"max_mb"`
			SwapMB int  `json:"swap_max_mb"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
			return
		}
		if st := s.mem.Status(); !st.Supported {
			writeErr(w, &apiError{code: 501, err: fmt.Errorf("%s", st.Note)})
			return
		}
		var err error
		if body.Off {
			err = s.mem.Clear()
		} else {
			err = s.mem.Apply(body.HighMB, body.MaxMB, body.SwapMB)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, s.mem.Status())
	default:
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use GET or POST")})
	}
}
