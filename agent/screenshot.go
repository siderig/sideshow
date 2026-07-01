package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CaptureKMS grabs the live screen via the universal DRM/KMS helper
// (disp-kmsshot): it reads whatever the active CRTC is scanning out — below
// X11, Wayland, the cog/KMS kiosk, a direct-KMS A/V app, or a bare console — so
// one path covers every mode, unlike CDP (web only) or scrot (X only). The
// helper emits PPM on stdout and GPU-detiles via EGL; we transcode to PNG here
// with the stdlib, keeping the agent pure-Go. Runs as root (the agent's uid):
// drmModeGetFB2 needs CAP_SYS_ADMIN.
func (s *Supervisor) CaptureKMS(ctx context.Context) ([]byte, error) {
	exe := s.cfg.KmsShotCmd
	if exe == "" {
		return nil, fmt.Errorf("KMS screenshot backend disabled (-kmsshot-cmd empty)")
	}
	card := s.cfg.DriCard
	if card == "" {
		card = "/dev/dri/card0"
	}
	// Bound the GPU work even if the caller passed an unbounded context — a
	// wedged driver must not hang the request goroutine.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}

	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, "-D", card)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s: %s", exe, msg)
	}

	img, err := decodePPM(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%s: bad output: %w", exe, err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// CaptureWayland grabs the labwc/Wayland primary surface with grim — the Wayland
// analogue of CaptureCompositor's scrot, used as the screenshot fallback when the
// universal KMS path is unavailable and a labwc session is on screen. grim speaks
// the wlr-screencopy protocol, so it runs as the seat user against the same
// wayland socket labwc serves (XDG_RUNTIME_DIR + WAYLAND_DISPLAY) — the same
// identity/env as the wlr-randr screen-sleep path — and writes PNG to stdout.
func (s *Supervisor) CaptureWayland() ([]byte, error) {
	sock := firstWaylandSocket(s.cfg.RuntimeDir)
	if sock == "" {
		return nil, fmt.Errorf("no wayland socket in %s (is labwc running?)", s.cfg.RuntimeDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "grim", "-t", "png", "-")
	cmd.Env = []string{
		"XDG_RUNTIME_DIR=" + s.cfg.RuntimeDir,
		"WAYLAND_DISPLAY=" + sock,
		"HOME=" + s.cfg.Home,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if s.cfg.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: s.cfg.cred}
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("grim: %s", msg)
	}
	return out.Bytes(), nil
}

// decodePPM parses the binary PPM (P6, maxval 255) that disp-kmsshot writes into
// an RGBA image. Minimal but tolerant of the standard whitespace/# comments in
// the header; the pixel block is opaque RGB.
func decodePPM(b []byte) (*image.RGBA, error) {
	i := 0
	token := func() (string, error) {
		for i < len(b) { // skip whitespace + comment lines
			switch c := b[i]; {
			case c == '#':
				for i < len(b) && b[i] != '\n' {
					i++
				}
			case c == ' ' || c == '\t' || c == '\n' || c == '\r':
				i++
			default:
				goto read
			}
		}
	read:
		start := i
		for i < len(b) {
			if c := b[i]; c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '#' {
				break
			}
			i++
		}
		if start == i {
			return "", io.ErrUnexpectedEOF
		}
		return string(b[start:i]), nil
	}

	magic, err := token()
	if err != nil {
		return nil, err
	}
	if magic != "P6" {
		return nil, fmt.Errorf("not a P6 PPM (got %q)", magic)
	}
	dims := make([]int, 0, 3)
	for len(dims) < 3 { // width, height, maxval
		t, err := token()
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(t)
		if err != nil {
			return nil, fmt.Errorf("bad header field %q", t)
		}
		dims = append(dims, n)
	}
	w, h, maxv := dims[0], dims[1], dims[2]
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("bad dimensions %dx%d", w, h)
	}
	if maxv != 255 {
		return nil, fmt.Errorf("unsupported maxval %d (want 255)", maxv)
	}
	// Exactly one whitespace byte separates the header from the pixel block.
	if i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	need := w * h * 3
	if len(b)-i < need {
		return nil, fmt.Errorf("truncated pixel data: have %d, want %d", len(b)-i, need)
	}
	px := b[i:]

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		dst := img.Pix[img.PixOffset(0, y):]
		src := px[y*w*3:]
		for x := 0; x < w; x++ {
			dst[x*4+0] = src[x*3+0]
			dst[x*4+1] = src[x*3+1]
			dst[x*4+2] = src[x*3+2]
			dst[x*4+3] = 255
		}
	}
	return img, nil
}
