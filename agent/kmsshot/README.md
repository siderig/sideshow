# disp-kmsshot — one screenshot path for every display mode

`disp-kmsshot` captures whatever a CRTC is **currently scanning out**, by reading
the active framebuffer(s) over the DRM/KMS API and GPU-detiling them through EGL.
It works *below* whatever drew the screen, so a single binary covers every
sideshow surface:

| Mode | Old per-mode capture | `disp-kmsshot` |
|------|----------------------|----------------|
| Chromium kiosk (X11) | CDP / `scrot` | ✅ |
| cog kiosk (`web`+`display:kms`) | **none** (CDP absent → 503) | ✅ |
| labwc / Wayland primary | `grim` (wlroots-only) | ✅ |
| `media`/`app` on the X compositor | `scrot` | ✅ |
| `airplay`/`media` on direct KMS (`kmssink`,`--vo=drm`) | **none** | ✅ |
| bare console | **none** | ✅ |

## Why a separate C helper (not in the Go agent)

1. **The agent stays pure-Go and cross-compilable.** Universal detile needs
   EGL+GLES2+GBM; linking those via cgo would break the clean arm64/amd64 build.
   This helper is compiled *on the node* against its real Mesa.
2. **Fault isolation.** GPU/EGL init can hang or crash on a driver hiccup. As a
   short-lived subprocess that's a non-zero exit the agent reports — not a crash
   of the `Restart=always` root service that owns the kiosk.
3. **No GPU state held by the always-on service.** Open GPU → grab one frame → exit.

The agent execs it, parses the PPM, and does PNG/scaling/auth/serving in Go.

## How it works

`drmModeGetFB2(plane)` → `drmPrimeHandleToFD` (dma-buf) →
`eglCreateImageKHR(EGL_LINUX_DMA_BUF_EXT, …, PLANEn_MODIFIER_*)` →
`glEGLImageTargetTexture2DOES` (`samplerExternalOES`) → composite into an RGBA
FBO → `glReadPixels` → PPM (P6) on stdout.

Passing the **format modifier** to EGL is what makes it driver-agnostic: the GPU
detiles linear (vc4 under X), X/Y-tiled (i915), AFBC, VC4 T/SAND, etc. — no
per-modifier CPU code (the limitation of `ffmpeg -f kmsgrab … hwmap` and of
hand-rolled detilers like `screenrec`). The external-texture sampler also does
YUV→RGB, so KMS video planes (NV12) come back as RGB.

By default it composites **all active planes** on the CRTC in zpos order
(primary + video overlays + hardware cursor) — a true "what's on screen". This is
the part single-plane `kmsgrab`/`scrot`/`grim` miss for the KMS A/V modes.

## Privilege

`drmModeGetFB2` returns the buffer handles only to the DRM master or a
`CAP_SYS_ADMIN` caller, so this runs **as root** (the agent already does). The GL
side uses the render node and needs no master. To run unprivileged:
`setcap cap_sys_admin+ep disp-kmsshot`.

## Usage

```sh
disp-kmsshot                 > shot.ppm   # composite the active CRTC
disp-kmsshot --primary       > shot.ppm   # primary plane only
disp-kmsshot -c 97           > shot.ppm   # a specific CRTC (multi-head)
disp-kmsshot -d                           # dump CRTC/plane/format/modifier to stderr
disp-kmsshot -D /dev/dri/card0 -r /dev/dri/renderD128 > shot.ppm
```

## Build

```sh
make            # needs: libdrm-dev libegl-dev libgles2-dev libgbm-dev + a C compiler
```

Deploy builds it on the node: `scripts/sideshow-deploy.sh kmsshot` (and `prereqs`
installs the `-dev` packages). Installed as `/usr/local/bin/disp-kmsshot`.

## Limits / notes

- **Multi-head:** auto-picks the first lit CRTC; use `-c <id>` for a specific
  output (find ids with `-d`).
- **Compositor-side effects:** none — it only reads scanout (non-disruptive
  alongside a running X/Wayland session).
- **Performance:** a single frame is cheap. Continuous capture (e.g. driving the
  live-view stream) through GLES2 readback may be heavy on the Pi 3B — fine for
  screenshots/thumbnails; benchmark before using it for the VNC stream.
