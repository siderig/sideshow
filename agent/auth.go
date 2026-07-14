package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
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

func (s *Server) authEnabled() bool { return s.cfg.AuthKeyValue() != "" }

// ctEq is a constant-time string compare (0 for differing lengths).
func ctEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// authed reports whether the request carries the configured key.
func (s *Server) authed(r *http.Request) bool {
	key := s.cfg.AuthKeyValue()
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
	if !ctEq(body.Key, s.cfg.AuthKeyValue()) {
		writeErr(w, &apiError{code: 401, err: fmt.Errorf("invalid key")})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookie,
		Value:    s.cfg.AuthKeyValue(),
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

// handleAuthKey rotates the node's control key (POST {"key":"…"}). Pre-auth during
// first-run setup (setupExempt) so the operator can choose a key they know instead
// of the invisible minted one; gated by the current key afterward. It writes the
// key file and applies the new key LIVE (no restart) — so the very next request
// must carry it. An empty key is refused (that would silently disable auth).
func (s *Server) handleAuthKey(w http.ResponseWriter, r *http.Request) {
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
	key := strings.TrimSpace(body.Key)
	if err := validAuthKey(key); err != nil {
		writeErr(w, &apiError{code: 400, err: err})
		return
	}
	if s.cfg.AuthKeyFile == "" {
		writeErr(w, &apiError{code: 501, err: fmt.Errorf("no key file is configured on this node")})
		return
	}
	if err := writeAuthKeyFile(s.cfg.AuthKeyFile, key); err != nil {
		writeErr(w, &apiError{code: 500, err: fmt.Errorf("could not save the key: %w", err)})
		return
	}
	s.cfg.SetAuthKey(key) // apply live — the next request must use the new key
	log.Printf("[auth] control key rotated via API")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// validAuthKey gates a control key set via the API: 8–128 printable, non-space
// ASCII characters. Rejects empty (which would silently disable auth) and any
// whitespace/control byte, so the key round-trips cleanly through the key file,
// the Authorization header, and the cookie value.
func validAuthKey(k string) error {
	if len(k) < 8 || len(k) > 128 {
		return fmt.Errorf("a control key must be 8–128 characters")
	}
	for _, r := range k {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("a control key must be printable ASCII with no spaces")
		}
	}
	return nil
}
