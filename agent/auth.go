package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Auth gates the whole control surface (webUI + API + screenshot + VNC) behind a
// single shared key, when one is configured (cfg.AuthKey, loaded from a config
// file). The key proves identity three ways, covering every browser surface:
//   - a cookie (set by POST /api/auth) — sent automatically on fetch, <img>,
//     the noVNC page navigation, AND the /vnc/ws WebSocket (all same-origin);
//   - an "Authorization: Bearer <key>" header — for API clients (curl, the
//     future fleet panel);
//   - an "X-Sideshow-Key" header — convenience alias for the above.
//
// Disabled (no gating) when cfg.AuthKey is empty, preserving the v0 LAN-only
// default. Plain-HTTP-on-LAN means the key/cookie is sniffable on the wire — the
// threat model is a trusted LAN / Tailscale, same as before; this stops casual
// access and accidental control, it is not TLS.
const authCookie = "sideshow_key"

func (s *Server) authEnabled() bool { return s.cfg.AuthKey != "" }

// ctEq is a constant-time string compare (0 for differing lengths).
func ctEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// authed reports whether the request carries the configured key.
func (s *Server) authed(r *http.Request) bool {
	key := s.cfg.AuthKey
	if c, err := r.Cookie(authCookie); err == nil && ctEq(c.Value, key) {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if ctEq(strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")), key) {
			return true
		}
	}
	return ctEq(r.Header.Get("X-Sideshow-Key"), key)
}

// authExempt is reachable without the key, so the browser can load the UI shell
// and submit the key. Everything else (all /api/*, /api/screenshot, /vnc*) is
// gated.
func authExempt(method, path string) bool {
	switch {
	case path == "/" && method == http.MethodGet: // the login/UI shell (no secrets)
		return true
	case path == "/api/auth" && method == http.MethodPost: // the login endpoint
		return true
	case path == "/api/health" && method == http.MethodGet: // liveness for external monitors
		return true
	case path == "/favicon.ico":
		return true
	}
	return false
}

// handleAuth validates the key and, on success, sets the auth cookie.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, &apiError{code: 405, err: fmt.Errorf("use POST")})
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, &apiError{code: 400, err: fmt.Errorf("invalid JSON: %w", err)})
		return
	}
	if !s.authEnabled() {
		writeJSON(w, 200, map[string]any{"ok": true, "auth": false}) // nothing to unlock
		return
	}
	if !ctEq(body.Key, s.cfg.AuthKey) {
		writeErr(w, &apiError{code: 401, err: fmt.Errorf("invalid key")})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookie,
		Value:    s.cfg.AuthKey,
		Path:     "/",
		HttpOnly: true,
		// The cookie value IS the key. When the login arrived over TLS (self-signed
		// HTTPS or `tailscale serve`), mark it Secure so the browser never sends it
		// back over the plain-HTTP :80 that stays up alongside, where a LAN sniffer
		// could lift it. Left off for a plain-HTTP login so :80-only nodes still work.
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
		MaxAge:   30 * 24 * 3600,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleLogout clears the auth cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: authCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}
