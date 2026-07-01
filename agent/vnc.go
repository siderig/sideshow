package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	gws "github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// VNC provides on-demand remote screen viewing. It runs a capture server bound to
// localhost — x11vnc for the X surface, or wayvnc for the labwc Wayland primary —
// and the agent bridges a browser WebSocket (/vnc/ws) to it so the embedded noVNC
// viewer at /vnc works over the agent's :80 (no websockify, no extra exposed
// port; RFB is protocol-identical regardless of which server produces it). The
// server is started lazily when the first viewer connects and stopped shortly
// after the last one leaves (unless pinned), so the heavy capture only runs while
// someone is actually watching — important on the marginal Pi 3B.
type VNC struct {
	cfg      *Config
	sup      *Supervisor // to learn the on-screen backend (X → x11vnc, Wayland → wayvnc)
	x11vncOK bool        // x11vnc installed (captures the X surface)
	wayvncOK bool        // wayvnc installed (captures the wlroots/labwc surface)

	startMu sync.Mutex // serializes the start sequence (so the port-wait isn't under mu)

	mu        sync.Mutex // guards the state fields below
	cmd       *exec.Cmd
	running   bool
	clients   int
	pinned    bool // keep the server up even with no viewers (POST /api/vnc {on:true})
	startedAt time.Time
	lastErr   string
	stopTimer *time.Timer
}

// VNCStatus is the JSON for GET /api/vnc (also drives webUI feature detection).
type VNCStatus struct {
	Supported    bool   `json:"supported"`
	Running      bool   `json:"running"`
	Clients      int    `json:"clients"`
	Pinned       bool   `json:"pinned"`
	Port         int    `json:"port"`
	Controllable bool   `json:"controllable"`      // input allowed (only when the API is key-protected)
	Scale        string `json:"scale,omitempty"`   // active low-end capture downscale (x11vnc -scale)
	MaxFPS       int    `json:"max_fps,omitempty"` // active low-end frame-rate cap (0 = uncapped)
	Nice         int    `json:"nice,omitempty"`    // active low-end nice/idle-I/O increment (0 = normal)
	Since        string `json:"since,omitempty"`
	LastErr      string `json:"last_error,omitempty"`
	Note         string `json:"note,omitempty"`
}

func NewVNC(cfg *Config, sup *Supervisor) *VNC {
	_, ex := exec.LookPath("x11vnc")
	_, wv := exec.LookPath("wayvnc")
	return &VNC{cfg: cfg, sup: sup, x11vncOK: ex == nil, wayvncOK: wv == nil}
}

// waylandActive reports whether the labwc Wayland kiosk is the on-screen primary.
// It picks the capture backend: wayvnc attaches to labwc (wlr-screencopy), while
// x11vnc would scrape the now-suspended/hidden X session — the wrong surface.
func (v *VNC) waylandActive() bool {
	return v.sup != nil && v.sup.OnWaylandPrimary()
}

// backendSupported reports whether a capture server exists for the on-screen
// backend: wayvnc for the Wayland primary, else x11vnc for the X surface.
func (v *VNC) backendSupported() bool {
	if v.waylandActive() {
		return v.wayvncOK
	}
	return v.x11vncOK
}

func (v *VNC) Status() VNCStatus {
	wayland := v.waylandActive()
	supported := v.backendSupported()
	v.mu.Lock()
	defer v.mu.Unlock()
	st := VNCStatus{
		Supported:    supported,
		Running:      v.running,
		Clients:      v.clients,
		Pinned:       v.pinned,
		Port:         v.cfg.VNCPort,
		Controllable: v.cfg.AuthKey != "", // input only when the surface is key-protected
		Scale:        v.cfg.VNCScale,
		MaxFPS:       v.cfg.VNCMaxFPS,
		Nice:         v.cfg.VNCNice,
		LastErr:      v.lastErr,
	}
	if !supported {
		if wayland {
			st.Note = "live view needs wayvnc for the Wayland kiosk (not installed)"
		} else {
			st.Note = "live view needs x11vnc (not installed)"
		}
	}
	if v.running && !v.startedAt.IsZero() {
		st.Since = v.startedAt.UTC().Format(time.RFC3339)
	}
	return st
}

// Pin starts the server and keeps it up regardless of viewers; off clears that
// and stops it if nobody is watching. Backs POST /api/vnc {on:bool}.
func (v *VNC) Pin(on bool) error {
	if !v.backendSupported() {
		return &apiError{code: 501, err: fmt.Errorf("live view not available for the current backend (need %s)", v.backendBinary())}
	}
	if on {
		if err := v.ensureServer(); err != nil {
			return &apiError{code: 502, err: err}
		}
		v.mu.Lock()
		v.pinned = true
		v.mu.Unlock()
		return nil
	}
	v.mu.Lock()
	v.pinned = false
	if v.clients == 0 {
		v.stopLocked()
	}
	v.mu.Unlock()
	return nil
}

// HandleWS bridges a browser WebSocket to the local VNC server (RFB over TCP).
func (v *VNC) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !v.backendSupported() {
		http.Error(w, "vnc not available for the current backend", http.StatusNotImplemented)
		return
	}
	if err := v.clientConnect(); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer v.clientDisconnect()

	tcp, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", v.cfg.VNCPort), 5*time.Second)
	if err != nil {
		http.Error(w, "vnc server not reachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer tcp.Close()

	// Upgrade, negotiating the "binary" subprotocol if the client offers it
	// (older noVNC builds do; 1.5 uses raw binary frames either way).
	up := gws.HTTPUpgrader{Protocol: func(p string) bool { return p == "binary" }}
	conn, _, _, err := up.Upgrade(r, w)
	if err != nil {
		return
	}
	defer conn.Close()
	bridge(conn, tcp)
}

// bridge copies bytes both ways between a WebSocket (binary frames) and the RFB
// TCP stream until either side closes.
func bridge(ws net.Conn, tcp net.Conn) {
	done := make(chan struct{}, 2)
	// VNC server -> browser: raw TCP bytes wrapped as binary WS frames.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if n > 0 {
				if werr := wsutil.WriteServerBinary(ws, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	// browser -> VNC server: WS message payloads written to the TCP stream.
	go func() {
		for {
			data, op, err := wsutil.ReadClientData(ws)
			if err != nil {
				break
			}
			if op == gws.OpBinary || op == gws.OpText {
				if _, werr := tcp.Write(data); werr != nil {
					break
				}
			}
		}
		done <- struct{}{}
	}()
	<-done
	_ = tcp.Close()
	_ = ws.Close()
	<-done
}

// clientConnect registers a viewer, (lazily) starting the server, and cancels a
// pending idle-stop.
func (v *VNC) clientConnect() error {
	if err := v.ensureServer(); err != nil {
		return err
	}
	v.mu.Lock()
	v.clients++
	if v.stopTimer != nil {
		v.stopTimer.Stop()
		v.stopTimer = nil
	}
	v.mu.Unlock()
	return nil
}

// clientDisconnect deregisters a viewer and schedules an idle stop when the last
// one leaves (unless pinned).
func (v *VNC) clientDisconnect() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.clients > 0 {
		v.clients--
	}
	if v.clients == 0 && !v.pinned {
		// Grace period so a quick reload doesn't thrash x11vnc start/stop.
		v.stopTimer = time.AfterFunc(8*time.Second, func() {
			v.mu.Lock()
			defer v.mu.Unlock()
			if v.clients == 0 && !v.pinned {
				v.stopLocked()
			}
		})
	}
}

// backendBinary names the capture server for the current backend (for messages).
func (v *VNC) backendBinary() string {
	if v.waylandActive() {
		return "wayvnc"
	}
	return "x11vnc"
}

// buildServerCmd constructs the capture server for the on-screen backend, bound
// to localhost:VNCPort and (when the control surface is NOT key-protected) forced
// view-only so an open LAN can't drive the kiosk. wayvnc attaches to labwc via
// the seat user's wayland socket; x11vnc scrapes the X session.
//
// The low-end knobs (cfg.VNCScale / VNCMaxFPS / VNCNice) keep the capture cheap on
// weak nodes like the Pi 3B, where full-resolution software scraping on top of a
// software-rendered kiosk can thrash the box: -scale downsamples the framebuffer,
// the fps cap throttles polling/updates, and the nice/idle-I/O wrap stops the
// capturer from starving the kiosk for CPU and SD-card I/O.
func (v *VNC) buildServerCmd() (*exec.Cmd, error) {
	port := fmt.Sprintf("%d", v.cfg.VNCPort)
	var bin string
	var args, env []string

	if v.waylandActive() {
		sock := firstWaylandSocket(v.cfg.RuntimeDir)
		if sock == "" {
			return nil, fmt.Errorf("no wayland socket in %s (is labwc running?)", v.cfg.RuntimeDir)
		}
		bin = "wayvnc"
		if v.cfg.AuthKey == "" {
			args = append(args, "--disable-input") // view-only on an open LAN (also needs no virtual-input protocol)
		}
		if v.cfg.VNCMaxFPS > 0 {
			args = append(args, fmt.Sprintf("--max-fps=%d", v.cfg.VNCMaxFPS)) // throttle on low-end nodes
		}
		// (wayvnc has no capture downscale; -vnc-scale applies to x11vnc only.)
		args = append(args, "127.0.0.1", port) // bind localhost; reached via the agent's ws proxy
		env = []string{
			"XDG_RUNTIME_DIR=" + v.cfg.RuntimeDir,
			"WAYLAND_DISPLAY=" + sock,
			"HOME=" + v.cfg.Home,
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		}
	} else {
		bin = "x11vnc"
		args = []string{
			"-display", v.cfg.Display,
			"-localhost", // bind 127.0.0.1 only; reached via the agent's ws proxy
			"-rfbport", port,
			"-nopw",               // RFB-level auth off; access is gated by the agent's :80 key + localhost bind
			"-shared", "-forever", // multiple viewers; keep listening across disconnects
			"-noxdamage",   // vc4/KMS X DAMAGE can be unreliable
			"-ncache", "0", // no client-side cache band (saves RAM on the Pi)
			"-quiet",
		}
		if v.cfg.VNCScale != "" {
			args = append(args, "-scale", v.cfg.VNCScale) // downsample the capture on low-RAM/CPU nodes
		}
		if v.cfg.VNCMaxFPS > 0 {
			ms := 1000 / v.cfg.VNCMaxFPS
			if ms < 1 {
				ms = 1
			}
			msStr := strconv.Itoa(ms)
			// -wait caps the poll interval, -defer the update batching: together ≈ an fps ceiling.
			args = append(args, "-wait", msStr, "-defer", msStr)
		}
		// Allow keyboard/mouse ONLY when the control surface is key-protected;
		// otherwise force view-only so an open LAN can't drive the kiosk.
		if v.cfg.AuthKey == "" {
			args = append(args, "-viewonly")
		}
		env = []string{
			"DISPLAY=" + v.cfg.Display,
			"XAUTHORITY=" + v.cfg.XAuthority,
			"HOME=" + v.cfg.Home,
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		}
	}

	exe, finalArgs := lowPriorityWrap(v.cfg.VNCNice, bin, args)
	cmd := exec.Command(exe, finalArgs...)
	cmd.Env = env
	return cmd, nil
}

// lowPriorityWrap optionally prefixes the capture command with `nice -n N` and
// `ionice -c3` (idle I/O class) so the heavy framebuffer scrape runs below the
// kiosk for both CPU and SD-card I/O on a weak node — the single biggest lever
// against the "live view thrashes the Pi" failure. niceInc<=0 leaves the command
// unchanged; missing nice/ionice degrade gracefully (the available one is used,
// or none). Returns the executable and the full argv to hand to exec.Command.
func lowPriorityWrap(niceInc int, bin string, args []string) (string, []string) {
	if niceInc <= 0 {
		return bin, args
	}
	if niceInc > 19 {
		niceInc = 19
	}
	var chain []string
	if p, err := exec.LookPath("nice"); err == nil {
		chain = append(chain, p, "-n", strconv.Itoa(niceInc))
	}
	if p, err := exec.LookPath("ionice"); err == nil {
		chain = append(chain, p, "-c", "3") // idle I/O class — yields the SD card to the kiosk
	}
	if len(chain) == 0 {
		return bin, args // neither tool present; run unwrapped
	}
	full := append(chain, bin)
	full = append(full, args...)
	return full[0], full[1:]
}

// ensureServer starts the capture server if it isn't running and blocks until its
// RFB port accepts a connection. startMu serializes concurrent starts (so two
// viewers can't double-launch on the same port); the port-wait runs WITHOUT the
// state mutex, so Status()/disconnect aren't blocked during a cold start.
func (v *VNC) ensureServer() error {
	if !v.backendSupported() {
		return fmt.Errorf("live view not available: %s not installed", v.backendBinary())
	}
	v.startMu.Lock()
	defer v.startMu.Unlock()

	v.mu.Lock()
	if v.running {
		v.mu.Unlock()
		return nil
	}
	v.mu.Unlock()

	// A just-stopped instance may still be releasing the RFB port; wait for it to
	// free before binding, so a reconnect in the idle-stop window doesn't fail.
	addr := fmt.Sprintf("127.0.0.1:%d", v.cfg.VNCPort)
	waitPortFree(addr, 3*time.Second)

	bin := v.backendBinary()
	cmd, err := v.buildServerCmd()
	if err != nil {
		v.mu.Lock()
		v.lastErr = err.Error()
		v.mu.Unlock()
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if v.cfg.cred != nil {
		cmd.SysProcAttr.Credential = v.cfg.cred
	}
	cmd.Stdout = prefixWriter(os.Stdout, "vnc")
	cmd.Stderr = prefixWriter(os.Stderr, "vnc")
	if err := cmd.Start(); err != nil {
		v.mu.Lock()
		v.lastErr = err.Error()
		v.mu.Unlock()
		return fmt.Errorf("start %s: %w", bin, err)
	}
	v.mu.Lock()
	v.cmd = cmd
	v.running = true
	v.startedAt = time.Now()
	v.lastErr = ""
	v.mu.Unlock()
	log.Printf("[vnc] %s started pid=%d on :%d (localhost)", bin, cmd.Process.Pid, v.cfg.VNCPort)

	// Reap the child when it exits so v.running tracks reality.
	go func() {
		_ = cmd.Wait()
		v.mu.Lock()
		if v.cmd == cmd {
			v.running = false
			v.cmd = nil
		}
		v.mu.Unlock()
		log.Printf("[vnc] %s exited", bin)
	}()

	// Wait for the RFB port to accept before the caller dials it (no state lock).
	// Generous: x11vnc + libvncserver init can be slow on a loaded Pi (e.g. when
	// pinned right after a restart while Chromium is still cold-starting).
	for i := 0; i < 100; i++ { // ~20s
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("x11vnc did not open %s in time", addr)
}

// waitPortFree blocks until nothing accepts on addr (a prior x11vnc has released
// it) or the timeout elapses. Best-effort — proceeds regardless at the deadline.
func waitPortFree(addr string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			return // nothing listening → free
		}
		c.Close()
		time.Sleep(150 * time.Millisecond)
	}
}

// stopLocked kills the VNC server process group. Caller holds v.mu.
func (v *VNC) stopLocked() {
	if v.cmd != nil && v.cmd.Process != nil {
		killGroup(v.cmd.Process.Pid, syscall.SIGTERM)
		log.Printf("[vnc] x11vnc stopped (idle)")
	}
	v.running = false
	v.cmd = nil
}
