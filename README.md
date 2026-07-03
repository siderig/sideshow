# sideshow

**Fleet-ready display nodes for Linux.** Turn a Raspberry Pi or an x86 mini-PC wired to a
screen into a remotely-controllable "display node": a small Go agent owns the screen, runs one
**display mode** at a time (a web kiosk, a media/AirPlay receiver, an arbitrary GUI app, or a
console), and serves a key-protected control **webUI + JSON API on `:80`**.

> Think "supervisor that owns the screen." Exactly one thing holds the display at a time; you
> choose which, from any browser on the network.

Single static binary, no runtime dependencies of its own. Runs as a `Restart=always` systemd
service. Tested on Raspberry Pi 3B (arm64, X11) and x86 (amd64, Wayland).

## Install

On a fresh Debian / Raspberry Pi OS node:

```sh
curl -fsSL https://github.com/siderig/sideshow/releases/latest/download/install.sh | sudo sh
```

This installs the right-arch prebuilt binary (verified against the release `SHA256SUMS`), creates
a seat user, generates an auth key, starts the agent, and prints a **setup URL**. Open it from any
device on the same network to finish setup in the browser: pick a compositor (X11/matchbox or
Wayland/labwc), install the features you want, and go.

Prefer to build it yourself? See [Build from source](#build-from-source).

### Flashable images

Each release also ships **preconfigured, flashable appliance images** (built in CI) with the deps,
the agent, and `disp-kmsshot` already installed — flash, boot, and the node comes up in the
agent-owned display showing the setup wizard. On first boot the node **expands the root filesystem
to fill the card/disk** and **generates a unique auth key** (no shared secret is baked in).

- `sideshow-rpi-arm64-<ver>.img.xz` — Raspberry Pi (arm64), X11/matchbox
- `sideshow-debian-amd64-<ver>.img.xz` — x86-64 UEFI, Wayland/labwc

Flash with Raspberry Pi Imager, `dd`, or [balenaEtcher](https://etcher.balena.io/). The images are
built by [`.github/workflows/images.yml`](.github/workflows/images.yml) (Pi via `arm-runner-action`,
Debian via `debos`).

## Display modes

A node's screen is owned by exactly one surface at a time; switching modes tears down the current
owner and brings up the next.

| Mode | Owns the screen via | Examples |
|------|---------------------|----------|
| **web** | a Chromium kiosk (X11 or Wayland), driven over CDP | any URL; a built-in media-playlist player |
| **app** | an arbitrary GUI program under matchbox (X11) or labwc (Wayland) | a WebKitGTK app, a dashboard |
| **media / airplay** | mpv / uxplay — as an X11 client **or** direct DRM/KMS | screen-mirror from an Apple device; local video |
| **moonlight / steamlink** | the game-streaming clients (X11) | stream a PC/console to the screen |
| **console** | a foreground program on a dedicated TTY | a status TUI |
| **off** | nothing (blank) | |

## Control surface

The agent serves a webUI + a JSON API on `:80`, optionally gated by a shared key
(`/etc/sideshow/agent.key`). Highlights:

- **Now** — live "now showing" with state-aware controls, screen sleep/wake + a nightly schedule,
  TV control over HDMI-CEC, system stats + apt upgrades, on-screen messages, reboot/shutdown.
- **Library** — a media file manager (upload/organize, served with byte-range for video seeking),
  a mixed-media **playlist** player, and saved **custom modes** (named launchers).
- **Settings** — display rotate/zoom/layout, **hostname rename**, **Wi-Fi** management,
  boot splash, live screen view (VNC), memory cap, and the first-run **setup wizard**.
- Plus a **watchdog** (recover a wedged kiosk), a **DRM/KMS screenshot** that works under any mode,
  and **heartbeat** to a central aggregator.

The full REST API and the mode model are documented in **[docs/node-api.md](docs/node-api.md)**;
the project direction is in **[docs/ROADMAP.md](docs/ROADMAP.md)**.

## Build from source

Requires Go (see `agent/go.mod`). Pure Go, cross-compiles with no C toolchain.

```sh
cd agent
# build for this machine
go build -o sideshow-agent .
# or a release set (arm64 + amd64 + armhf + SHA256SUMS) into agent/dist/
sh scripts/build-release.sh
```

Install a local build on a node:

```sh
sudo SIDESHOW_BINARY=./sideshow-agent sh agent/scripts/install.sh
```

Deploy an update to an already-installed node (atomic binary swap + restart):

```sh
NODE=root@<node>.local ARCH=arm64 SEATUSER=<user> agent/scripts/sideshow-deploy.sh deploy
```

## Status

Working: the per-node agent (this repo) — broad and live-validated. Not yet built: the **central
web panel** that manages a fleet of nodes (each node can already heartbeat its status to an
aggregator). See the roadmap.

## License

[MIT](LICENSE).
