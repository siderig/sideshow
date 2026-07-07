package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Heartbeat POSTs a compact node-status payload to a central aggregator on a
// timer (the bridge to the fleet panel). Disabled unless -heartbeat-url is set.
// Best-effort: failures are logged, never fatal.
type Heartbeat struct {
	cfg    *Config
	sup    *Supervisor
	stats  *Stats
	net    *Net
	client *http.Client
}

func NewHeartbeat(cfg *Config, sup *Supervisor, stats *Stats, netmgr *Net) *Heartbeat {
	return &Heartbeat{cfg: cfg, sup: sup, stats: stats, net: netmgr, client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *Heartbeat) Start() {
	if h.cfg.HeartbeatURL == "" {
		return
	}
	interval := h.cfg.HeartbeatSec
	if interval < 10 {
		interval = 10
	}
	go func() {
		time.Sleep(15 * time.Second)
		h.send()
		t := time.NewTicker(time.Duration(interval) * time.Second)
		defer t.Stop()
		for range t.C {
			h.send()
		}
	}()
}

func (h *Heartbeat) send() {
	ms := h.sup.Status()
	st := h.stats.Snapshot()
	node := h.net.Hostname()
	ln := h.net.Live()
	payload := map[string]any{
		"node":         node,
		"label":        h.cfg.NodeLabel,
		"group":        h.cfg.NodeGroup,
		"time":         time.Now().UTC().Format(time.RFC3339),
		"health":       health(ms),
		"cdp_attached": h.sup.CDPAttached(),
		"mode":         map[string]any{"type": ms.Type, "display": ms.Display, "state": ms.State, "params": ms.Params},
		"stats": map[string]any{
			"uptime_s": st.UptimeS, "cpu_percent": st.CPUPercent, "load": st.Load,
			"mem_percent": st.Mem.Percent, "disk_percent": st.Disk.Percent,
			"temp_c": st.TempC, "undervolt": st.UnderVolt,
			"resolution": st.Resolution, "upgrades": st.Upgrades.Available,
		},
		"net": map[string]any{
			"online": ln.Online, "iface": ln.Iface, "ip": ln.IP,
			"wireless": ln.Wireless, "ssid": ln.SSID, "signal": ln.Signal,
		},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, h.cfg.HeartbeatURL, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sideshow-Node", node)
	resp, err := h.client.Do(req)
	if err != nil {
		log.Printf("[heartbeat] post failed: %v", err)
		return
	}
	resp.Body.Close()
}
