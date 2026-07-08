package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Net is the node-identity + connectivity manager: it renames the node
// (hostnamectl) and manages Wi-Fi (nmcli — the NetworkManager CLI, which comitup
// also assumes), plus two boot-time actions: a first-run auto-name and the
// comitup recovery-AP fallback. It is the source of truth for the *live*
// hostname, so a rename is reflected in the control surface immediately without
// an agent restart (cfg.Node is set once at boot and never mutated, so there is
// no race on it).
//
// The persisted operator-chosen name (net.json) lets the first-boot auto-namer
// know the operator already picked a name and must not re-derive one.
//
// Snapshot cost: Info() is fork-free (cached hostname + capability flags) because
// the webUI polls the snapshot frequently on weak nodes; the expensive nmcli
// scan/list runs only on the on-demand GET /api/wifi.
type Net struct {
	cfg  *Config
	path string

	mu       sync.Mutex
	hostname string // live hostname (updated on rename)

	// capability flags, resolved once at construction (cheap, no per-poll fork)
	canRename     bool // hostnamectl present
	wifiSupported bool // nmcli present AND a wireless device exists
	comitup       bool // comitup binary present

	suggested string // sideshow-<serial4>, computed once

	saveMu sync.Mutex // serialize save()
}

// NetInfo is the GET /api/hostname payload + the cheap `net` block of the
// snapshot. It carries no Wi-Fi scan (that is the on-demand WifiInfo) — only a
// supported flag — to keep the snapshot poll free of subprocess forks.
type NetInfo struct {
	Hostname  string   `json:"hostname"`
	Suggested string   `json:"suggested"`  // sideshow-<serial4> default name
	CanRename bool     `json:"can_rename"` // hostnamectl available
	Protected bool     `json:"protected"`  // current name is load-bearing (deploy convention) → rename refused
	Comitup   bool     `json:"comitup"`    // comitup recovery-AP available
	Wifi      WifiCaps `json:"wifi"`
	Link      LiveNet  `json:"link"` // live connectivity (IP / online / Wi-Fi signal), fork-free
}

// LiveNet is the live connectivity summary for the System box (and the fleet
// heartbeat): the primary — default-route — interface and its IP/online state,
// plus, when that interface is wireless, the associated SSID and signal strength.
// Every field is read fork-free (from /proc + the Go net stdlib; the SSID via a
// WEXT ioctl — a syscall, not a subprocess) so it can ride the frequent snapshot
// poll without breaking Info()'s no-subprocess contract.
type LiveNet struct {
	Online   bool   `json:"online"`           // a default route exists
	Iface    string `json:"iface,omitempty"`  // primary interface, e.g. eth0 / wlp3s0
	IP       string `json:"ip,omitempty"`     // its IPv4 address
	Wireless bool   `json:"wireless"`         // the primary interface is a Wi-Fi device
	SSID     string `json:"ssid,omitempty"`   // associated SSID (wireless only)
	Signal   int    `json:"signal,omitempty"` // 0–100 link strength (wireless only)
}

// WifiCaps is the cheap Wi-Fi capability summary in the snapshot (no scan).
type WifiCaps struct {
	Supported bool `json:"supported"` // nmcli + a wireless device
}

// WifiInfo is the full, on-demand GET /api/wifi payload (forks nmcli).
type WifiInfo struct {
	Supported bool          `json:"supported"`
	Managed   bool          `json:"managed"`          // NetworkManager actually manages the wifi device
	Active    string        `json:"active,omitempty"` // currently-connected SSID
	Radio     string        `json:"radio,omitempty"`  // "enabled" | "disabled"
	Networks  []WifiNetwork `json:"networks"`         // scan results (merged with saved)
	Note      string        `json:"note,omitempty"`
}

// WifiNetwork is one visible or saved network. PSKs are never carried.
type WifiNetwork struct {
	SSID     string `json:"ssid"`
	Signal   int    `json:"signal"`             // 0–100 (0 for a saved-but-unseen network)
	Security string `json:"security,omitempty"` // "" = open
	Active   bool   `json:"active"`
	Saved    bool   `json:"saved"`
}

type persistedNet struct {
	Hostname string `json:"hostname"` // the operator's explicit choice (empty = never set)
}

// protectedHostnames are load-bearing in the deploy convention
// (nodes/<name>/fs → NODENAME). Renaming away from one would silently break
// deploys, so the agent refuses it (change it over ssh if you really mean to).
var protectedHostnames = map[string]bool{"disp": true, "disp-deb-air": true}

// genericHostnames are stock distro defaults the first-boot auto-namer may
// replace; anything else is assumed operator-chosen and left alone.
var genericHostnames = map[string]bool{"raspberrypi": true, "debian": true, "localhost": true, "archlinux": true, "": true}

// hostnameRE is an RFC-1123 label (lowercased): 1–63 chars, alphanumeric ends,
// hyphens allowed inside.
var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func NewNet(cfg *Config) *Net {
	n := &Net{cfg: cfg, hostname: cfg.Node}
	if cfg.StateFile != "" {
		n.path = filepath.Join(filepath.Dir(cfg.StateFile), "net.json")
	}
	n.load()
	n.canRename = lookPath("hostnamectl")
	n.comitup = lookPath("comitup")
	n.wifiSupported = lookPath("nmcli") && n.hasWifiDevice()
	n.suggested = "sideshow-" + serial4()
	return n
}

func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func (n *Net) load() {
	if n.path == "" {
		return
	}
	b, err := os.ReadFile(n.path)
	if err != nil {
		return
	}
	var p persistedNet
	if json.Unmarshal(b, &p) != nil {
		return
	}
	// The persisted name records the operator's choice; the OS hostname
	// (cfg.Node = os.Hostname at boot) remains the live truth. They agree unless
	// something reset /etc/hostname out from under us.
}

// chosenName returns the operator's persisted hostname choice (empty = none).
func (n *Net) chosenName() string {
	if n.path == "" {
		return ""
	}
	b, err := os.ReadFile(n.path)
	if err != nil {
		return ""
	}
	var p persistedNet
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	return p.Hostname
}

func (n *Net) saveChosen(name string) {
	if n.path == "" {
		return
	}
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	b, _ := json.MarshalIndent(persistedNet{Hostname: name}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(n.path), 0o755); err != nil {
		log.Printf("[net] state dir: %v", err)
		return
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[net] save: %v", err)
		return
	}
	if err := os.Rename(tmp, n.path); err != nil {
		log.Printf("[net] save rename: %v", err)
	}
}

// Hostname returns the live hostname (updated in-place on a rename).
func (n *Net) Hostname() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hostname
}

// Info is the cheap, fork-free snapshot block.
func (n *Net) Info() NetInfo {
	host := n.Hostname()
	return NetInfo{
		Hostname:  host,
		Suggested: n.suggested,
		CanRename: n.canRename,
		Protected: protectedHostnames[strings.ToLower(host)],
		Comitup:   n.comitup,
		Wifi:      WifiCaps{Supported: n.wifiSupported},
		Link:      n.Live(),
	}
}

// Live returns the fork-free live connectivity summary (see LiveNet). It reads
// the primary interface from /proc/net/route, its IPv4 from the Go net stdlib,
// and — when that interface is wireless — the signal from /proc/net/wireless and
// the SSID from a WEXT ioctl. No subprocess, so it is safe on the snapshot poll.
func (n *Net) Live() LiveNet {
	l := LiveNet{}
	if iface := defaultRouteIface(); iface != "" {
		l.Iface, l.Online = iface, true
		l.IP = ifaceIPv4(iface)
	} else {
		// No IPv4 default route: still surface an IP if any interface has one, and
		// count an IPv6-only default route as online.
		l.Online = hasDefaultRouteV6()
		if fi, ip := firstGlobalIPv4(); fi != "" {
			l.Iface, l.IP = fi, ip
		}
	}
	if l.Iface != "" && isWirelessIface(l.Iface) {
		l.Wireless = true
		if sig, ok := procWirelessSignal(l.Iface); ok {
			l.Signal = sig
		}
		l.SSID = wifiSSID(l.Iface)
	}
	return l
}

// SetHostname validates + applies a new hostname (hostnamectl set-hostname),
// updates the live value, and persists the operator's choice. It refuses to
// rename a load-bearing (protected) hostname and rejects invalid labels.
func (n *Net) SetHostname(name string) (NetInfo, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validHostname(name) {
		return NetInfo{}, fmt.Errorf("invalid hostname %q — use 1–63 letters, digits, or hyphens (not at the ends)", name)
	}
	cur := n.Hostname()
	if protectedHostnames[strings.ToLower(cur)] {
		return NetInfo{}, fmt.Errorf("refusing to rename %q — it is load-bearing in the deploy convention; change it over ssh if you really mean to", cur)
	}
	if !n.canRename {
		return NetInfo{}, fmt.Errorf("hostnamectl not available on this node")
	}
	if name == cur {
		n.saveChosen(name) // record the explicit choice even on a no-op
		return n.Info(), nil
	}
	if out, err := runShort(10*time.Second, "hostnamectl", "set-hostname", name); err != nil {
		return NetInfo{}, fmt.Errorf("hostnamectl: %v: %s", err, out)
	}
	n.mu.Lock()
	n.hostname = name
	n.mu.Unlock()
	n.saveChosen(name)
	log.Printf("[net] hostname set to %q", name)
	return n.Info(), nil
}

// ---- Wi-Fi (nmcli) ----

// WifiStatus runs the on-demand nmcli queries: the current SSID, the radio
// state, and a scan merged with saved connections. PSKs are never read or
// returned.
func (n *Net) WifiStatus() WifiInfo {
	if !lookPath("nmcli") {
		return WifiInfo{Supported: false, Networks: []WifiNetwork{}, Note: "NetworkManager (nmcli) is not installed"}
	}
	info := WifiInfo{Supported: true, Networks: []WifiNetwork{}}
	dev, managed := n.wifiDevice()
	if dev == "" {
		info.Supported = false
		info.Note = "no wireless device found"
		return info
	}
	info.Managed = managed
	if !managed {
		info.Note = "the wireless device is not managed by NetworkManager"
	}
	if out, err := runNmcli(5*time.Second, "-t", "-f", "WIFI", "radio"); err == nil {
		info.Radio = strings.TrimSpace(out)
	}
	saved := n.savedWifi() // set of SSID names with a saved connection

	// Scan (a rescan can be slow → a generous bound).
	out, _ := runNmcli(20*time.Second, "-t", "-f", "ACTIVE,SSID,SIGNAL,SECURITY", "dev", "wifi", "list")
	info.Networks, info.Active = parseWifiScan(out, saved)
	return info
}

// parseWifiScan turns `nmcli -t -f ACTIVE,SSID,SIGNAL,SECURITY dev wifi list`
// output into a deduped, sorted network list (merged with the saved set) and the
// active SSID. Pure (testable): the strongest signal wins per SSID, hidden
// (empty-SSID) rows are dropped, and saved-but-unseen networks are folded in so
// they can still be forgotten.
func parseWifiScan(out string, saved map[string]bool) ([]WifiNetwork, string) {
	networks := []WifiNetwork{}
	active := ""
	seen := map[string]int{} // SSID → index in networks
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitNmcli(line)
		if len(f) < 2 || f[1] == "" {
			continue // hidden / empty SSID
		}
		nw := WifiNetwork{
			SSID:     f[1],
			Active:   f[0] == "yes",
			Security: normalizeSecurity(field(f, 3)),
			Saved:    saved[f[1]],
			Signal:   atoiSafe(field(f, 2)),
		}
		if nw.Active {
			active = nw.SSID
		}
		if idx, ok := seen[nw.SSID]; ok {
			if nw.Signal > networks[idx].Signal {
				networks[idx].Signal = nw.Signal
			}
			networks[idx].Active = networks[idx].Active || nw.Active
			continue
		}
		seen[nw.SSID] = len(networks)
		networks = append(networks, nw)
	}
	// Fold in saved networks that were not in range (so they can be forgotten).
	for ssid := range saved {
		if _, ok := seen[ssid]; !ok {
			networks = append(networks, WifiNetwork{SSID: ssid, Saved: true})
		}
	}
	sortNetworks(networks)
	return networks, active
}

// WifiConnect joins a network (adding/activating a connection). An empty psk
// connects to an open network. A hidden network, or one that is simply not in
// range yet, can't be joined with `dev wifi connect` (that only sees SSIDs the
// live scan found) — so a saved autoconnect profile is created instead and
// NetworkManager joins it when the AP appears (see wifiSaveProfile). The psk is
// passed as a separate argv element (no shell) and never logged.
func (n *Net) WifiConnect(ssid, psk string, hidden bool) error {
	ssid = strings.TrimSpace(ssid)
	if ssid == "" {
		return fmt.Errorf("ssid required")
	}
	if strings.ContainsAny(ssid, "\x00\n\r") || len(ssid) > 32 {
		return fmt.Errorf("invalid ssid")
	}
	// `nmcli dev wifi connect` takes the SSID as a bare positional (it accepts no
	// `--` end-of-options terminator there), so reject a leading '-' — it could be
	// parsed as an option (e.g. `-a`/`--ask` makes nmcli block for interactive
	// input until our timeout). A dash-prefixed SSID must be joined over ssh. The
	// guard applies to the hidden path too (the SSID is still an nmcli token there).
	if strings.HasPrefix(ssid, "-") {
		return fmt.Errorf("invalid ssid (cannot start with '-')")
	}
	if psk != "" && (len(psk) < 8 || len(psk) > 63 || strings.ContainsAny(psk, "\x00\n\r")) {
		return fmt.Errorf("a Wi-Fi password must be 8–63 characters")
	}
	// A hidden network never appears in a passive scan — save a probing profile
	// (802-11-wireless.hidden=yes) straight away and try to bring it up now.
	if hidden {
		return n.wifiSaveProfile(ssid, psk, true, true)
	}
	args := []string{"dev", "wifi", "connect", ssid}
	if psk != "" {
		args = append(args, "password", psk)
	}
	if dev, _ := n.wifiDevice(); dev != "" {
		args = append(args, "ifname", dev)
	}
	// nmcli can block while associating/DHCP-ing → a generous bound.
	out, err := runNmcli(45*time.Second, args...)
	if err == nil {
		log.Printf("[net] wifi connect to %q ok", ssid)
		return nil
	}
	// Out of range? `dev wifi connect` only joins SSIDs the scan currently sees, so
	// a network that just isn't here yet fails with "No network with SSID … found".
	// Save a NON-hidden autoconnect profile (it broadcasts normally — setting the
	// hidden flag would needlessly leak the SSID in probe requests) so the node
	// joins it when the AP appears. This is the out-of-range add the webUI's
	// by-name form promises; any other failure (wrong key, device error) is real.
	if wifiNotInRange(out) {
		return n.wifiSaveProfile(ssid, psk, false, false)
	}
	return fmt.Errorf("connect failed: %s", firstLine(out))
}

// wifiSaveProfile creates (or updates in place) a saved Wi-Fi profile for a
// network that `nmcli dev wifi connect` can't join directly — either a hidden SSID
// or one that is simply out of range right now. The profile autoconnects, so
// NetworkManager joins it when the AP appears.
//
// hidden sets 802-11-wireless.hidden=yes (active probing) — pass it ONLY for a
// genuinely non-broadcasting network; a normal out-of-range network must not set
// it (that leaks the SSID in probe requests). When hidden is false the hidden
// property is left untouched, so re-saving an existing profile never flips it.
//
// tryUp says the network might be in range now (the hidden case): bring the
// profile up and surface a wrong-password failure. A known-out-of-range save
// (tryUp=false) skips activation — there is no AP to associate with, so `up` would
// only block to its deadline and then fail. The PSK is a separate argv element (no
// shell) and never logged.
func (n *Net) wifiSaveProfile(ssid, psk string, hidden, tryUp bool) error {
	exists := n.savedWifi()[ssid]
	props := []string{"802-11-wireless.ssid", ssid, "connection.autoconnect", "yes"}
	if hidden {
		props = append(props, "802-11-wireless.hidden", "yes")
	}
	if psk != "" {
		props = append(props, "wifi-sec.key-mgmt", "wpa-psk", "wifi-sec.psk", psk)
	}
	if exists {
		// Update IN PLACE so a failed change never destroys the working profile
		// (delete-then-add would strand the node if the add failed). Converting an
		// existing secured profile to open isn't handled here — supply a password to
		// update a secured network.
		args := append([]string{"connection", "modify", "id", ssid}, props...)
		if out, err := runNmcli(15*time.Second, args...); err != nil {
			return fmt.Errorf("update network failed: %s", firstLine(out))
		}
	} else {
		args := []string{"connection", "add", "type", "wifi", "con-name", ssid}
		if dev, _ := n.wifiDevice(); dev != "" {
			args = append(args, "ifname", dev)
		}
		args = append(args, props...)
		if out, err := runNmcli(15*time.Second, args...); err != nil {
			return fmt.Errorf("add network failed: %s", firstLine(out))
		}
	}
	if !tryUp {
		// Known out of range: the profile is saved and autoconnects when the AP shows
		// up; activating now would just block and fail against a missing AP.
		log.Printf("[net] network %q saved (out of range — will join when in range)", ssid)
		return nil
	}
	// Bring it up now. Distinguish a wrong-key/secrets failure (surface it, and drop
	// a just-added profile so a retry is clean) from a genuine out-of-range failure
	// (expected — the saved profile auto-connects when the AP appears).
	out, err := runNmcli(45*time.Second, "connection", "up", "id", ssid)
	if err == nil {
		log.Printf("[net] network %q saved and connected", ssid)
		return nil
	}
	if psk != "" && wifiAuthFailure(out) {
		if !exists {
			_, _ = runNmcli(10*time.Second, "connection", "delete", "id", ssid)
		}
		return fmt.Errorf("couldn't connect to %q — check the password (the network appears to be in range)", ssid)
	}
	log.Printf("[net] network %q saved (not connected now: %s)", ssid, firstLine(out))
	return nil
}

// wifiNotInRange reports whether an nmcli `dev wifi connect` failure is the
// "SSID isn't currently visible" case (rather than a wrong password or a device
// error), so WifiConnect can fall back to saving an autoconnect profile. nmcli
// prints "Error: No network with SSID '…' found." for this.
func wifiNotInRange(nmcliOut string) bool {
	return strings.Contains(strings.ToLower(nmcliOut), "no network with ssid")
}

// wifiAuthFailure reports whether an nmcli activation error looks like a
// wrong-key / missing-secrets failure rather than a not-in-range / timeout one,
// so the hidden-network path can surface a bad password instead of silently
// "saving" it. Matched loosely across NetworkManager reason strings.
func wifiAuthFailure(nmcliOut string) bool {
	s := strings.ToLower(nmcliOut)
	return strings.Contains(s, "secret") || strings.Contains(s, "802-1x") || strings.Contains(s, "password")
}

// WifiForget deletes a saved connection by SSID (its NetworkManager profile
// name). Only names that already have a saved profile are accepted.
func (n *Net) WifiForget(ssid string) error {
	ssid = strings.TrimSpace(ssid)
	if ssid == "" {
		return fmt.Errorf("ssid required")
	}
	if !n.savedWifi()[ssid] {
		return fmt.Errorf("no saved network %q", ssid)
	}
	if out, err := runNmcli(10*time.Second, "connection", "delete", "id", ssid); err != nil {
		return fmt.Errorf("forget failed: %s", firstLine(out))
	}
	log.Printf("[net] wifi forget %q ok", ssid)
	return nil
}

// hasWifiDevice reports whether nmcli sees any wireless device (used once at
// construction for the cheap `supported` flag).
func (n *Net) hasWifiDevice() bool {
	dev, _ := n.wifiDevice()
	return dev != ""
}

// wifiDevice returns the first wireless device name and whether it is managed
// by NetworkManager.
func (n *Net) wifiDevice() (string, bool) {
	out, err := runNmcli(5*time.Second, "-t", "-f", "DEVICE,TYPE,STATE", "dev")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(out, "\n") {
		f := splitNmcli(line)
		if len(f) >= 2 && f[1] == "wifi" {
			state := field(f, 2)
			return f[0], state != "unmanaged" && state != "unavailable"
		}
	}
	return "", false
}

// savedWifi returns the set of SSIDs (connection names) with a saved wifi
// profile. NetworkManager names a wifi profile after its SSID by default.
func (n *Net) savedWifi() map[string]bool {
	out, err := runNmcli(5*time.Second, "-t", "-f", "NAME,TYPE", "connection", "show")
	saved := map[string]bool{}
	if err != nil {
		return saved
	}
	for _, line := range strings.Split(out, "\n") {
		f := splitNmcli(line)
		if len(f) >= 2 && f[1] == "802-11-wireless" && f[0] != "" {
			saved[f[0]] = true
		}
	}
	return saved
}

// ---- boot-time actions (opt-in) ----

// MaybeAutoName, when -auto-hostname is set, renames a stock-default hostname to
// sideshow-<serial4> on first boot. It never touches an operator-chosen name (a
// persisted choice, or any non-generic hostname — which includes the protected
// deploy names). Best-effort: logged, never fatal.
func (n *Net) MaybeAutoName() {
	if !n.cfg.AutoHostname {
		return
	}
	cur := strings.ToLower(n.Hostname())
	if !genericHostnames[cur] {
		return // operator-chosen (or protected) → leave it
	}
	if n.chosenName() != "" {
		return // the operator already picked a name earlier
	}
	target := n.suggested
	if !validHostname(target) {
		return
	}
	if _, err := n.SetHostname(target); err != nil {
		log.Printf("[net] auto-name to %q: %v", target, err)
		return
	}
	log.Printf("[net] first-boot auto-name: %q → %q", cur, target)
}

// MaybeStartComitup, when -comitup is set, starts the comitup recovery Wi-Fi AP
// if the node has no default route and a wireless device exists — so a headless
// node that boots with no network can still be reached to configure Wi-Fi.
// Best-effort: logged, never fatal.
func (n *Net) MaybeStartComitup() {
	if !n.cfg.Comitup || !n.comitup {
		return
	}
	if hasDefaultRoute() {
		return
	}
	if dev, _ := n.wifiDevice(); dev == "" {
		return
	}
	log.Printf("[net] no default route + wifi present → starting comitup recovery AP")
	if out, err := runShort(15*time.Second, "systemctl", "start", "comitup"); err != nil {
		// systemctl may be absent or comitup not a registered unit → launch the
		// daemon DETACHED. comitup is a long-running foreground process that holds
		// the recovery AP; it must NOT run under runShort's kill deadline, which
		// would raise the AP and then SIGKILL it seconds later — defeating the very
		// purpose (keeping a network-less headless node reachable).
		cmd := exec.Command("comitup")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err2 := cmd.Start(); err2 != nil {
			log.Printf("[net] comitup start failed: systemctl=%v (%s); binary=%v", err, firstLine(out), err2)
			return
		}
		log.Printf("[net] comitup recovery AP launched detached (pid %d)", cmd.Process.Pid)
		go func() { _ = cmd.Wait() }() // reap the child without blocking or deadlining it
	}
}

// ---- helpers ----

func validHostname(s string) bool {
	return len(s) >= 1 && len(s) <= 63 && hostnameRE.MatchString(s)
}

// serial4 derives a short stable node suffix: the last 4 hex chars of the Pi
// serial (/proc/cpuinfo), else of the machine-id. "0000" if neither is readable.
func serial4() string {
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Serial") {
				if i := strings.LastIndex(line, ":"); i >= 0 {
					return last4hex(strings.TrimSpace(line[i+1:]))
				}
			}
		}
	}
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		return last4hex(strings.TrimSpace(string(b)))
	}
	return "0000"
}

// last4hex returns the last 4 lowercased hex chars of s, left-padded to 4.
func last4hex(s string) string {
	var hex strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			hex.WriteRune(r)
		}
	}
	h := hex.String()
	if h == "" {
		return "0000"
	}
	if len(h) > 4 {
		h = h[len(h)-4:]
	}
	for len(h) < 4 {
		h = "0" + h
	}
	return h
}

// splitNmcli splits one nmcli -t (terse) line on unescaped ':' and unescapes the
// backslash-escaped ':' and '\' within fields.
func splitNmcli(line string) []string {
	var fields []string
	var cur strings.Builder
	esc := false
	for _, r := range line {
		if esc {
			cur.WriteRune(r)
			esc = false
			continue
		}
		switch r {
		case '\\':
			esc = true
		case ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	fields = append(fields, cur.String())
	return fields
}

func field(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

func atoiSafe(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

// normalizeSecurity collapses nmcli's security tokens to a short label ("" =
// open). nmcli reports e.g. "WPA2", "WPA1 WPA2", "WPA3", "802.1X".
func normalizeSecurity(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "--" {
		return ""
	}
	switch {
	case strings.Contains(s, "WPA3"):
		return "WPA3"
	case strings.Contains(s, "WPA2"):
		return "WPA2"
	case strings.Contains(s, "WPA"):
		return "WPA"
	case strings.Contains(s, "WEP"):
		return "WEP"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// sortNetworks orders: active first, then saved, then by descending signal, then
// by SSID — a stable, deterministic order for the UI list.
func sortNetworks(ns []WifiNetwork) {
	for i := 1; i < len(ns); i++ {
		for j := i; j > 0 && lessNetwork(ns[j], ns[j-1]); j-- {
			ns[j], ns[j-1] = ns[j-1], ns[j]
		}
	}
}

func lessNetwork(a, b WifiNetwork) bool {
	if a.Active != b.Active {
		return a.Active
	}
	if a.Saved != b.Saved {
		return a.Saved
	}
	if a.Signal != b.Signal {
		return a.Signal > b.Signal
	}
	return a.SSID < b.SSID
}

// hasDefaultRoute reports whether the kernel routing table has a default route
// (IPv4 or IPv6), by reading /proc — no fork.
func hasDefaultRoute() bool {
	return defaultRouteIface() != "" || hasDefaultRouteV6()
}

// hasDefaultRouteV6 reports a *usable* IPv6 default route by reading /proc — no fork.
func hasDefaultRouteV6() bool {
	b, err := os.ReadFile("/proc/net/ipv6_route")
	if err != nil {
		return false
	}
	return parseHasDefaultRouteV6(string(b))
}

// parseHasDefaultRouteV6 reports whether /proc/net/ipv6_route holds a real default
// route: destination ::/0 (all-zero dest + "00" prefix length) that is UP, not a
// reject/unreachable route, and not on loopback. Without those guards a lingering
// lo or unreachable ::/0 row (common on IPv6-enabled hosts with no real IPv6
// uplink) makes an offline node report Online. Fields:
// dest destlen src srclen nexthop metric refcnt use flags iface. Pure (testable).
func parseHasDefaultRouteV6(content string) bool {
	const rtfUp, rtfReject = 0x1, 0x200
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[0] != "00000000000000000000000000000000" || f[1] != "00" {
			continue
		}
		if f[9] == "lo" {
			continue // a loopback ::/0 is not real connectivity
		}
		flags := parseHexU64(f[8])
		if flags&rtfUp == 0 || flags&rtfReject != 0 {
			continue // down, or a reject/unreachable/blackhole route
		}
		return true
	}
	return false
}

// defaultRouteIface returns the interface carrying the IPv4 default route (the
// lowest-metric one if several exist), or "" if there is none. Fork-free.
func defaultRouteIface() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return parseDefaultRouteIface(string(b))
}

// parseDefaultRouteIface picks the interface of the lowest-metric IPv4 default
// route (Destination 00000000) from /proc/net/route contents. Pure (testable).
func parseDefaultRouteIface(content string) string {
	best, bestMetric := "", int(^uint(0)>>1)
	for i, line := range strings.Split(content, "\n") {
		if i == 0 {
			continue // header
		}
		f := strings.Fields(line)
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if len(f) < 7 || f[1] != "00000000" {
			continue
		}
		metric, _ := strconv.Atoi(f[6])
		if metric < bestMetric {
			bestMetric, best = metric, f[0]
		}
	}
	return best
}

// ifaceIPv4 returns the first non-loopback, non-link-local IPv4 address of the
// named interface, or "". Uses the Go net stdlib (netlink) — no fork.
func ifaceIPv4(name string) string {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
			return ip4.String()
		}
	}
	return ""
}

// firstGlobalIPv4 returns the first up, non-loopback interface with a usable
// IPv4 address — a fallback for a node online without a default route. No fork.
func firstGlobalIPv4() (string, string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", ""
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ip := ifaceIPv4(ifi.Name); ip != "" {
			return ifi.Name, ip
		}
	}
	return "", ""
}

// isWirelessIface reports whether the named interface is a Wi-Fi device, by
// probing /sys/class/net/<iface>/wireless (present for any cfg80211 interface,
// associated or not). Fork-free.
func isWirelessIface(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat("/sys/class/net/" + name + "/wireless")
	return err == nil
}

// procWirelessSignal returns the 0–100 signal strength for iface from
// /proc/net/wireless, or (0,false) if it has no row (radio down / unassociated).
func procWirelessSignal(iface string) (int, bool) {
	b, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return 0, false
	}
	return parseProcWireless(string(b), iface)
}

// parseProcWireless extracts iface's signal (0–100) from /proc/net/wireless
// contents. Rows read like "wlp3s0: 0000   69.  -41.  -256 …" — the 3rd numeric
// field (level) is the RF signal in dBm, which maps to a percentage. Trailing
// dots (nmcli/iwconfig formatting) are stripped. Pure (testable).
func parseProcWireless(content, iface string) (int, bool) {
	for _, line := range strings.Split(content, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != iface {
			continue
		}
		f := strings.Fields(rest) // status link level noise ...
		if len(f) < 3 {
			return 0, false
		}
		dbm, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "."), 64)
		if err != nil {
			return 0, false
		}
		return dbmToPercent(dbm), true
	}
	return 0, false
}

// dbmToPercent maps an RF signal level in dBm to a 0–100 strength using the
// widely-used linear scale: −100 dBm (and weaker) → 0%, −50 dBm (and stronger) →
// 100%. Pure (testable).
func dbmToPercent(dbm float64) int {
	switch {
	case dbm >= -50:
		return 100
	case dbm <= -100:
		return 0
	default:
		return int(2*(dbm+100) + 0.5)
	}
}
