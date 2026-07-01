package main

import (
	"sort"
	"strings"
	"testing"
)

func TestRecommendedCompositor(t *testing.T) {
	cases := map[string]string{"amd64": "wayland", "386": "wayland", "arm64": "x11", "arm": "x11", "riscv64": "x11"}
	for arch, want := range cases {
		if got := recommendedCompositor(arch); got != want {
			t.Errorf("recommendedCompositor(%q) = %q, want %q", arch, got, want)
		}
	}
}

func TestFeatureCatalogBaseRequired(t *testing.T) {
	s := &Setup{cfg: &Config{}}
	for _, comp := range []string{"x11", "wayland"} {
		feats := s.featureCatalog(comp)
		if len(feats) == 0 {
			t.Fatalf("%s catalog empty", comp)
		}
		if feats[0].Key != "base" || !feats[0].Required {
			t.Errorf("%s: first feature must be the required base, got %+v", comp, feats[0])
		}
		// Chromium is the one package every kiosk needs.
		if !contains(feats[0].Packages, "chromium") {
			t.Errorf("%s base must include chromium, got %v", comp, feats[0].Packages)
		}
	}
	// The compositor binary differs by choice.
	x11 := s.featureCatalog("x11")[0].Packages
	way := s.featureCatalog("wayland")[0].Packages
	if !contains(x11, "matchbox-window-manager") {
		t.Errorf("x11 base should include matchbox, got %v", x11)
	}
	if !contains(way, "labwc") || !contains(way, "seatd") {
		t.Errorf("wayland base should include labwc+seatd, got %v", way)
	}
}

func TestPackagesForUnionDedupSorted(t *testing.T) {
	s := &Setup{cfg: &Config{}}

	// No features → just the required base (x11), sorted.
	base := s.packagesFor("x11", nil)
	want := []string{"chromium", "matchbox-window-manager", "unclutter-xfixes", "xauth"}
	sort.Strings(want)
	if strings.Join(base, ",") != strings.Join(want, ",") {
		t.Errorf("x11 base packages = %v, want %v", base, want)
	}

	// base + airplay, deduped + sorted; airplay pkgs present, base pkgs present.
	got := s.packagesFor("wayland", []string{"airplay"})
	for _, p := range []string{"chromium", "labwc", "seatd", "uxplay", "avahi-daemon", "gstreamer1.0-plugins-bad"} {
		if !contains(got, p) {
			t.Errorf("wayland+airplay missing %q; got %v", p, got)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("packages not sorted: %v", got)
	}
	if hasDup(got) {
		t.Errorf("packages contain a duplicate: %v", got)
	}

	// Unknown feature key is ignored (still just the base).
	unknown := s.packagesFor("x11", []string{"does-not-exist"})
	if strings.Join(unknown, ",") != strings.Join(base, ",") {
		t.Errorf("unknown key changed the package set: %v", unknown)
	}
}

func TestSetupURL(t *testing.T) {
	cases := map[string]string{
		":80":            "http://127.0.0.1/setup",
		":8080":          "http://127.0.0.1:8080/setup",
		"0.0.0.0:80":     "http://127.0.0.1/setup",
		"127.0.0.1:9000": "http://127.0.0.1:9000/setup",
	}
	for addr, want := range cases {
		if got := setupURL(&Config{Addr: addr}); got != want {
			t.Errorf("setupURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func hasDup(ss []string) bool {
	seen := map[string]bool{}
	for _, s := range ss {
		if seen[s] {
			return true
		}
		seen[s] = true
	}
	return false
}
