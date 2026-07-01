package main

import (
	"image/color"
	"testing"
)

func TestDecodePPM(t *testing.T) {
	// 2x1, pixels (10,20,30) and (40,50,60).
	body := []byte("P6\n2 1\n255\n")
	body = append(body, 10, 20, 30, 40, 50, 60)
	img, err := decodePPM(body)
	if err != nil {
		t.Fatalf("decodePPM: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 2 || b.Dy() != 1 {
		t.Fatalf("bounds = %v, want 2x1", b)
	}
	if got, want := img.RGBAAt(0, 0), (color.RGBA{10, 20, 30, 255}); got != want {
		t.Errorf("px(0,0) = %v, want %v", got, want)
	}
	if got, want := img.RGBAAt(1, 0), (color.RGBA{40, 50, 60, 255}); got != want {
		t.Errorf("px(1,0) = %v, want %v", got, want)
	}
}

func TestDecodePPMHeaderWhitespaceAndComments(t *testing.T) {
	// Comment line + irregular whitespace in the header (valid Netpbm).
	body := []byte("P6\n# a comment\n2   1\n255\n")
	body = append(body, 1, 2, 3, 4, 5, 6)
	img, err := decodePPM(body)
	if err != nil {
		t.Fatalf("decodePPM: %v", err)
	}
	if img.RGBAAt(1, 0) != (color.RGBA{4, 5, 6, 255}) {
		t.Errorf("px(1,0) = %v", img.RGBAAt(1, 0))
	}
}

func TestDecodePPMErrors(t *testing.T) {
	cases := map[string][]byte{
		"wrong magic":  []byte("P3\n1 1\n255\n\x00\x00\x00"),
		"bad maxval":   []byte("P6\n1 1\n65535\n\x00\x00"),
		"truncated":    []byte("P6\n4 4\n255\nonly-a-few-bytes"),
		"empty":        {},
		"no dimension": []byte("P6\n255\n"),
	}
	for name, in := range cases {
		if _, err := decodePPM(in); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}
