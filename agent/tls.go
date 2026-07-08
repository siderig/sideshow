package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TLSManager is the opt-in self-signed-HTTPS option: it generates a persistent
// certificate on the node and serves the control surface over HTTPS (a second
// listener alongside plain :80), so LAN traffic — including the auth key — is
// encrypted with zero external dependencies and nothing for the operator to
// install. The tradeoff is a browser "not secure" warning (no trusted root): this
// is for offline / airgapped LANs where Tailscale is not wanted. It is off by
// default and toggled from the wizard or Settings.
//
// The cert is regenerable and served via GetCertificate, so a regenerate swaps in
// the new cert for fresh connections without bouncing the listener.
type TLSManager struct {
	cfg     *Config
	netmgr  *Net // live-hostname source, so the cert CN/SANs match the CURRENT name
	handler http.Handler
	dir     string // cert directory (<state-dir>/tls)
	flag    string // tls.json — persists the enabled toggle

	mu      sync.Mutex
	enabled bool
	cert    *tls.Certificate
	meta    tlsMeta
	srv     *http.Server
	note    string
}

type tlsMeta struct {
	Fingerprint string
	NotAfter    time.Time
	SANs        []string
}

// TLSInfo is the GET /api/tls payload + cached snapshot block.
type TLSInfo struct {
	Enabled     bool     `json:"enabled"`               // an HTTPS listener is up
	Available   bool     `json:"available"`             // a self-signed cert exists
	HTTPSAddr   string   `json:"https_addr"`            // the HTTPS listen address (e.g. ":443")
	Fingerprint string   `json:"fingerprint,omitempty"` // SHA-256 of the cert (verify on first use)
	NotAfter    string   `json:"not_after,omitempty"`   // expiry, RFC3339
	SANs        []string `json:"sans,omitempty"`        // names/IPs the cert is valid for
	Note        string   `json:"note,omitempty"`
}

type persistedTLS struct {
	Enabled bool `json:"enabled"`
}

func NewTLSManager(cfg *Config, netmgr *Net, handler http.Handler) *TLSManager {
	m := &TLSManager{cfg: cfg, netmgr: netmgr, handler: handler}
	if cfg.StateFile != "" {
		base := filepath.Dir(cfg.StateFile)
		m.dir = filepath.Join(base, "tls")
		m.flag = filepath.Join(base, "tls.json")
	}
	m.loadFlag()
	_ = m.tryLoadCert() // populate meta if a cert already exists (ignored if none)
	return m
}

func (m *TLSManager) certPath() string { return filepath.Join(m.dir, "cert.pem") }
func (m *TLSManager) keyPath() string  { return filepath.Join(m.dir, "key.pem") }

// Info returns the cached status (fork-free) for the snapshot and GET /api/tls.
func (m *TLSManager) Info() TLSInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := TLSInfo{
		Enabled:   m.srv != nil,
		Available: m.cert != nil,
		HTTPSAddr: m.cfg.HTTPSAddr,
		Note:      m.note,
	}
	if m.cert != nil {
		info.Fingerprint = m.meta.Fingerprint
		info.SANs = m.meta.SANs
		if !m.meta.NotAfter.IsZero() {
			info.NotAfter = m.meta.NotAfter.UTC().Format(time.RFC3339)
		}
	}
	return info
}

// Enable ensures a cert exists and starts the HTTPS listener (idempotent). Binding
// the HTTPS port (443 by default) needs root, which the agent has; a bind failure
// is returned so the UI can show it, and the toggle is not persisted on failure.
func (m *TLSManager) Enable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dir == "" {
		return fmt.Errorf("no state directory configured for cert storage")
	}
	if err := m.ensureCertLocked(); err != nil {
		return err
	}
	if m.srv != nil {
		return nil // already serving
	}
	addr := m.cfg.HTTPSAddr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.note = "HTTPS listen failed: " + err.Error()
		return fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:   m.handler,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: m.getCertificate},
	}
	m.srv = srv
	m.note = ""
	m.enabled = true
	go func() {
		err := srv.ServeTLS(ln, "", "")
		if err != nil && err != http.ErrServerClosed {
			// The listener died unexpectedly (not a Disable). Clear our state so
			// Info() stops claiming HTTPS is up and a later Enable() can retry rather
			// than short-circuiting on a stale non-nil m.srv.
			log.Printf("[tls] https server exited: %v", err)
			m.mu.Lock()
			if m.srv == srv {
				m.srv = nil
				m.enabled = false
				m.note = "HTTPS stopped: " + err.Error()
			}
			m.mu.Unlock()
		}
	}()
	log.Printf("[tls] serving HTTPS on %s (self-signed)", addr)
	m.saveFlagLocked()
	return nil
}

// Disable stops the HTTPS listener (the cert is kept for a later re-enable).
func (m *TLSManager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv != nil {
		_ = m.srv.Close()
		m.srv = nil
	}
	m.enabled = false
	m.note = ""
	log.Printf("[tls] HTTPS disabled")
	m.saveFlagLocked()
	return nil
}

// Regenerate replaces the self-signed cert (new key + fresh 10-year validity).
// Live connections keep the old cert; new handshakes pick up the new one via
// GetCertificate — no listener bounce.
func (m *TLSManager) Regenerate() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dir == "" {
		return fmt.Errorf("no state directory configured for cert storage")
	}
	// Do NOT null m.cert first: if generation fails (e.g. a read-only SD card), the
	// live listener keeps serving the existing valid cert instead of going dark.
	// generateLocked swaps m.cert only after the new pair is written and loaded.
	return m.generateLocked()
}

// StartIfEnabled brings HTTPS up at boot when the operator previously enabled it.
func (m *TLSManager) StartIfEnabled() {
	m.mu.Lock()
	want := m.enabled
	m.mu.Unlock()
	if want {
		if err := m.Enable(); err != nil {
			log.Printf("[tls] could not restore HTTPS at boot: %v", err)
		}
	}
}

func (m *TLSManager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cert == nil {
		return nil, fmt.Errorf("no certificate")
	}
	return m.cert, nil
}

// ensureCertLocked loads an existing cert or generates one. Caller holds mu.
func (m *TLSManager) ensureCertLocked() error {
	if m.cert != nil {
		return nil
	}
	if _, err := os.Stat(m.certPath()); err == nil {
		if err := m.loadCertLocked(); err == nil {
			// A cert minted before NTP sync (RTC-less Pi) can load yet already be
			// expired once the clock corrects — regenerate rather than serve a dead
			// cert that fails every handshake.
			if time.Now().Before(m.meta.NotAfter) {
				return nil
			}
			log.Printf("[tls] existing certificate is expired — regenerating")
		}
	}
	return m.generateLocked()
}

// tryLoadCert loads the cert if present (used at construction). Locks mu.
func (m *TLSManager) tryLoadCert() error {
	if m.dir == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(m.certPath()); err != nil {
		return nil
	}
	return m.loadCertLocked()
}

// loadCertLocked reads the cert/key pair from disk into the cache. Caller holds mu.
func (m *TLSManager) loadCertLocked() error {
	cert, err := tls.LoadX509KeyPair(m.certPath(), m.keyPath())
	if err != nil {
		return err
	}
	m.cert = &cert
	m.meta = certMeta(&cert)
	return nil
}

// generateLocked writes a fresh self-signed cert and loads it. Caller holds mu.
func (m *TLSManager) generateLocked() error {
	host := m.netmgr.Hostname() // the LIVE hostname (post -auto-hostname / rename)
	dns, ips := selfSignedSANs(host)
	if err := writeSelfSignedCert(host, dns, ips, m.dir, m.certPath(), m.keyPath()); err != nil {
		return err
	}
	log.Printf("[tls] generated a self-signed certificate for %v", dns)
	return m.loadCertLocked()
}

// ---- enabled-flag persistence ----

func (m *TLSManager) loadFlag() {
	if m.flag == "" {
		return
	}
	b, err := os.ReadFile(m.flag)
	if err != nil {
		return
	}
	var p persistedTLS
	if json.Unmarshal(b, &p) == nil {
		m.enabled = p.Enabled
	}
}

// saveFlagLocked persists the enabled toggle. Caller holds mu.
func (m *TLSManager) saveFlagLocked() {
	if m.flag == "" {
		return
	}
	b, _ := json.MarshalIndent(persistedTLS{Enabled: m.enabled}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(m.flag), 0o755); err != nil {
		log.Printf("[tls] state dir: %v", err)
		return
	}
	tmp := m.flag + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[tls] save: %v", err)
		return
	}
	if err := os.Rename(tmp, m.flag); err != nil {
		log.Printf("[tls] save rename: %v", err)
	}
}

// ---- pure cert helpers (testable without a manager) ----

// selfSignedSANs returns the DNS names and IPs a node's self-signed cert should
// cover: the hostname, its .local mDNS name, loopback, and every non-loopback
// interface address.
func selfSignedSANs(node string) ([]string, []net.IP) {
	node = strings.TrimSpace(node)
	if node == "" {
		node = "sideshow"
	}
	dns := []string{node}
	if !strings.Contains(node, ".") {
		dns = append(dns, node+".local")
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	return dns, ips
}

// writeSelfSignedCert creates an ECDSA P-256 self-signed serving cert (10-year
// validity) covering dns/ips and writes cert.pem (0644) + key.pem (0600) into dir.
func writeSelfSignedCert(cn string, dns []string, ips []net.IP, dir, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	if cn == "" {
		cn = "sideshow"
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"sideshow (self-signed)"}},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// Write both to temp files (key 0600 so the private half is never briefly
	// world-readable), then rename into place — a crash mid-write can't leave a
	// half-written file or a mismatched key/cert pair.
	if err := os.WriteFile(keyPath+".tmp", keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath+".tmp", certPEM, 0o644); err != nil {
		return err
	}
	if err := os.Rename(keyPath+".tmp", keyPath); err != nil {
		return err
	}
	if err := os.Rename(certPath+".tmp", certPath); err != nil {
		return err
	}
	return nil
}

// certMeta extracts UI-facing metadata (fingerprint, expiry, SANs) from a loaded
// keypair.
func certMeta(cert *tls.Certificate) tlsMeta {
	if cert == nil || len(cert.Certificate) == 0 {
		return tlsMeta{}
	}
	sum := sha256.Sum256(cert.Certificate[0])
	meta := tlsMeta{Fingerprint: "SHA256:" + strings.ToUpper(hex.EncodeToString(sum[:]))}
	if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
		meta.NotAfter = leaf.NotAfter
		meta.SANs = append(meta.SANs, leaf.DNSNames...)
		for _, ip := range leaf.IPAddresses {
			meta.SANs = append(meta.SANs, ip.String())
		}
	}
	return meta
}
