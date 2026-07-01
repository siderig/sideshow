# sideshow agent (v0 sketch)

The per-node supervisor: owns the screen, arbitrates **display modes**, supervises the active
mode's child with **restart-on-exit**, and serves a local **webUI + JSON API on `:80`**.
Single static Go binary. See [`../docs/node-api.md`](../docs/node-api.md) for the API and
[`../docs/ROADMAP.md`](../docs/ROADMAP.md) §9 for the decisions this implements.

## What works
### Modes (who owns the screen)
- `web` (`display:compositor`): launches a Chromium kiosk with localhost CDP debugging,
  **attaches** over the DevTools Protocol (`chromedp`, attach-not-launch), navigates, applies
  dark-mode, screenshots. On `display:kms` the web backend is **cog/WPE WebKit** on the raw
  framebuffer (no X/Wayland) — a far lighter kiosk for weak nodes.
- `web` on `display:wayland`: a **labwc Wayland primary** (Chromium-Wayland) on its own VT, CDP on
  a separate port — an alternative to the X kiosk (stops it; one Chromium). Default runs labwc as
  the **seat user** via seatd (GPU/GLES2); `-wayland-root` is the legacy root-via-libseat path.
- `app` on `display:compositor`/`display:wayland`: supervises an arbitrary GUI child (X11 client,
  or a labwc/Xwayland client under Wayland) as the seat user.
- `app` on `display:console`: a **TTY/console app (htop, a shell) on a dedicated VT**, layered over
  the compositor via `chvt` (the DRM-master handoff). Auto-recovers to the compositor VT if it
  crash-loops.
- **Streaming receivers**: `airplay` (uxplay) and `media` (mpv) run either as `display:compositor`
  X11 clients **or** via direct `display:kms` DRM/KMS on their own VT (`uxplay -vs kmssink`,
  `mpv --vo=drm`). `moonlight` (moonlight-qt) and `steamlink` are `display:compositor`-only X11
  clients. `miracast` is an experimental wireless-display sink, gated by `-allow-miracast`.
- `off`: stops everything (screen idle).
- Restart-on-exit with capped exponential backoff; gives up (mode `failed`) after a crash burst.
- Runs as **root** (binds `:80`, manages systemd/watchdog) and **drops child privileges** to the
  seat user (uid 1000) — except the legacy `-wayland-root` primary and root-on-KMS app modes.
- **Agent-owned Xorg** (`-start-x`): on X11 nodes the agent starts + supervises Xorg + a minimal
  window manager (**matchbox**) itself — no lightdm, no openbox — plus a cursor-hider. The X11
  modes attach to it.
- **Node control + telemetry** (node-api.md §2): `GET /api/stats` (cpu/mem/disk/load/temp/throttle/
  upgrades), `POST /api/upgrade` (niced, gated), `POST /api/theme` (light/dark kiosk),
  `POST /api/rotate` + `POST /api/zoom`, **display sleep/wake** (`POST /api/screen`, xrandr +
  CEC) with a nightly **schedule** (`POST /api/schedule`), TV over CEC (`GET/POST /api/cec`,
  incl. volume + bus monitoring), embedded **noVNC** live view (`GET /vnc`), and a boot-time
  EDID preferred-mode fix.
- **Power & lifecycle**: `POST /api/standby` (idle + screen off), `/api/reboot`, `/api/shutdown`
  (gated), `/api/restart`.
- **Reliability**: `GET /api/health` (auth-exempt liveness), `GET /api/logs` (ring buffer), and a
  network/render **watchdog** that reloads/restarts (and optionally reboots) an unattended kiosk.
- **Signage**: URL **playlist** rotation (`/api/playlist`), periodic **reload** (`/api/reload`),
  on-screen **message** overlay (`/api/message`).
- **Fleet**: optional **heartbeat** to a central aggregator (`-heartbeat-url`) + node identity
  (`-node-label`/`-node-group`).

Also live: uploadable **media library + playlists**, on-screen **message** styling, a **Plymouth
boot splash** (`/api/plymouth`), a universal DRM/KMS **screenshot** (any mode below the
compositor), a kiosk Chromium **managed policy** (kills the translate bar, force-installs a
cookie-dialog extension), and system-wide light/dark **color-scheme** theming for GUI apps. Modes
that need a tool the node lacks are guarded (HTTP 400) so a missing binary can't blank the screen.

## Build & run
```sh
go build .                                  # host build
GOOS=linux GOARCH=arm64 go build .          # cross-compile for a Pi

# local dry-run (no display children): off mode, high port, no privilege drop
./agent -addr 127.0.0.1:8080 -start-mode off -no-priv-drop

# on a node, as root (binds :80, runs Chromium as the seat user on DISPLAY :0):
DISPLAY=:0 ./agent -addr :80 -seat-user sideshow -url https://example.org
```

## Deploy to disp
`scripts/sideshow-deploy.sh {build|push|stop-legacy|run|deploy|migrate|prereqs|install|memcap|plymouth|moonlight|steamlink|kmsshot|xown}`
— see the script header. `deploy` = safe in-place binary swap + restart of an already-installed
agent (atomic; when the node is idle). `prereqs`/`install` provision + wire the systemd service;
`xown` performs the lightdm→agent-owned-Xorg cutover. disp reboots back to the agent-owned kiosk
on its own; the test nodes have no rollback path (re-flash or re-run `install`).

## Key flags
`-addr` listen addr · `-seat-user` child user · `-display` X DISPLAY · `-url` initial web URL ·
`-start-mode web|wayland|off` · `-cdp-port` Chromium debugging port · `-no-priv-drop` (run children
as self) · `-start-x` (agent-owned Xorg + matchbox) · `-fix-mode` (force EDID-preferred mode at
boot) · `-cec-device /dev/cec0` · `-vnc-port 5900` · `-wayland-launcher` / `-wayland-vt` /
`-wayland-cdp-port` / `-wayland-root` (the labwc primary) · `-auth-key-file /etc/sideshow/agent.key`
(gate the surface) · `-allow-miracast` · `-heartbeat-url` / `-node-label` / `-node-group` (fleet).

## Security note
`-addr :80` binds all interfaces for LAN webUI control, and `app` mode = arbitrary command
execution as the seat user, by design. An **optional key** gates the whole surface when
`/etc/sideshow/agent.key` (or `-auth-key`) is set — cookie (`sideshow_key`) or bearer, with
`GET /`, `POST /api/auth`, and `GET /api/health` exempt. With auth off, keep it on a trusted
LAN / Tailscale, or bind `127.0.0.1` for local-only access.
