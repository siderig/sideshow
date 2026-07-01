# sideshow — node capability ideas

> A living brainstorm of **what a display node could do / load / be used for** — the *what*,
> not the *how*. [`ROADMAP.md`](ROADMAP.md) owns delivery/bulletproofing and
> [`node-api.md`](node-api.md) owns the mode + API contract; this file is the idea backlog that
> feeds them. Started 2026-06-29. Add freely; promote to ROADMAP/node-api when a thing gets built.

**Legend** — `✅` built (2026-06-29 round) · `◇` mostly reuses existing primitives
(mode/CDP/CEC/content/heartbeat) · `✦` needs new hardware or deps · `★` north-star / demo-worthy.

The node already has a strong base to build on: a one-owner-of-screen **supervisor**, a
**mode** abstraction (`web`/`app`/`media`/`airplay`/`off` live, plus receiver modes), **CDP** kiosk
control, **CEC**, **noVNC**, per-display **rotate/zoom/sleep + schedule**, **signage playlist**, a
**watchdog**, and a **heartbeat** to a future aggregator. Most leverage now is in *what the one
screen owner can be*.

## 1. New payload modes (what the screen shows)

Each is "just another mode" the supervisor can swap in:

- `✅◇` **Native media** — mpv playing a video loop, HLS/IPTV stream, or RTSP camera feed.
  Far lighter than Chromium for full-screen video. (compositor-hosted X11 client + a `display:kms`
  direct-KMS path.)
- `✅◇` **Slideshow** — cycle a list of images full-screen on an interval; reuses the kiosk + the
  existing content cycler.
- `✅◇` **Document** — render a PDF / slides full-screen via Chromium's built-in PDF viewer
  (menus, schedules, price lists), optional auto page-advance.
- `◇` **Dashboard presets** — first-class "show a Grafana / Home Assistant / Datadog board" mode
  with auto-login + auto-refresh baked in, vs. a raw URL.
- `✦` **Camera / NVR wall** — an RTSP/Frigate grid; a node above a desk becomes a security monitor.
- `◇` **Smart-mirror / ambient idle** — when nothing is scheduled, show clock + weather + calendar
  + transit + art instead of going black (Samsung Frame / Apple TV aerials).
- `◇` **Generative / screensaver art** — shader/procedural art, slow-TV, an aquarium; a good default.
- `✦` **Retro arcade / game-stream client** — RetroArch, or a Moonlight/Sunshine client streaming a
  game from a beefier box; node + gamepad = an arcade cabinet.
- `✦` **Conference-room endpoint** — USB cam + the screen = a Jitsi/Zoom Room. The one-owner model
  is exactly right for "the meeting takes the screen, then releases it."

## 2. Multi-display & fleet-as-one-canvas

- `✅◇` **Per-output content** — outputs are enumerated + addressable and rotate/sleep is now
  per-output; showing *different* content per output is modelled + persisted but only the primary
  renders today (secondary is a reported scaffold).
- `★✦` **Video wall / tiling** — synchronize N nodes to show *one* image/video spanning a wall,
  with bezel compensation + frame-sync. The fleet becomes a single canvas; each node a tile.
- `◇` **Display groups / zones** — push content to "all lobby screens" / "all portrait screens,"
  not one node at a time; mixed portrait/landscape per group.
- `◇` **Mirror mode** — one node is the source, others mirror it for overflow rooms.

## 3. Make it interactive (input, not just output)

- `✦` **Touchscreen** → wayfinding directories, self-order kiosks, room-booking panels ("tap to book
  the next 30 min").
- `◇` **QR-to-control** — show a QR on screen; scanning it lets a passerby cast a URL / pair / drive
  the kiosk. Zero-install remote.
- `◇` **Phone-as-cast-source** — "throw this to the lobby screen" from any device on the LAN.
- `✦` **Gamepad / barcode scanner / NFC** wired into modes (gaming, POS, "tap badge → show your
  dashboard").

## 4. It *is* a Linux box — use the rest of it

- `◇` **Cast / receive target** — be a Google Cast / Miracast receiver alongside AirPlay; the panel
  and ad-hoc users both push to it.
- `✦` **Multiroom audio (Snapcast)** — nodes have audio out; synchronized whole-building audio +
  paging/PA. "Announce to all screens" with TTS is a natural fleet superpower.
- `✦` **Sensor hub** — GPIO/USB ambient-light sensor → **auto-brightness**; temp/humidity/air-quality
  /PIR → feed the heartbeat. The node becomes an environmental probe that happens to have a screen.
- `◇` **Edge compute** — idle CPU runs a **local content cache/proxy** (keeps showing content when
  the network dies) or a **tiny vision model** for anonymized audience counting / dwell time.

## 5. Presence-aware behaviour

- `✦` **PIR / camera presence** → wake on approach, sleep when the room empties (beyond the nightly
  schedule); optionally log foot-traffic to the panel.
- `★✦` **Attention-driven content** — a vision model picks/changes content based on whether anyone is
  looking.

## 6. Fleet-level superpowers

- `★◇` **Emergency override / instant takeover** — one button blasts an evacuation message to every
  screen (and powers TVs on over CEC). The single-owner model makes a guaranteed takeover clean.
- `◇` **Day-parting & calendar-driven content** — URL A 9–5, ambient after hours, a lunch menu
  11–14; rules engine ("if weather alert, interrupt with a banner").
- `◇` **Webhook / event-triggered content** — CI fails → the NOC wall goes red; a sale starts → menu
  boards swap. The heartbeat already goes *up*; let commands come *down* the same channel.
- `◇` **Offline resilience** — node caches its playlist locally + has a fallback URL, so a dead
  network shows last-good content instead of an error page.

## 7. Environment control (extend CEC)

- `✦` Generalize the existing CEC TV control into **"the node controls the room"**: projectors via
  PJLink, smart bulbs / relays via GPIO/MQTT, lighting scenes synced to content ("presentation mode
  dims the lights"). The screen owner becomes a small AV controller.

---

## Built (2026-06-29 round)

All four below shipped and are live on disp + disp-deb-air; the per-feature detail is in
`node-api.md`:

1. ✅ **Native media mode** (`media`) — mpv as a **compositor-hosted X11 client** in the running Xorg
   session (no DRM-master handoff needed → sidesteps the direct-KMS path). Params: `url`/`path`,
   `loop`, `mute`. Plays local files, http(s), HLS, RTSP. Also gained `display:kms` (direct-KMS,
   `mpv --vo=drm`). Adds `mpv` to the deploy prereqs.
2. ✅ **Multi-display — per-output content** — enumerates all connected outputs (geometry + preferred
   mode), exposes them, and makes rotate/sleep output-addressable (default = primary,
   back-compatible). Per-output content is modelled + persisted, but only the primary output
   renders today (secondary content is a reported scaffold).
3. ✅ **Slideshow mode** (`/api/slideshow`) — a full-screen image slideshow that reuses the kiosk +
   the existing content cycler (interval, fit, transition); no new system binary.
4. ✅ **Document mode** (`/api/document`) — point the kiosk at a PDF/slide deck rendered full-screen
   in Chromium's PDF viewer, with optional auto page-advance.
