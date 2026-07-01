package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SysStats is a point-in-time snapshot of node-health metrics served by
// GET /api/stats and shown in the webUI's System panel. Cheap fields (load,
// mem, disk, temp) are read fresh per request from /proc + /sys; the costlier
// ones (cpu%, throttle, resolution) are sampled on background tickers so a poll
// never spawns a subprocess or blocks. Apt state comes from the Apt manager.
type SysStats struct {
	Time       string      `json:"time"`
	UptimeS    int64       `json:"uptime_s"` // node (kernel) uptime, not the agent's
	Load       [3]float64  `json:"load"`     // 1 / 5 / 15-min loadavg
	CPUCount   int         `json:"cpu_count"`
	CPUPercent float64     `json:"cpu_percent"` // busy % over the last sample window
	Mem        MemStats    `json:"mem"`
	Disk       DiskStats   `json:"disk"`
	TempC      float64     `json:"temp_c,omitempty"`
	Throttled  string      `json:"throttled,omitempty"`  // vcgencmd get_throttled, e.g. "0x0"
	UnderVolt  bool        `json:"undervolt"`            // throttle bit 0/16 set (now / ever)
	Model      string      `json:"model,omitempty"`      // /proc/device-tree/model
	Resolution string      `json:"resolution,omitempty"` // current X output mode, e.g. 1920x1080
	Display    DisplayInfo `json:"display"`              // rotation + kiosk zoom
	Upgrades   AptStatus   `json:"upgrades"`
}

// MemStats is RAM in MiB (Used = Total - Available, the figure that matters on a
// box with no swap headroom).
type MemStats struct {
	TotalMB int     `json:"total_mb"`
	UsedMB  int     `json:"used_mb"`
	FreeMB  int     `json:"free_mb"`
	Percent float64 `json:"percent"`
}

// DiskStats is the root filesystem in GB (Used counts non-free blocks; Free is
// what an unprivileged write can still use — Bavail, not Bfree).
type DiskStats struct {
	TotalGB float64 `json:"total_gb"`
	UsedGB  float64 `json:"used_gb"`
	FreeGB  float64 `json:"free_gb"`
	Percent float64 `json:"percent"`
}

// Stats collects node metrics. Construct with NewStats and call Start once; it
// runs two background tickers (a fast cpu sampler, a slow exec sampler) and
// answers Snapshot() without blocking on a subprocess.
type Stats struct {
	cfg     *Config
	apt     *Apt
	display *Display

	model    string
	cpuCount int

	mu         sync.Mutex
	lastCPU    cpuSample
	cpuPct     float64
	throttled  string
	everUnder  bool
	resolution string
}

func NewStats(cfg *Config, apt *Apt, display *Display) *Stats {
	s := &Stats{cfg: cfg, apt: apt, display: display}
	s.model = strings.TrimRight(readFileTrim("/proc/device-tree/model"), "\x00")
	s.cpuCount = countCPUs()
	s.lastCPU, _ = readCPUSample()
	return s
}

// Start launches the background samplers. Idempotent enough for one call from
// main; the goroutines run for the process lifetime.
func (s *Stats) Start() {
	// Fast loop: cpu% from /proc/stat deltas (file read only, cheap).
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			cur, err := readCPUSample()
			if err != nil {
				continue
			}
			s.mu.Lock()
			s.cpuPct = cpuBusyPercent(s.lastCPU, cur)
			s.lastCPU = cur
			s.mu.Unlock()
		}
	}()
	// Slow loop: subprocess-backed fields (vcgencmd throttle, xrandr resolution).
	// Hold the first sample until the Chromium cold-start window has cleared so
	// these forks (xrandr drops to the seat uid) don't stack onto it and risk the
	// hardware watchdog. The fast /proc CPU loop above keeps running meanwhile.
	go func() {
		time.Sleep(45 * time.Second)
		s.refreshSlow()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			s.refreshSlow()
		}
	}()
}

func (s *Stats) refreshSlow() {
	thr := readThrottled()
	res := currentResolution(s.cfg)
	s.mu.Lock()
	if thr != "" {
		s.throttled = thr
		if throttleUnderVolt(thr) {
			s.everUnder = true
		}
	}
	if res != "" {
		s.resolution = res
	}
	s.mu.Unlock()
}

// Snapshot reads the cheap fields fresh and merges the cached sampled fields.
func (s *Stats) Snapshot() SysStats {
	s.mu.Lock()
	cpuPct, thr, ever, res := s.cpuPct, s.throttled, s.everUnder, s.resolution
	s.mu.Unlock()

	st := SysStats{
		Time:       time.Now().UTC().Format(time.RFC3339),
		UptimeS:    readUptime(),
		Load:       readLoadavg(),
		CPUCount:   s.cpuCount,
		CPUPercent: round1(cpuPct),
		Mem:        readMem(),
		Disk:       readDisk("/"),
		TempC:      round1(readTempC()),
		Throttled:  thr,
		UnderVolt:  ever || throttleUnderVolt(thr),
		Model:      s.model,
		Resolution: res,
	}
	if s.display != nil {
		st.Display = s.display.Info()
	}
	if s.apt != nil {
		st.Upgrades = s.apt.Status()
	}
	return st
}

// ---- /proc + /sys readers ----

func readUptime() int64 {
	f := readFileTrim("/proc/uptime")
	if i := strings.IndexByte(f, ' '); i > 0 {
		f = f[:i]
	}
	if v, err := strconv.ParseFloat(f, 64); err == nil {
		return int64(v)
	}
	return 0
}

func readLoadavg() [3]float64 {
	var out [3]float64
	fields := strings.Fields(readFileTrim("/proc/loadavg"))
	for i := 0; i < 3 && i < len(fields); i++ {
		out[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return out
}

func readMem() MemStats {
	total, avail := 0, 0
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemStats{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMeminfoKB(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = parseMeminfoKB(line)
		}
	}
	totMB, availMB := total/1024, avail/1024
	used := totMB - availMB
	var pct float64
	if totMB > 0 {
		pct = round1(float64(used) / float64(totMB) * 100)
	}
	return MemStats{TotalMB: totMB, UsedMB: used, FreeMB: availMB, Percent: pct}
}

// parseMeminfoKB pulls the kB value out of a "Key:   12345 kB" line.
func parseMeminfoKB(line string) int {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		if v, err := strconv.Atoi(fields[1]); err == nil {
			return v
		}
	}
	return 0
}

func readTempC() float64 {
	// thermal_zone0 is the CPU on the Pi; millidegrees. File read — no vcgencmd.
	raw := readFileTrim("/sys/class/thermal/thermal_zone0/temp")
	if v, err := strconv.Atoi(raw); err == nil {
		return float64(v) / 1000
	}
	return 0
}

// cpuSample is the cumulative jiffy counters from /proc/stat's aggregate line.
type cpuSample struct{ busy, total uint64 }

func readCPUSample() (cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // user nice system idle iowait irq softirq steal ...
		var total, idle uint64
		for i, fv := range fields {
			v, _ := strconv.ParseUint(fv, 10, 64)
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		return cpuSample{busy: total - idle, total: total}, nil
	}
	return cpuSample{}, os.ErrInvalid
}

func cpuBusyPercent(prev, cur cpuSample) float64 {
	dt := float64(cur.total - prev.total)
	if dt <= 0 {
		return 0
	}
	pct := float64(cur.busy-prev.busy) / dt * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func countCPUs() int {
	n := 0
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// readFileTrim returns a file's contents with surrounding whitespace stripped,
// or "" on any error (so a missing /sys node degrades to an empty metric).
func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
