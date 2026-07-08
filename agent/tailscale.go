package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tailscale is the opt-in overlay-network manager: it drives the preinstalled
// `tailscale` CLI so an operator can *choose* to join a tailnet (from the setup
// wizard or Settings) and reach the node over an encrypted WireGuard mesh — with,
// optionally, a real ts.net HTTPS cert in front of the control UI via
// `tailscale serve`. Joining is never forced: a fresh node is installed but
// logged out, and stays that way until someone pastes an auth key.
//
// Transport note: even plain HTTP to the node over the tailnet is WireGuard-
// encrypted end to end; `tailscale serve` additionally gives the browser a
// trusted padlock. Both are surfaced so the operator understands the difference.
//
// Snapshot cost: Info() is fork-free (returns a cached status refreshed by a
// background ticker), because the webUI polls the snapshot frequently on weak
// nodes; the on-demand GET /api/tailscale forces a fresh `tailscale status` fork.
type Tailscale struct {
	cfg       *Config
	netmgr    *Net   // live-hostname source (updated on rename), so we join as the CURRENT name
	installed bool   // the tailscale CLI is present
	path      string // tailscale.json — persists the operator's "serve" intent

	mu     sync.Mutex
	cached TailscaleInfo
	serve  bool // operator asked us to front the UI over HTTPS (persisted)

	refreshMu sync.Mutex // serializes refresh() so a slow, stale poll can't clobber fresh status
}

// TailscaleInfo is the GET /api/tailscale payload and the cached snapshot block.
// It carries no secrets (auth keys are write-only, never stored or returned).
type TailscaleInfo struct {
	Installed bool     `json:"installed"`          // the tailscale CLI is present
	Running   bool     `json:"running"`            // tailscaled answered a status query
	LoggedIn  bool     `json:"logged_in"`          // joined a tailnet (BackendState=Running)
	State     string   `json:"state,omitempty"`    // raw BackendState (NeedsLogin/Running/Stopped…)
	Tailnet   string   `json:"tailnet,omitempty"`  // the joined tailnet's name
	DNSName   string   `json:"dns_name,omitempty"` // this node's MagicDNS name (…​.ts.net)
	IPs       []string `json:"ips,omitempty"`      // tailnet IP(s)
	ServeOn   bool     `json:"serve_on"`           // `tailscale serve` fronting the UI (HTTPS)
	URL       string   `json:"url,omitempty"`      // best reachable overlay URL for the UI
	Note      string   `json:"note,omitempty"`
}

type persistedTailscale struct {
	Serve bool `json:"serve"`
}

// tsStatus is the subset of `tailscale status --json` we read.
type tsStatus struct {
	BackendState string `json:"BackendState"`
	Self         *struct {
		DNSName      string   `json:"DNSName"`
		HostName     string   `json:"HostName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
	CurrentTailnet *struct {
		Name           string `json:"Name"`
		MagicDNSSuffix string `json:"MagicDNSSuffix"`
	} `json:"CurrentTailnet"`
}

func NewTailscale(cfg *Config, netmgr *Net) *Tailscale {
	t := &Tailscale{cfg: cfg, netmgr: netmgr, installed: lookPath("tailscale")}
	if cfg.StateFile != "" {
		t.path = filepath.Join(filepath.Dir(cfg.StateFile), "tailscale.json")
	}
	t.loadServe()
	t.cached = TailscaleInfo{Installed: t.installed, ServeOn: t.serve}
	return t
}

// Start kicks off the background status refresher (a no-op if tailscale is not
// installed) so the snapshot's cached block stays current without forking on the
// poll path.
func (t *Tailscale) Start() {
	if !t.installed {
		return
	}
	go func() {
		t.refresh()
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for range tick.C {
			t.refresh()
		}
	}()
}

// Info returns the cached status (fork-free) for the snapshot.
func (t *Tailscale) Info() TailscaleInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cached
}

// Status forces a refresh (forks `tailscale status`) and returns it — the
// on-demand GET /api/tailscale.
func (t *Tailscale) Status() TailscaleInfo {
	t.refresh()
	return t.Info()
}

// refresh re-reads live status and updates the cache. Lenient: it parses the JSON
// even when the CLI exits non-zero (logged-out still yields a valid document).
func (t *Tailscale) refresh() {
	if !t.installed {
		t.store(TailscaleInfo{Installed: false})
		return
	}
	// Serialize refreshes so a slow, stale `tailscale status` can't land its store()
	// after a newer one (e.g. the refresh Up()/SetServe() triggers) and revert the
	// cache to a pre-join view until the next tick.
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	t.mu.Lock()
	serve := t.serve
	t.mu.Unlock()

	out, err := runShort(6*time.Second, "tailscale", "status", "--json")
	info := parseTailscaleStatus(out, serve)
	if !info.Running && info.Note == "" && err != nil {
		info.Note = err.Error()
	}
	t.store(info)
}

// parseTailscaleStatus builds a TailscaleInfo from `tailscale status --json`
// output (the CLI is assumed installed). A parse failure means the daemon is
// unreachable or gave unexpected output → Running=false with a hint. Pure, so the
// status mapping is unit-tested without forking tailscale.
func parseTailscaleStatus(out string, serve bool) TailscaleInfo {
	info := TailscaleInfo{Installed: true, ServeOn: serve}
	var st tsStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		info.Note = firstLine(out)
		return info
	}
	info.Running = true
	info.State = st.BackendState
	info.LoggedIn = st.BackendState == "Running"
	if st.Self != nil {
		info.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
		info.IPs = st.Self.TailscaleIPs
	}
	if st.CurrentTailnet != nil {
		info.Tailnet = st.CurrentTailnet.Name
	}
	info.URL = tsOverlayURL(info.DNSName, info.IPs, serve)
	if !info.LoggedIn && info.Note == "" {
		info.Note = "not joined to a tailnet"
	}
	return info
}

// tsOverlayURL is the best URL to reach this node over the tailnet: the MagicDNS
// name with https when fronted by serve, else http (still WireGuard-encrypted);
// falls back to the raw tailnet IP.
func tsOverlayURL(dnsName string, ips []string, serve bool) string {
	if dnsName != "" {
		if serve {
			return "https://" + dnsName
		}
		return "http://" + dnsName
	}
	if len(ips) > 0 {
		host := ips[0]
		if strings.Contains(host, ":") { // IPv6 literal → bracket it for a valid URL
			host = "[" + host + "]"
		}
		return "http://" + host
	}
	return ""
}

func (t *Tailscale) store(info TailscaleInfo) {
	t.mu.Lock()
	t.cached = info
	t.mu.Unlock()
}

// Up joins a tailnet with a pre-auth key. The key is passed as a separate argv
// element (no shell) and is never logged or persisted — tailscale stores its own
// node identity, so we hold no secret afterwards. When serve is set, the control
// UI is additionally fronted with a real ts.net HTTPS cert.
func (t *Tailscale) Up(authKey string, ssh, serve bool) error {
	if !t.installed {
		return fmt.Errorf("tailscale is not installed on this node")
	}
	authKey = strings.TrimSpace(authKey)
	if authKey == "" {
		return fmt.Errorf("an auth key is required to join a tailnet")
	}
	if strings.ContainsAny(authKey, " \t\r\n\x00") {
		return fmt.Errorf("invalid auth key")
	}
	// Join under the LIVE hostname, not the boot-time cfg.Node: -auto-hostname or a
	// later rename can change it, and we want the MagicDNS name to match what the UI
	// advertises rather than a stale "raspberrypi".
	host := tsHostname(t.netmgr.Hostname())
	args := []string{"up", "--authkey", authKey, "--hostname", host, "--accept-dns=true"}
	if ssh {
		args = append(args, "--ssh")
	}
	if out, err := runShort(60*time.Second, "tailscale", args...); err != nil {
		return fmt.Errorf("join failed: %s", firstLine(out))
	}
	log.Printf("[tailscale] joined tailnet as %q", host)
	if serve {
		if err := t.SetServe(true); err != nil {
			// The node IS joined (traffic already WireGuard-encrypted); only the
			// padlock front failed — surface it without unwinding the join.
			t.refresh()
			return fmt.Errorf("joined, but serving the UI over HTTPS failed: %v", err)
		}
	}
	t.refresh()
	return nil
}

// Down leaves the tailnet (clears any serve config first, then logs out so the
// node is removed from the tailnet — the clean opt-out for an opt-in feature).
func (t *Tailscale) Down() error {
	if !t.installed {
		return fmt.Errorf("tailscale is not installed on this node")
	}
	_ = t.SetServe(false) // best-effort; a logged-out node has nothing to serve
	if out, err := runShort(20*time.Second, "tailscale", "logout"); err != nil {
		return fmt.Errorf("leave failed: %s", firstLine(out))
	}
	log.Printf("[tailscale] left the tailnet (logged out)")
	t.refresh()
	return nil
}

// SetServe turns `tailscale serve` on or off in front of the agent's HTTP port,
// giving the control UI a trusted ts.net certificate. Requires HTTPS certificates
// to be enabled for the tailnet (an admin-console setting) — that error is
// surfaced verbatim so the operator knows the one-time step.
func (t *Tailscale) SetServe(on bool) error {
	if !t.installed {
		return fmt.Errorf("tailscale is not installed on this node")
	}
	if on {
		port := agentPort(t.cfg.Addr)
		if out, err := runShort(20*time.Second, "tailscale", "serve", "--bg", port); err != nil {
			return fmt.Errorf("%s", firstLine(out))
		}
	} else {
		// `serve reset` clears all serve config; ignore "nothing to do" errors.
		if out, err := runShort(20*time.Second, "tailscale", "serve", "reset"); err != nil {
			return fmt.Errorf("%s", firstLine(out))
		}
	}
	t.mu.Lock()
	t.serve = on
	t.mu.Unlock()
	t.saveServe()
	t.refresh()
	return nil
}

// MaybeJoinFromKeyFile is the automatic first-boot path for flashed images: if a
// pre-auth key was dropped at the key file and the node is not yet joined, join
// and then remove the file so no key lingers on disk. Best-effort: logged, never
// fatal. Opt-in by construction — it does nothing unless someone placed a key.
// Tailscale SSH is deliberately NOT enabled here (ssh=false): joining a tailnet
// must not silently grant a tailnet-wide root shell; the operator enables SSH
// explicitly from Settings if they want it.
func (t *Tailscale) MaybeJoinFromKeyFile() {
	if !t.installed || t.cfg.TailscaleAuthKeyFile == "" {
		return
	}
	b, err := os.ReadFile(t.cfg.TailscaleAuthKeyFile)
	if err != nil {
		return // no key staged → nothing to do
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return
	}
	if t.Status().LoggedIn {
		// Already joined (tailscale persists its own state) — clear the stale key.
		t.shredKeyFile()
		return
	}
	log.Printf("[tailscale] first-boot: joining tailnet from staged key file")
	if err := t.Up(key, false, false); err != nil {
		log.Printf("[tailscale] first-boot join failed: %v", err)
		return // leave the key so a later boot can retry
	}
	t.shredKeyFile()
}

func (t *Tailscale) shredKeyFile() {
	if t.cfg.TailscaleAuthKeyFile == "" {
		return
	}
	if err := os.Remove(t.cfg.TailscaleAuthKeyFile); err != nil && !os.IsNotExist(err) {
		log.Printf("[tailscale] could not remove staged key file: %v", err)
	}
}

// ---- serve-intent persistence ----

func (t *Tailscale) loadServe() {
	if t.path == "" {
		return
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var p persistedTailscale
	if json.Unmarshal(b, &p) == nil {
		t.serve = p.Serve
	}
}

func (t *Tailscale) saveServe() {
	if t.path == "" {
		return
	}
	t.mu.Lock()
	serve := t.serve
	t.mu.Unlock()
	b, _ := json.MarshalIndent(persistedTailscale{Serve: serve}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		log.Printf("[tailscale] state dir: %v", err)
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[tailscale] save: %v", err)
		return
	}
	if err := os.Rename(tmp, t.path); err != nil {
		log.Printf("[tailscale] save rename: %v", err)
	}
}

// ---- helpers ----

// tsHostname sanitizes a node name into a tailscale-acceptable hostname label
// (lowercase alphanumerics + hyphens); tailscale sanitizes again server-side, but
// a clean value keeps the MagicDNS name predictable.
func tsHostname(node string) string {
	node = strings.ToLower(strings.TrimSpace(node))
	var b strings.Builder
	for _, r := range node {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.':
			b.WriteByte('-')
		}
	}
	h := strings.Trim(b.String(), "-")
	if h == "" {
		return "sideshow-node"
	}
	return h
}

// agentPort extracts the port the agent serves on from its listen address (":80"
// → "80"), defaulting to 80. `tailscale serve --bg <port>` fronts localhost:port.
func agentPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "80"
	}
	return port
}
