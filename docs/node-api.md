# sideshow node API + mode abstraction

> The contract the per-node Go supervisor serves on **`:80`**, and the **mode** model it
> arbitrates. The local webUI, a future self-hosted aggregator, and any future SaaS all speak
> this same API (ROADMAP §8 — "define it once"). Written 2026-06-27 alongside the first
> `agent/` sketch; **v0** — minimal but real. Stable shape, not stable internals.

This document is the spec; [`agent/`](../agent/) is the reference implementation. Where they
disagree today, the code wins and this doc is the bug.

## 1. The mode abstraction

A node's screen is owned by **exactly one mode at a time**. The supervisor is the arbiter
that tears down the current owner and brings up the next (ROADMAP §1). A mode is:

```jsonc
{
  "type":    "web" | "media" | "airplay" | "moonlight" | "steamlink" | "miracast" | "app" | "off",
  "params":  { /* type-specific, see below */ },
  "display": "compositor" | "wayland" | "console" | "kms"   // how it owns the screen
}
```

- **`type`** — what surface runs:
  | type | owns screen via | payload (disp today) | params |
  |------|-----------------|----------------------|--------|
  | `web` | X11/Wayland (a GUI client), **or** DRM/KMS framebuffer (`display:kms`) | Chromium kiosk driven over CDP, **or** `cog` (WPE WebKit) on the framebuffer with no X — a light kiosk for weak nodes | `url` (required), `dark` (bool, default true; CDP backends only) |
  | `media` | GUI (X11 client) | `mpv --fullscreen` | exactly one of `url`/`path`; `loop` (bool, default true), `mute` (bool, default false) |
  | `airplay` | X11 client *or* KMS framebuffer | `uxplay -n <name>` (autovideosink, or `-vs kmssink` for `display:kms`) | `audio` (bool, default false), `name` (string, default node name) |
  | `moonlight` | GUI (X11 client) | `moonlight-qt stream <host> <app>` (or the pairing GUI) | `host` (string; empty → pairing GUI), `app` (string, default `Desktop`) |
  | `steamlink` | GUI (X11 client) | operator launcher (`-steamlink-cmd`, default `steamlink`) | — |
  | `miracast` | GUI (X11 client) | operator launcher (`-miracast-cmd`) | — (**experimental**, gated by `-allow-miracast`) |
  | `app` | GUI / CLI / TTY | arbitrary argv | `argv` (array, required), `env` (map) |
  | `off` | nothing (screen idle/blank) | — | — |

  **Streaming receivers.** `airplay` (uxplay) runs either as an **X11 client** (`display:compositor`,
  autovideosink) or via **direct DRM/KMS** (`display:kms`, `uxplay -vs kmssink`) — the latter is the
  performant, **backend-agnostic** path (no X/Wayland; the right choice on a Pi, and the *only* way
  to run a receiver on a Wayland-only node). It's discovered over the network as the node's name and
  needs `avahi-daemon`. `moonlight` (binary **`moonlight-qt`**) is an X11 client and needs a
  Sunshine/GameStream host paired once out of band (`moonlight-qt pair <host>`); install it with
  `sideshow-deploy.sh moonlight` (the official Cloudsmith apt repo — validated: `moonlight-qt 6.1.0` for
  `debian/trixie/arm64`). `steamlink` is an X11 client whose launcher is operator-set
  (`-steamlink-cmd`); packaging is awkward — the apt `steamlink` package is **Raspberry-Pi-OS-only**
  and the Flathub Flatpak (`com.valvesoftware.SteamLink`) is **x86_64-only**, so there's no clean
  arm64-Debian path (`sideshow-deploy.sh steamlink` tries apt, else prints the Flatpak route). The
  X11-client receivers (`moonlight`/`steamlink`/`miracast`) need an Xorg session, so they don't run
  on a Wayland-only node — only `airplay`/`media` reach those via `display:kms`. `miracast` is an X11 client, **off by
  default** — Wi-Fi P2P can knock a single-adapter node off its own uplink — and only runs with
  `-allow-miracast` (else 400). **Live-validated:** `airplay`+`display:kms` on disp (X11, Pi 3B) and
  disp-deb-air (Wayland, x86) — uxplay+kmssink runs as root on the mode VT, holding `/dev/dri/cardN`,
  while the X/Wayland primary sits VT-suspended.

  **Low-end web kiosk options.** Two levers for nodes too weak for full Chromium (the Pi 3B —
  ~730 MB RAM, GLES2-only V3D → software-rendered Chromium — can OOM/thrash on a heavy page):
  (1) **`-chromium-low-mem`** keeps the Chromium kiosk but applies a memory-reduction flag profile
  (single renderer, `--process-per-site`, no GPU process, capped V8 heap + disk cache) — the middle
  ground; (2) **`web` + `display:kms`** — a **`cog` (WPE WebKit)** kiosk rendering directly on
  DRM/KMS with **no X and no compositor**, a real JS-capable engine at a fraction of Chromium's
  footprint (~half the RAM, roughly a third the CPU on the Pi 3B). cog runs directly (it shows the
  launch URL itself) and is controlled in place over its **built-in D-Bus interface** (`cogctl`):
  **`POST /api/url` re-navigates with no restart** (`cogctl open`, ~0.2 s) and **`POST /api/reload`**
  (`cogctl reload`). The agent runs a private session bus for cog at `unix:path=/run/sideshow/cog-bus`
  (not the fragile root-login `/run/user/0/bus`) so control works on a headless boot, and switches to
  the mode VT **before** launching so cog can take DRM master. cog has **no CDP**, so these return a
  clean error: `/api/theme` and `/api/zoom` → **501** (no `prefers-color-scheme` emulation; runtime
  zoom unavailable — `cog --scale` sets a fixed boot zoom), `/api/screenshot` → **503** (no CDP, and
  a raw KMS grab is DRM-master-locked + vc4-tiled). For pages needing theme/zoom/screenshot, use the
  Chromium/CDP kiosk. Flags: `-cog-cmd`, `-cogctl-cmd`, `-cog-video-mode` (e.g. `1920x1080`). Install
  with `apt install cog`. **Live-validated on disp** (switch + in-place navigate + reload).
  > *A WebDriver-managed cog (for screenshot/zoom/execute-JS) was prototyped but dropped — cog's
  > automation is unreliable on the DRM backend ("automation is not allowed in the context" → hangs);
  > cog is EOL, so the dependable D-Bus path above is the chosen design. **Dark mode** isn't reachable
  > on WPE 2.48.3 at all (no public API; the override exists only behind WebKit's private
  > remote-inspector, verified unreachable on disp). It's a real upstream feature via the new
  > WPEPlatform `WPE_SETTINGS_DARK_MODE` (fixed in WPE 2.49.2) but needs a newer-than-Debian WPE +
  > a WPEPlatform launcher — see [ROADMAP §9](ROADMAP.md#9-decisions) (2026-06-30). Use the
  > Chromium/CDP kiosk for dark-needing pages.*

- **`display`** — how the mode owns the screen, and the DRM-master handoff (ROADMAP §1):
  - **`compositor`** — runs as an **X11 client** drawn into the **agent-owned Xorg** session on
    the X VT (default tty7). The agent starts + supervises Xorg + a minimal WM (**matchbox**)
    itself under `-start-x` (no lightdm / openbox), and drops a cursor-hider in as a third seat-user
    X client. Remote-viewable (x11vnc, see `/vnc`); slight compositing cost. **The default.** This
    is a **persistent base surface** — it stays alive (VT-suspended) behind a foreground mode, so
    returning to it is instant.
  - **`wayland`** — a **`labwc` Wayland primary** (Chromium as a Wayland client) on its own VT
    (default tty8). By default labwc runs as the **seat user via `seatd`**, which enables the
    wlroots **GLES2 GPU renderer** — the same GPU-accelerated path RPi OS 13's default labwc
    desktop uses (needs `seatd` + the user in its group; see `sideshow-deploy.sh prereqs`). The legacy
    **`-wayland-root`** flag runs it as root via libseat `builtin` + pixman (software) as a
    fallback. It is an *alternative* primary, not a layer: entering it **stops the X kiosk's
    Chromium** (only one Chromium runs) and `chvt`s to the Wayland VT so Xorg drops DRM master and
    labwc acquires it; returning to `compositor` `chvt`s back and cold-starts the X Chromium. CDP
    runs on a **separate port** (default 9223). `web` only. **Note:** impractical on a Pi 3B
    (V3D is GLES2-only → Chromium software-renders; 1 GB RAM thrashes) — for a Pi 4/5+. The
    seatd/user GPU path is **validated on GLES3 hardware** (`disp-deb-air`, Intel HD 5000);
    only X↔Wayland seat coexistence on an X-running node is still open. See inventory §9.
  - **`console`** — a **TTY/console app** (htop, a shell, any curses program) on a **dedicated
    VT** (default tty9), layered over the X *or* Wayland base via **VT switching** (`chvt`). logind
    performs the DRM-master handoff on the VT switch; a foreground crash returns to whichever
    compositor owns the screen. Implemented + live-tested (htop) on disp. *Works over the Wayland
    primary too, as long as the mode VT differs from the Wayland VT — they collide only if
    `-mode-vt == -wayland-vt` (then 409 with a fix-it hint). The defaults (mode tty9, Wayland tty8)
    don't collide.*
  - **`kms`** — owns DRM/KMS directly, no compositor (`mpv --vo=drm`, `uxplay -vs kmssink`). Same VT
    layering as `console`: the supervisor `chvt`s to the mode VT, the X/Wayland primary drops DRM
    master as its VT deactivates, and the child — **run as root** (`Mode.runsAsRoot`), since a
    priv-dropped process not in a logind session on that VT can't `SET_MASTER` — acquires it.
    **Implemented for `media`/`airplay`/`app`** and live-validated on both an X11 (disp) and a
    Wayland (disp-deb-air) node. The performant, backend-agnostic path. Gotcha: KMS children run as
    root, so root's GStreamer registry must list the bad plugins (kmssink) — `sideshow-deploy.sh
    prereqs` clears `/root/.cache/gstreamer-1.0` so it rebuilds.

  The supervisor runs **one primary surface** (X11 `compositor` *or* `wayland`) and at most one
  foreground VT mode (`console`/`kms`). Exactly one VT is active, so exactly one surface is
  visible. A foreground crash returns the screen to the primary VT automatically; a failed primary
  switch that tore down a working base restores the X web kiosk rather than blanking the screen.

  **Kiosk input lockdown (`-lock-input`).** Off by default (it changes local input handling —
  enable per node in the unit). When set, the agent strips the compositor's window-management
  shortcuts so a local keyboard/mouse can't switch away from, close, or pop a menu over the kiosk:
  under **labwc** it writes an agent-owned config dir (`/var/lib/sideshow/labwc/rc.xml`) whose inert
  binds suppress *all* of labwc's defaults (Alt+Tab, Super-tiling, Alt+F4, the right-click root
  menu) and points labwc at it via `-C` (the launcher honours `SIDESHOW_LABWC_CONFIG`); under
  **matchbox** it writes an empty `~/.matchbox/kbdconfig`, which overrides matchbox's built-in
  shortcuts.

  It also closes **VT switching** (`Ctrl+Alt+Fn`), by the means each stack allows:
  - **X11** — enforces Xorg `-novtswitch` (the X server refuses the switch).
  - **Wayland / non-X** — wlroots owns VT switching below labwc and exposes no knob, so the agent
    closes it at the **seat** instead: it writes a logind override (`NAutoVTs=0`/`ReserveVT=0`) and
    masks the console gettys (`getty@tty1..6`), so a switch lands on a dead console with no login or
    shell. Applied automatically at boot (root only) and reconciled with the flag — the override
    drop-in doubles as the "applied" marker (written before masking, removed after unmasking), so
    turning `-lock-input` off unmasks `getty@tty1..6` and drops the override, and an interrupted
    apply/revert is always finished on the next boot (the masks can never get stuck on). Trade-off:
    the off path unmasks the whole range, re-enabling any getty you masked by hand.

  **No local input (`-no-local-input`).** A stronger, orthogonal stance for a *pure display*: whereas
  `-lock-input` leaves the page interactive (it only removes the compositor's escape shortcuts), this
  makes the compositor ignore **all local keyboard/mouse/touch** devices, so nothing — not even a
  click or `Ctrl+N` in Chromium — reaches the kiosk. The agent installs a libinput udev rule
  (`/etc/udev/rules.d/99-sideshow-noinput.rules`, `LIBINPUT_IGNORE_DEVICE`) and, on an `-start-x`
  node, an `xorg.conf.d` `Option "Ignore" "on"` snippet (the libinput property alone isn't honored
  under X). **Remote control (VNC / the panel) still works** — it injects through wlroots
  *virtual*-input, which the rules don't touch. Power/lid/sleep buttons are left alone. Reconciled
  with the flag (off removes the files); takes effect when the compositor next (re)starts, i.e. the
  deploy restart. Root only.

**Scope:** implemented — `web`/`app`/`media`/`airplay`/`moonlight`/`steamlink`/`miracast` on
`display: compositor`, `web` on `display: wayland`, `app` on `display: console`, and
`media`/`airplay`/`app` on `display: kms`. `web` URLs must be `http(s)://`. `media`, `airplay`,
`moonlight`, `steamlink`, and `miracast` on `compositor` are **X11 clients** (need an Xorg session);
`media` and `airplay` on `kms` are **direct-DRM** (no X/Wayland — the path for Wayland-only/headless
nodes). `moonlight`/`steamlink`/`miracast` are X11-only (the Qt/GTK clients aren't direct-KMS).
`miracast` also needs `-allow-miracast` (else 400). `web`+`kms` is rejected (web is a compositor
client). Validation happens **before** the current mode is torn down, so a bad request never blanks
the screen.

### Mode lifecycle

```
        POST /api/mode {type, params, display}
                     │
   ┌─────────────────▼─────────────────┐
   │ 1. validate request               │
   │ 2. stop current mode (SIGTERM →   │  ← supervisor releases the screen
   │    grace → SIGKILL the child grp) │
   │ 3. (if kms↔compositor) hand off   │  ← v0: same display path only
   │    DRM master via seat            │
   │ 4. start new mode child           │  ← supervised: restart-on-exit
   │ 5. post-start hook (web: attach   │
   │    CDP, navigate, inject dark)    │
   └─────────────────┬─────────────────┘
                     ▼
            mode = running / failed
```

The **supervised child** is restarted automatically if it exits **while it is the active
mode** (the "no-respawn gap" fix, ROADMAP §3). A deliberate mode switch stops the old child
*without* triggering a respawn. Restart uses capped backoff; repeated rapid crashes mark the
mode `failed` (and would, in the bulletproof tier, fall through to the watchdog).

## 2. HTTP API (`:80`)

JSON over HTTP. Still intended to be **LAN / localhost / Tailscale-bound, never exposed to the
internet** (ROADMAP §4). All responses `application/json` except `GET /` (HTML) and
`GET /api/screenshot` (PNG). Times are RFC-3339 / Unix where noted.

**Optional auth (key).** If a key is configured (`-auth-key-file`, default `/etc/sideshow/agent.key`;
empty/missing → no auth, the LAN-only default), the whole surface is gated: every `/api/*`,
`/api/screenshot`, `/vnc`, and `/vnc/ws` requires the key, via the `sideshow_key` cookie
(set by `POST /api/auth {"key":"…"}`, sent automatically by the browser on fetch/img/ws) **or** an
`Authorization: Bearer <key>` header (for API clients) **or** an `X-Sideshow-Key: <key>` header
(convenience alias). `POST /api/logout` clears the cookie. It is **not TLS** — on plain HTTP the key
is sniffable on the wire; it stops casual access + accidental control on a trusted LAN. **When auth
is on, VNC also allows input control** (otherwise the live view is forced view-only).

**Exempt from the key:**
- `GET /` (the login/UI shell — no secrets), `POST /api/auth` (the login endpoint),
  `GET /api/health` (liveness for an external monitor), and `/favicon.ico`.
- **The first-run setup surface** — `GET /setup`, `GET /api/setup`, `POST /api/setup/install`,
  `POST /api/setup/finish` — but **only while the node is not yet provisioned** (`!SetupComplete`).
  This is the bootstrap window on a fresh node: no key has been entered yet and the LAN is the trust
  boundary (same model as the plain-HTTP auth). Once the wizard finishes, `SetupComplete` flips and
  this surface is gated like everything else, closing the pre-auth hole.
- **Loopback-only kiosk fetches** — the locally-running CDP-driven kiosk has no auth cookie, so a
  handful of viewer surfaces are exempt **only for a loopback client** (never widening LAN access):
  `/docfs/…`, `GET`/`HEAD /media/…`, `GET /show`, and `GET /api/playlist-media`.

### `GET /api/status` → current mode + health

```jsonc
{
  "node":   "disp",                 // the live hostname
  "label":  "",                     // -node-label, human label for the fleet view (omitted if empty)
  "group":  "",                     // -node-group, group/site (omitted if empty)
  "time":   "2026-06-27T04:30:00Z",
  "uptime_s": 1234,                 // agent uptime
  "mode": {
    "type": "web",
    "params": { "url": "https://example.org", "dark": true },
    "display": "compositor",
    "state": "running",            // starting | running | failed | stopped
    "since": "2026-06-27T04:20:00Z",
    "restarts": 0,                  // supervised restarts since this mode started
    "last_error": "",              // last child error / exit reason, if any
    "background": ""               // a compositor mode suspended behind a foreground VT mode
  },
  "child": { "pid": 1270, "running": true },
  "cdp":   { "attached": true, "url": "http://127.0.0.1:9222" },  // web mode only
  "health": "ok",                  // ok | degraded | down
  "auth":  true                    // the control surface is key-protected
}
```

### `GET /api/snapshot` → the aggregated control-surface document

The single document the webUI polls each tick so the whole UI renders + refreshes from **one**
request instead of a fan-out of per-feature polls (heavy on a weak node). It folds in the cheap,
cached `Info()` of every manager. Fields:

```jsonc
{
  "node": "disp", "label": "", "group": "",
  "time": "…", "uptime_s": 1234, "auth": true, "health": "ok",
  "mode":  { … },            // same shape as status.mode
  "child": { "pid": 1270, "running": true },
  "cdp":   { "attached": true, "url": "…" },

  "stats":     { … },        // SysStats (same as GET /api/stats)
  "display":   { … },        // DisplayInfo (rotation/zoom/asleep/schedule/layout/outputs)
  "content":   { "reload_min": 0 }, // periodic page-reload interval (minutes)
  "document":  { … },        // document viewer config
  "cec":       { … },        // CECInfo (same as GET /api/cec)
  "vnc":       { … },        // VNCStatus (same as GET /api/vnc)
  "memory":    { … },
  "plymouth":  { … },        // PlymouthInfo
  "state":     { … },        // StateInfo (active mode + setup_complete + history + custom_modes)
  "playlist":  { "count": 3, "show_url": "http://127.0.0.1/show" }, // cheap summary; items on demand
  "miracast":  { … },        // MiracastInfo
  "net":       { … },        // NetInfo (hostname/suggested/can_rename/protected/comitup/wifi.supported/link)
  "caps":      { "shutdown": true, "miracast": false }  // agent-level gates for showing/hiding controls
}
```

The heavier multi-output view (`GET /api/outputs`, an xrandr fork) stays a separate, slower poll and
is **not** folded into the snapshot.

### `GET /api/state` → persisted active mode + history + custom modes

The thin layer above the (stateless-on-restart) supervisor that remembers the operator's last intent
and replays it at boot. Also carried in the snapshot's `state` block.

```jsonc
{
  "active": { "type": "web", "display": "compositor", "params": { "url": "…" } }, // the mode restored at boot (omitted if none)
  "setup_complete": true,          // the first-run wizard has finished
  "history": [                     // things this node has shown, most-recent first (≤ 50, deduped by label)
    { "id": "…", "at": "2026-07-01T…Z", "type": "web", "label": "web:https://…", "mode": { … } }
  ],
  "custom_modes": [ … ]            // saved named launchers (see custom modes)
}
```

### `GET /api/history` · `POST /api/history` → the shown-content history

- `GET /api/history` → `{ "history": [HistoryItem…] }` — the same list as `state.history`.
- `POST /api/history {"delete":"<id>"}` removes one entry (`404` if the id is unknown);
  `POST /api/history {"clear":true}` empties it. Missing both → `400`. Returns the updated
  `{ "history": [...] }`. The active mode is untouched.

### `POST /api/mode` → switch mode

Request body is a mode object (`display` optional → defaults to `compositor`):

```jsonc
// switch to a different URL (web mode)
{ "type": "web", "params": { "url": "https://example.org" } }

// run an arbitrary GUI app, compositor-hosted (display defaults to compositor)
{ "type": "app", "params": { "argv": ["xclock"] } }

// run a CLI/TTY app on a dedicated VT (kiosk stays alive behind it; chvt handoff)
{ "type": "app", "params": { "argv": ["htop"] }, "display": "console" }

// native media: mpv as an X11 client on the compositor (fullscreen video/stream)
{ "type": "media", "params": { "url": "https://example.org/clip.mp4", "loop": true } }

// streaming receivers
{ "type": "airplay", "display": "kms", "params": { "audio": false } } // uxplay -vs kmssink (direct DRM; works on X/Wayland/headless)
{ "type": "airplay", "params": { "audio": false } }          // uxplay X11 client (needs Xorg)
{ "type": "moonlight", "params": { "host": "10.0.0.5" } }    // moonlight-qt X11 client; empty host → pairing GUI
{ "type": "steamlink" }                                       // steamlink X11 client (-steamlink-cmd launcher)
{ "type": "miracast" }                                        // X11 client; needs -allow-miracast (else 400)

// direct-DRM media (no compositor): mpv --vo=drm on its own VT
{ "type": "media", "display": "kms", "params": { "url": "https://example.org/clip.mp4" } }

// stop the current mode; screen goes idle
{ "type": "off" }

// NOTE: moonlight/miracast are X11-only (compositor). airplay/media also accept display:"kms"
//       (direct DRM, the only receiver path on a Wayland-only node). web+kms is rejected.
```

Response: the same shape as `status.mode` (the new active mode), `200` on success. Errors:
`400` invalid/unimplemented mode/params (validated **before** the current mode is touched, so a
bad request never blanks the screen), `409` switch already in progress, `500` child failed to start.
The call **blocks until the new child is started** (and, for `web`, CDP is attached and the
first navigation issued) or fails — so the caller knows the screen actually changed.

Convenience: `POST /api/url {"url":"…","dark":true}` is sugar for `POST /api/mode {type:web,
params:{url}}` (re-navigates in place if already in web mode — no Chromium restart; `dark` defaults
true). It **inherits the current web mode's `display`** (so a Wayland node stays Wayland) and clears
the active document owner so a timer can't re-assert old content over the new URL.
`400` on a missing/non-`http(s)` URL. And `POST /api/media {"url"|"path":"…","loop":true,"mute":false}`
is sugar for `POST /api/mode {type:media, display:compositor, params:{…}}` — exactly one of
`url`/`path` (`loop` default true, `mute` default false); `400` on missing/both, a bad URL scheme, or
a non-absolute `path`.

### Custom modes — saved named launchers

A **custom mode** is an operator-defined, named launcher for an arbitrary program, run as an `app`
mode with a chosen display surface. Persisted in `state.json` (also surfaced in `state.custom_modes`).

- `GET /api/custom` → `{ "custom_modes": [ { id, name, command, args[], display, env{} } ] }`.
- `POST /api/custom {"id"?,"name","command","args":[…],"display","env":{…}}` — create (no `id`) or
  update (existing `id`) a launcher; returns the saved `CustomMode` (with its assigned `id`). `name`
  and `command` are required (`400` otherwise). `display` defaults to `compositor` (one of
  `compositor|console|kms|wayland`). `env` is only honored on `compositor`/`console` modes — the
  framebuffer (`kms`) and Wayland launchers build a fixed env, so `env` with any other display is
  rejected (`400`) rather than silently ignored.
- `POST /api/custom/delete {"id":"…"}` → `{ "ok": true }`; `404` if the id is unknown.
- `POST /api/custom/launch {"id":"…"}` → switches to the saved mode (built as `{type:"app", display,
  params:{argv:[command, …args], env}}`) and returns the new `status.mode`. The app-mode validation
  and the display=kms root gate are enforced by the switch itself; `404` if the id is unknown.

### Actions — saved, slugged, fireable launchers

An **action** generalizes a custom mode: a named launcher wrapping ANY mode (not just `app`), with a
url-safe **slug** and an ordering **index**. Fire one by slug from a Stream Deck or a script.
Persisted in `actions.json`; also surfaced in `snapshot.actions` (+ `snapshot.current_action`, the
slug of the action matching what is on screen). Existing `custom_modes` are migrated into actions on
first run.

- `GET /api/actions` → `{ "actions": [ { id, slug, index, name, mode:{type,params,display} } ] }`
  (sorted by index).
- `POST /api/actions {"id"?,"slug"?,"name","mode":{…}}` — create (no `id`) or update (existing `id`);
  returns the saved `Action`. `name` is required; `mode` is validated by the same rules as
  `POST /api/mode`, so an un-fireable action can't be saved (`400`). The slug is normalized to
  `[a-z0-9-]` (derived from the name when omitted); an operator-typed slug that collides with another
  action is rejected (`400`), a derived collision is auto-suffixed (`news` → `news-2`).
- `POST /api/actions/delete {"slug"|"id":"…"}` → the refreshed list; `404` if unknown.
- `POST /api/actions/reorder {"order":["slug-a","slug-b",…]}` → the reordered list (listed slugs take
  positions `0…n-1`; any omitted keep their relative order after them).
- `POST /api/action/<slug>` → fires the action (`sup.Switch(action.mode)` + records it active) and
  returns the new `status.mode`. Send the node key as `X-Sideshow-Key` (or `Authorization: Bearer`).
  Inherits the switch's errors as-is: `400` (bad mode), `404` (unknown slug), `409` (a switch is in
  progress), `500` (launch failed).

> `/api/custom*` remains as a compatibility shim for one release; new integrations use `/api/actions*`.

### `GET /api/screenshot` → PNG thumbnail of the current screen

- **web mode:** CDP `Page.captureScreenshot` (decided thumbnail source, ROADMAP §4).
- **other compositor modes (media/app):** `scrot` (X11) of the running Xorg framebuffer — so a
  media (mpv) or app screen still produces a live thumbnail. (`grim` is the Wayland equivalent.)
- **kms / direct modes:** DRM framebuffer grab (best-effort) — no live capture while a
  direct-KMS client holds the screen; may return `503`.

Query: `?w=480` to cap width (server downscales). `503` if no capturer is available for the
current mode.

### `GET /` → webUI

A single self-contained HTML page (no build step, embedded in the binary): shows current
mode + health, a live-ish screenshot, and controls to switch mode / set the web URL. Polls
`GET /api/status` and `GET /api/screenshot`. This is the node's standalone control surface
(ROADMAP §4) — the aggregator, when it exists, drives the same `/api/*` endpoints.

### `POST /api/theme` → kiosk light/dark

`{"dark": true|false}` — re-applies `prefers-color-scheme` to the live web kiosk **in place**
(CDP emulation, no navigate/restart) and persists it on the active web mode. `400` outside web
mode, `409` while a foreground mode is on screen, `503` if CDP isn't attached.

### `POST /api/rotate` → rotate the display

`{"degrees": 0|90|180|270}` — rotates a connected X output via `xrandr --rotate`
(0=normal, 90=right/CW, 180=inverted, 270=left/CCW). Persisted (state file, per output) and
re-applied at boot. `400` invalid angle, `409` under the Wayland kiosk (X11-only for now) or a
foreground mode, `500` if `xrandr` fails. Returns the `DisplayInfo` (see `/api/status`).
**Multi-display:** an optional `"output":"HDMI-2"` targets that output; omitting it (the
back-compat default) rotates the **primary** output.

### `POST /api/zoom` → kiosk page zoom

`{"percent": 125}` (or `{"factor": 1.25}`) — sets the kiosk page zoom via CDP CSS zoom, applied
in place and re-applied on every navigation; persisted and restored at boot. Factor clamped
0.25–5.0. Same web-mode gating as `/api/theme`. Returns the same shape as `/api/rotate`.

### Multi-display (`/api/outputs`, `/api/layout`, `/api/outputs/content`)

A node may drive **more than one connected output**. Single-output nodes are unchanged — the
top-level `DisplayInfo.rotation`/`asleep` keep meaning the **primary** output, the webUI hides
the Displays panel until ≥2 outputs are seen, and `/api/rotate`+`/api/screen` default to the
primary. Output state (per-output rotation, layout) is persisted in the state file and re-applied
at boot. All of this is **xrandr (X11) only** — `409` under the Wayland kiosk (`wlr-randr` is the
follow-up).

- `GET /api/outputs` → `[{ name, primary, rotation, asleep, geometry, content, rendered }]` for every
  connected output (`geometry` is the `WxH+X+Y` placement; `content` is the assigned
  `OutputContent`, default `{type:"off"}`; `rendered` is `true` only when that `content` is actually
  on screen — today only the primary, so a secondary output reports its assignment with
  `rendered:false`). Never `500`s — falls back to the persisted/primary view when xrandr is
  unavailable (host / Wayland).
- `POST /api/layout {"mode":"single|mirror|extend","primary":"HDMI-1"}` — arranges the outputs:
  **single** (primary on, others `--off`), **mirror** (others `--same-as` the primary), **extend**
  (others chained `--right-of`). `primary` is optional (defaults to the xrandr primary). Persisted
  per-output rotations are re-applied after the layout change (xrandr `--auto` resets them).
  Returns the `DisplayInfo`. `400` on an unknown mode or <2 outputs for mirror/extend; `409` under
  Wayland or a foreground mode.
- `POST /api/outputs/content {"output":"HDMI-2","type":"web|media|off|mirror","url":"…","path":"…"}`
  — assigns content to an output (`output` optional → primary). **Only the primary output's content
  renders today** (routed through the running kiosk / a media switch); a **secondary** output's
  assignment is **persisted + reported** by `/api/outputs` but **not yet rendered** (positioned
  Chromium/mpv with its own CDP port is the bounded follow-up — see Later). Returns `[]OutputInfo`.
  `400` on a bad type or missing `url`/`path`.

### `GET /api/stats` → node health metrics

```jsonc
{
  "uptime_s": 27019, "load": [1.38, 0.33, 0.11], "cpu_count": 4, "cpu_percent": 2.1,
  "mem":  { "total_mb": 730, "used_mb": 376, "free_mb": 354, "percent": 51.5 },
  "disk": { "total_gb": 14.4, "used_gb": 6.7, "free_gb": 7, "percent": 46.8 },
  "temp_c": 56.4, "throttled": "0x0", "undervolt": false,
  "model": "Raspberry Pi 3 Model B Rev 1.2", "resolution": "1920x1080",
  "upgrades": { "supported": true, "available": 0, "packages": [...], "checked_at": "…",
                "checking": false, "upgrading": false, "last_result": "ok", "log_tail": "…" }
}
```
Cheap fields are read per request from `/proc`+`/sys`; `cpu_percent`/`throttled`/`resolution` are
sampled on background tickers; the upgrade count comes from a cached `apt-get -s upgrade`. A
`"display"` sub-object carries the `DisplayInfo`: `{ rotation, zoom_percent, asleep, schedule,
layout, outputs[] }` — `rotation`/`asleep` are the **primary** output (back-compat); `layout`
(`single|mirror|extend`) and `outputs[]` (the same shape as `GET /api/outputs`) are the additive
multi-display view, empty/omitted on a single-output node.

### `POST /api/upgrade` → run / re-check Debian upgrades

`{}` or `{"action":"upgrade"}` runs a conservative `apt-get upgrade` (never dist-upgrade), under
`nice`+`ionice`, async — `202` with the current apt status; the webUI polls `/api/stats`.
`{"action":"check"}` refreshes the count. Gated: `409` during the cold-boot window (kernel
uptime < 180s) or while a mode switch / foreground mode is active; `409` if already running;
`501` on a non-apt node.

### `POST /api/screen` → sleep / wake the attached display

`{"on": false}` sleeps the screen, `{"on": true}` wakes it. It does **two** things so it works on
any display: (1) disables/enables the **output** — `xrandr --output <out> --off|--auto` under X
(the reliable lever where the DPMS extension is absent — disp's Xorg has none — re-applying the
persisted rotation on wake), or **`wlr-randr --output <out> --off|--on`** under the Wayland (labwc)
primary, where xrandr is blind; and (2) sends **CEC** standby/image-view-on for a CEC-capable TV
(disp2's Philips). So a plain monitor sleeps via the HDMI signal, a TV also goes to real standby.
Returns the `DisplayInfo`. `500` if the output toggle fails (under Wayland, falls back to CEC when
a TV is present rather than failing). **Multi-display:** an optional `"output":"HDMI-2"`
sleeps/wakes that output under X (CEC is only sent for the **primary**, since CEC is TV/bus-level
not per-output); omitting it (the back-compat default) targets the primary. The Wayland lever is
whole-screen (output addressing isn't wired there).

> **CEC standby vs display-off.** *CEC standby* is a control-channel command that puts a TV into
> its own standby (real power-down, wakeable over CEC) — only if the display is a CEC sink. *Output
> off* (xrandr) stops the HDMI signal so the display enters power-save regardless of CEC, but a TV
> may show "no signal" rather than fully power down. `/api/screen` does both; `/api/cec` is the
> CEC-only path.

### `GET /api/schedule/week` · `POST /api/schedule/week` → the display timeline

One scheduler owns the display timeline: a per-weekday list of **time → action** transitions,
date **exceptions**, and a folded-in **nightly window**. Node-local time; persisted (`schedule.json`,
hardened atomic write) and re-armed at boot. A single **edge-triggered** 30s tick (first tick after
the cold-boot window) computes the action that should be on screen now and fires it once per
transition — so a manual mode switch between transitions sticks until the next slot.

```json
{
  "enabled": true,
  "days": [ [ {"at":"08:00","action":"lobby"}, {"at":"18:00","action":"@sleep"} ], … 7 ],
  "exceptions": [ {"date":"2026-12-25","entries":[{"at":"00:00","action":"@sleep"}]} ],
  "nightly": {"enabled": true, "sleep": "22:00", "wake": "07:00"}
}
```

`days` is indexed by weekday (`0`=Sunday … `6`=Saturday). An `action` is a saved **Action slug**
(see `/api/actions`) or a reserved token: **`@sleep`** powers the display off, **`@wake`** powers
it back on **without changing the mode** (content keeps running behind a slept screen). The last
entry of a day carries past midnight until the next day's first entry; an empty day inherits the
previous day's last entry (blank a day with a single `{"00:00","@sleep"}`).

**Nightly window** is the everyday "off at `sleep`, back on at `wake`" cycle (its own `enabled`,
independent of the weekly `enabled`). It is **not** a second scheduler: when enabled it contributes a
daily `{sleep→@sleep, wake→@wake}` pair that is **merged** into each day's timeline, so it composes
with explicit entries under the same "last transition ≤ now wins" rule (an explicit entry at the
exact same minute wins). Enabling only the nightly window (weekly `enabled:false`) is fine.

`POST` replaces the whole schedule and returns it. `400` on a bad `HH:MM` / exception date, or a
nightly window whose `sleep`==`wake`. *The former `POST /api/schedule` (a separate nightly-only
scheduler) is retired — a legacy nightly window in `display.json` is migrated into `nightly` on
first boot after upgrade.*

### `GET /api/cec` · `POST /api/cec` → TV power over HDMI-CEC

`GET /api/cec` → `{ available, configured, tv_name, vendor, power, phys, device }` (uses
`cec-ctl` on `/dev/cec0`; `available:false` on nodes without it). `POST /api/cec
{"action":"on|off|active-source"}` drives a CEC TV (image-view-on + active-source / standby) —
TV power *without* touching the output. `501` if CEC is unavailable, `502` on a `cec-ctl` failure.

### `GET /api/vnc` · `POST /api/vnc` · `GET /vnc` → live screen view

`GET /api/vnc` → `{ supported, running, clients, pinned, port, scale, max_fps, nice, note, … }`
(drives webUI feature detection). `GET /vnc` serves the **embedded noVNC** viewer; it connects to `/vnc/ws`, a Go
WebSocket↔TCP bridge to a localhost-bound capture server that the agent starts on demand and stops
shortly after the last viewer leaves. The server is chosen by the **on-screen backend**: **x11vnc**
for the X surface, **wayvnc** for the labwc Wayland primary (x11vnc can't see labwc, and wayvnc
can't see X — RFB is identical to the bridge/viewer either way). `POST /api/vnc {"on":bool}`
pins/unpins. `supported:false` (with a `note`) when the matching server isn't installed, and `Pin`
then `501`s. *No websockify, no extra exposed port — all over `:80`.* Live-validated on disp (x11vnc,
X) and disp-deb-air (wayvnc 0.9.1 attaching to labwc, RFB on localhost:5900).

**Low-end capture knobs** (startup flags; for weak nodes like the Pi 3B, where full-res software
scraping on top of a software-rendered kiosk can thrash the box): `-vnc-scale` downsamples the
capture (x11vnc `-scale`, e.g. `0.5`; x11vnc-only), `-vnc-max-fps` caps the update rate (x11vnc
`-wait`/`-defer`, wayvnc `--max-fps`), and `-vnc-nice` runs the capture server at a `nice`
increment **plus `ionice -c3` idle I/O** so it can't starve the kiosk for CPU or SD-card I/O. The
active values appear in `GET /api/vnc` as `scale`/`max_fps`/`nice`. Suggested on disp:
`-vnc-scale 0.5 -vnc-max-fps 4 -vnc-nice 15`.

**View-only unless key-protected:** when the control surface is unauthenticated the live view is
forced read-only — `rfb.viewOnly=true` client-side plus the server-side lever (x11vnc `-viewonly`,
wayvnc `--disable-input`); with the key set, input is allowed (wayvnc input also needs the
compositor's virtual-pointer/keyboard protocols). The webUI detects the button at load (and after
login), so a backend change (X↔Wayland) shows/hides it on the next refresh.

### Power & lifecycle

- `POST /api/standby` — stop the on-screen mode **and** power the display off (a true low-power
  idle, distinct from `off` which leaves the screen lit and `sleep` which keeps the kiosk running).
- `POST /api/reboot` — `systemctl reboot` (returns `202` first). The agent comes back into the kiosk.
- `POST /api/shutdown` — `poweroff`, gated by `-allow-shutdown` (default on). `403` when disabled.
  ⚠️ A Pi has no Wake-on-LAN/RTC-wake — a powered-off node needs someone on site.
- `POST /api/restart` — relaunch the on-screen mode with a fresh child (hard kiosk refresh).

### Reliability

- `GET /api/health` — **auth-exempt** liveness for an external monitor: `200 {status:ok…}`, `503`
  when the mode is down.
- `GET /api/logs?tail=N[&format=json]` — tail of the agent + child log ring (text by default).
- **Watchdog** (`-watchdog`, on by default): reloads the kiosk when the network recovers (so a page
  left on an error refreshes itself), restarts a CDP-wedged kiosk, and — only with
  `-watchdog-reboot` — reboots a node whose mode stays down for minutes.

### Signage / content

The live signage surfaces are the **mixed-media playlist + the `/show` player** (below), the
**document viewer**, and **per-output content** — each drives the single web kiosk. A periodic page
reload (kiosk hygiene) rounds it out. Exactly one page owner drives the kiosk at a time.

- `POST /api/reload {"minutes":15}` — periodic page reload (0 disables); `{"now":true}` reloads now.
- `POST /api/message {"text":"…","seconds":30}` — overlay a banner on the kiosk (`{"clear":true}`
  removes it). Text is injected as DOM `textContent` (no HTML/script). Optional appearance:
  `"position"` (`top`|`bottom`|`center`|`top-left`|`top-right`|`bottom-left`|`bottom-right`, default
  `top`), `"size"` (font px, default 18, clamped 10–200), `"color"` and `"bg"` (CSS color tokens —
  hex / `rgb()` / named; anything else is rejected and the default used, so the inline style can't
  be broken out of). Works over both the X and Wayland kiosks (it's CDP-driven).
- `GET /api/document` → `{ src, auto_advance_s, enabled }`.
  `POST /api/document {"url"|"path":"…","auto_advance_s":0,"enabled":true}` — show a PDF / slides
  over the web kiosk. `url` is any `http(s)://`; `path` is a **relative path under `-docs-dir`**,
  served from `GET /docfs/<relpath>` (path traversal, symlink escapes, absolute paths, and other
  schemes are rejected; `404` if `-docs-dir` is unset). `/docfs/` is normally auth-gated, but the
  agent navigates the kiosk to its **own loopback URL** (`http://127.0.0.1:<port>/docfs/…`) and that
  fetch is **exempt only for loopback clients** — so a local document loads without widening LAN
  access. The PDF viewer chrome is hidden so the document fills the screen. `auto_advance_s>0`
  scrolls the top-level page every N seconds (good for a long HTML/scrolling doc; note Chromium's
  built-in PDF viewer renders multi-page PDFs in a sandboxed embed, so page-by-page advance there is
  best-effort). Enabling the document owns the page (a periodic reload won't reload it away).

### Media library (uploadable store)

The node's uploadable media store: a directory tree under `-media-dir` (default
`/var/lib/sideshow/media`) holding images, videos, audio, and documents the operator uploads and
arranges into playlists. It is the file-manager backend and the source for `/media/<path>` serving.
Every path is hardened against traversal + symlink escape. When `-media-dir` is unset/uncreatable the
library is disabled and the `/api/library*` endpoints return **`501`**.

- `GET /api/library?path=<rel>` → a folder listing:
  ```jsonc
  {
    "path": "lobby",                      // the listed folder, relative to the root ("" = root)
    "entries": [
      { "name": "clip.mp4", "kind": "video", "size": 1048576, "mtime": "…", "is_dir": false }
    ]
  }
  ```
  `kind` is one of `dir|image|video|audio|doc|other` (classified by extension). Entries are sorted
  dirs-first, then case-insensitive by name. `400` on a bad/nonexistent folder.
- `POST /api/library/upload?path=<folder>` (`multipart/form-data`) — streams each file part to disk
  under its filename (streaming keeps a large video off the heap on a low-RAM node). The target folder
  comes from the query. Each file is capped at **2 GiB**; a filename is reduced to a single safe
  component. Returns `{ "saved": ["clip.mp4 (…bytes)"], "listing": {…} }`. `400` if the body isn't
  multipart or no files were sent.
- `POST /api/library/mkdir {"path":"<parent>","name":"new"}` → creates a subfolder; returns
  `{ "ok": true, "listing": {…} }`. `400` on an invalid name or missing parent.
- `POST /api/library/rename {"from":"<rel>","to":"<name-or-rel>"}` — moves/renames an item; **both**
  `from` and `to` are root-relative paths (a rename in place is the same folder + a new leaf; a move
  to the root is just the leaf). `400` if the source is missing or the target already exists.
- `POST /api/library/delete {"path":"<rel>","recursive":false}` — removes a file, or a folder
  (recursive only when asked; a non-empty folder without `recursive` fails). The root can't be
  deleted. `400` on a bad/nonexistent path.
- `GET`/`HEAD /media/<path>` serves a library file with **byte-range** support (video seeking).
  Auth-gated, plus the loopback exemption so the kiosk / `/show` player can fetch it. Every response
  carries `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, and
  `Content-Security-Policy: sandbox` — so an uploaded `.html`/`.svg` can't script the API with the
  operator's cookie; images/video/PDF still render. `404` on a missing file or a disabled library.

### Mixed-media playlist + the `/show` player

An ordered list of images, videos, audio, and documents (from the media library or `http(s)://`
URLs) that the kiosk plays by pointing at the agent-served **`/show`** player page. `/show` is
self-contained: it fetches this config and advances client-side (on an interval, or when a
video/audio ends), so mixed media "one after another" works without any agent-side ticker. This is
the signage playlist (it replaced the legacy CDP-injected URL rotation / image slideshow). Persisted
in `playlist.json`.

- `GET /api/playlist-media` → the full config (also what `/show` fetches):
  ```jsonc
  {
    "items": [
      { "id": "…", "kind": "video", "src": "lobby/clip.mp4", "title": "", "duration_s": 0 }
    ],
    "interval_s": 8, "loop": true, "shuffle": false, "transition": "fade",
    "show_url": "http://127.0.0.1/show"    // loopback URL to point the kiosk at
  }
  ```
  Each item's `kind` is `image|video|audio|doc`; `src` is an `http(s)://` URL or a media-library
  relative path (resolved by `/show` to `/media/<src>`). `duration_s` overrides the interval for that
  item (0 = use the playlist interval; videos/audio always advance on their end event).
- `POST /api/playlist-media {"items":[…],"interval_s":8,"loop":true,"shuffle":false,"transition":"fade","play":false}`
  — replaces + persists the config (bad entries are dropped, `interval_s<1` → 8, ≤ 500 items).
  With **`play:true`** it also points the kiosk at `/show` — a real `web` mode switch (so the
  playlist persists and is restored on reboot), inheriting the current compositor backend and clearing
  the CDP content owners. `400` if `play:true` with an empty playlist. Returns the stored config.
- `GET /show` serves the embedded player page (loopback-exempt so the kiosk can load it).

The snapshot carries only the cheap `{ count, show_url }` summary; the full item list comes from
`GET /api/playlist-media` on demand.

### CEC volume + fleet

- `POST /api/cec {"action":"volume-up|volume-down|mute"}` — TV/amp volume over CEC; `-cec-monitor`
  watches the bus to learn when the TV is switched on/off by its own remote.
- **Heartbeat** (`-heartbeat-url`): the node POSTs a compact status+stats payload to a central
  aggregator on a timer (the bridge to the fleet panel). The payload also carries a `net` block
  (`{ online, iface, ip, wireless, ssid, signal }`, the same fork-free `link` summary the System box
  shows) so the panel can list each node's address and Wi-Fi strength. `-node-label`/`-node-group`
  add identity, surfaced in `/api/status`.

### Boot splash (Plymouth)

- `GET /api/plymouth` → `{ installed, enabled, theme, theme_set, message, image_set, note }` —
  `enabled` = whether `splash` is on the kernel cmdline (shows on the **next** boot); `theme_set` =
  the sideshow theme dir exists on the node.
- `POST /api/plymouth {"enabled":true|false}` — add/remove `quiet splash` in `-plymouth-cmdline`
  (`/boot/firmware/cmdline.txt`), so the graphical splash shows (or not) on the next boot. `503`
  when there is no cmdline file (a dev host).
- `POST /api/plymouth {"message":"Loading…"}` — set the splash status line. Regenerates the theme
  script (message inlined + escaped) and **rebuilds the initramfs** so it embeds for the next boot;
  the POST can take tens of seconds.
- `POST /api/plymouth {"image_base64":"<PNG>"}` — set the centered splash image (PNG only,
  validated by signature, ≤ 8 MiB; a `data:` URL prefix is stripped). Also rebuilds the initramfs.
- The sideshow Plymouth **theme** (a script theme: black bg, centered image, status line) ships in
  `agent/assets/plymouth/sideshow/` and is installed by `sideshow-deploy.sh plymouth` (separate from
  `install` because it edits `cmdline.txt`/`config.txt` and rebuilds the initramfs). `prereqs`
  installs the `plymouth` packages. **Node-side + reboot to take effect; not yet live-validated.**

### `GET /api/miracast` · `POST /api/miracast` → Miracast safety config

The experimental `miracast` sink is Wi-Fi Direct (P2P): on a single-radio node it can knock the box
off its own uplink and leave a headless node unreachable. The hard **`-allow-miracast`** deploy-time
gate (reported as `allowed`, not settable here) stays; this endpoint tunes three residual-risk
mitigations. Config persists in `miracast.json`; the guard goroutine runs whenever miracast is the
active mode.

- `GET /api/miracast` → `{ allowed, iface, max_minutes, abort_after_s, active }` (also in the
  snapshot's `miracast` block). `allowed` = the `-allow-miracast` gate; `active` = miracast is on
  screen now.
- `POST /api/miracast {"iface":"…","max_minutes":30,"abort_after_s":20}` — sets + persists the
  mitigations (negatives clamp to 0), returns the updated `MiracastInfo`:
  - **`iface`** — pin the P2P sink to a **dedicated second wireless adapter** (exported to the
    launcher as `SIDESHOW_MIRACAST_IFACE`), so it doesn't contend with the uplink radio;
  - **`max_minutes`** — auto-stop after N minutes (0 = unlimited) so it can't hold the radio;
  - **`abort_after_s`** — if connectivity is lost for N seconds *while miracast is on screen*, stop it
    and restore the last real mode (0 = off) — the node self-heals instead of needing an on-site
    power-cycle.

### Node identity & network

- `GET /api/hostname` → the node-identity block (same as the snapshot's `net`):
  `{ hostname, suggested, can_rename, protected, comitup, wifi:{ supported }, link }`. `suggested` is
  the `sideshow-<serial4>` default name; `protected:true` means the current name is load-bearing in
  the deploy convention (`disp`/`disp-deb-air`) and a rename is refused; `can_rename` = `hostnamectl`
  is present. `link` is the **live connectivity summary** —
  `{ online, iface, ip, wireless, ssid, signal }` — for the current (default-route) interface: `ip`
  its IPv4, `wireless` whether it is a Wi-Fi device, and, on Wi-Fi, the associated `ssid` and a `0–100`
  `signal`. It is read **fork-free** (from `/proc/net/{route,wireless}`, the Go `net` stdlib, and the
  `SIOCGIWESSID` ioctl — a syscall, not a subprocess) so it rides the frequent snapshot poll; the
  webUI shows it on the System box. A wired or offline node reports `wireless:false` and no `signal`.
- `POST /api/hostname {"name":"…"}` — renames the node (`hostnamectl set-hostname`), validated as an
  RFC-1123 label (1–63 chars, alphanumeric ends, hyphens inside). Takes effect live — the header
  updates on the next poll, no agent restart. `400` on an invalid label, a protected current name, or
  a node without `hostnamectl`. Returns the updated `NetInfo`.
- `GET /api/wifi` → the full, on-demand Wi-Fi state (forks `nmcli`, so it is **not** in the snapshot):
  ```jsonc
  {
    "supported": true, "managed": true, "active": "OfficeAP", "radio": "enabled",
    "networks": [ { "ssid": "OfficeAP", "signal": 78, "security": "WPA2", "active": true, "saved": true } ],
    "note": ""
  }
  ```
  Networks merge the live scan with saved connections (a saved-but-unseen network reports `signal:0`
  so it can still be forgotten); **PSKs are never returned**. `supported:false` (with a `note`) when
  `nmcli` or a wireless device is absent.
- `POST /api/wifi {"ssid":"…","psk":"…","hidden"?:false}` — joins a network (an empty `psk` = an open
  network); the PSK is passed as a separate argv element (no shell) and never logged. `400` on a bad
  SSID (empty, a leading `-`, control chars, > 32 chars) or a PSK outside 8–63 chars. With
  `hidden:true` — the way to pre-provision a hidden or not-yet-in-range network — a saved profile is
  created instead (`nmcli connection add … 802-11-wireless.hidden yes`, `autoconnect yes`) so the node
  probes for the SSID and joins when it appears; a "not in range now" activation failure is non-fatal
  (the profile persists). Returns the refreshed `WifiStatus`.
- `POST /api/wifi/delete {"ssid":"…"}` — forgets a saved connection (`nmcli connection delete`); only
  an SSID that already has a saved profile is accepted. `400` otherwise. Returns the refreshed
  `WifiStatus`.

### `GET /api/input` · `POST /api/input` → local keyboard/mouse policy

Node-global: whether the physically-attached keyboard/mouse drives the kiosk (remote control via the
panel / VNC is unaffected). The **persisted policy is the source of truth**, re-applied at boot; the
`-no-local-input` flag only seeds it on first boot. Also in `snapshot.input`.

- `GET /api/input` → `{ "allowed": bool, "supported": bool }`. `supported:false` when the agent is not
  root (it can't write the udev/Xorg rules).
- `POST /api/input {"allowed":bool}` → persists the policy and reconciles the libinput udev rule (all
  nodes) plus the `xorg.conf.d` Ignore snippet (agent-owned-X nodes). Applied **live** on a Wayland
  node by restarting the compositor (labwc re-enumerates input — a brief kiosk blip); on an
  agent-owned-X node the Xorg rule takes effect on the next agent restart / reboot. Returns
  `{ allowed, supported, applied_live, changed }`. `501` if the agent is not root.

### First-run setup wizard

A guided first-boot flow that detects the node and installs the feature prerequisites the operator
selects. It is **inert on an already-provisioned node**: a node with a persisted active mode is
migrated to `SetupComplete=true` at boot, and the setup surface is gated on `!SetupComplete`, so the
apt path never runs on a live node. While `!SetupComplete` the four setup endpoints are reachable
**pre-auth** (the bootstrap window — see the auth exemptions above); afterward they are gated like
everything else.

- `GET /setup` — the wizard HTML page.
- `GET /api/setup[?compositor=x11|wayland]` → the detection payload:
  ```jsonc
  {
    "complete": false, "arch": "arm64", "ram_mb": 730,
    "recommended_compositor": "x11",        // amd64/386 → wayland, else x11
    "seat_user": "sideshow", "seat_user_exists": true,
    "apt_available": true, "auth_enabled": true,
    "tools": { "chromium": true, "labwc": false, … },   // which binaries are present
    "features": [ { "key": "base", "label": "…", "packages": ["chromium", …], "compositor": "x11",
                    "installed": true, "required": true } ],
    "installing": false, "last_result": "", "log_tail": ""
  }
  ```
  `?compositor=` overrides the recommended compositor so the feature list reflects the operator's
  pick; without it the arch heuristic decides.
- `POST /api/setup/install {"compositor":"x11|wayland","features":["airplay",…]}` — starts a
  background `apt-get install` of the deduped union of the selected features' packages (the required
  `base` is always included), under `nice`+`ionice`. Returns `{ "ok": true, "installing": [pkgs…] }`.
  `501` if `apt-get` is absent, `400` if nothing resolves, `409` if an install is already running.
  The webUI polls `GET /api/setup` (`installing`/`last_result`/`log_tail`) to watch progress.
- `POST /api/setup/finish` → marks the wizard complete (`SetupComplete=true`, persisted), which closes
  the pre-auth window. `409` while an install is still running. Returns `{ "ok": true, "complete": true }`.

### Later (not yet implemented, shape reserved)

- **Second-output content rendering** — a positioned Chromium/mpv with its own CDP port for each
  non-primary output. Today secondary `/api/outputs/content` assignments are persisted + reported
  only; the primary renders. `geometryArgs` + the layout placement are the scaffold this wires.
- `wlr-randr` for multi-output **rotate/layout** under the Wayland kiosk (xrandr is blind there).
  *Wayland output sleep/wake is now wired (`/api/screen`); rotate/layout are still X11-only.*
- OTA agent self-update; scheduled content (day/night URLs) beyond the sleep schedule.
- Auth: per-node tokens / Tailscale identity, and TLS, once it leaves the LAN.

## 3. Notes for implementers

- **One owner invariant:** never start a new mode child before the previous one is reaped.
  The supervisor serializes switches (a single goroutine / mutex); concurrent `POST /api/mode`
  gets `409`.
- **Kiosk Chromium managed policy:** at startup the agent writes
  `/etc/chromium/policies/managed/sideshow.json` (`-chromium-policy-dir`) to shape kiosk defaults —
  `TranslateEnabled:false` (the `--disable-features=Translate` flag doesn't suppress the newer
  partial-translation bubble; the policy does) and `ExtensionInstallForcelist` for a cookie-dialog
  extension (`-cookie-extension`, default `edibdbjcniadpccecjdfdjjppcpchdlm` =
  I-still-dont-care-about-cookies; empty disables, or set Consent-O-Matic
  `mdjildafknihdffpkfmmpnpoiajfjnjd` to reject instead of hide). Both the X and Wayland kiosks read
  it (shared binary). Validated on disp + disp-deb-air: policy written, extension force-installs
  (proving the policy engine loaded it), no translate bar.
- **CDP is attach-not-launch** (ROADMAP §9): the supervisor *spawns* Chromium as the
  supervised child with `--remote-debugging-port` on **localhost only**, then the agent
  **attaches** over CDP (`chromedp` remote allocator). Browser lifecycle (restart-on-crash) is
  the supervisor's; control/navigation/screenshots are CDP's. Decouples the two.
- **Privilege:** the agent binds `:80` and manages systemd/watchdog as **root**, but spawns
  display children (`chromium`, `uxplay`, …) with credentials dropped to the **seat user**
  (e.g. uid 1000) and the seat env (`DISPLAY`, `XAUTHORITY`, `XDG_RUNTIME_DIR`).
  Chromium must not run as root.
- **Idempotency:** re-`POST`ing the current mode with the same params is a no-op (for `web`,
  a re-navigate at most); switching params re-navigates without restarting Chromium.
