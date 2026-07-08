package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelfSignedSANs(t *testing.T) {
	dns, ips := selfSignedSANs("disp")
	if len(dns) < 2 || dns[0] != "disp" || dns[1] != "disp.local" {
		t.Errorf("dns = %v, want [disp disp.local ...]", dns)
	}
	// loopback is always present.
	var haveLoop bool
	for _, ip := range ips {
		if ip.IsLoopback() {
			haveLoop = true
		}
	}
	if !haveLoop {
		t.Errorf("ips = %v, want a loopback entry", ips)
	}
	// A name that already looks FQDN-ish gets no .local appended.
	if d, _ := selfSignedSANs("a.b"); len(d) != 1 || d[0] != "a.b" {
		t.Errorf("dotted name → %v, want [a.b]", d)
	}
	// Empty node name falls back to a placeholder rather than an empty SAN.
	if d, _ := selfSignedSANs(""); d[0] != "sideshow" {
		t.Errorf("empty node → %v, want sideshow", d)
	}
}

func TestWriteSelfSignedCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	dns, ips := selfSignedSANs("disp")
	if err := writeSelfSignedCert("disp", dns, ips, dir, certPath, keyPath); err != nil {
		t.Fatalf("writeSelfSignedCert: %v", err)
	}

	// The private key must not be world/group-readable.
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key.pem perm = %o, want 600", perm)
	}

	// It loads as a usable keypair and covers the hostname + 10-year validity.
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	meta := certMeta(&cert)
	if !strings.HasPrefix(meta.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint = %q, want SHA256: prefix", meta.Fingerprint)
	}
	if meta.NotAfter.Before(time.Now().AddDate(9, 0, 0)) {
		t.Errorf("NotAfter = %v, want ~10 years out", meta.NotAfter)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if err := leaf.VerifyHostname("disp"); err != nil {
		t.Errorf("cert should be valid for 'disp': %v", err)
	}
	if err := leaf.VerifyHostname("disp.local"); err != nil {
		t.Errorf("cert should be valid for 'disp.local': %v", err)
	}
	// SAN metadata surfaces the names for the UI.
	var haveLocal bool
	for _, s := range meta.SANs {
		if s == "disp.local" {
			haveLocal = true
		}
	}
	if !haveLocal {
		t.Errorf("meta SANs = %v, want disp.local", meta.SANs)
	}
}

func TestCertMetaNil(t *testing.T) {
	if m := certMeta(nil); m.Fingerprint != "" || len(m.SANs) != 0 {
		t.Errorf("certMeta(nil) = %+v, want zero", m)
	}
}
