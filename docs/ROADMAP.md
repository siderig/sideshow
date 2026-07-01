# sideshow — roadmap (open-source display node)

> **Scope:** an **open-source, bulletproof, Debian-based display node** that runs our software and is
> **controlled over a webUI.** Turn any Debian(-family) machine — Pi, old laptop, mini-PC, ARM board —
> into a reliable display that can show a web/kiosk page, media, an AirPlay mirror, a framebuffer app,
> or an arbitrary GUI/CLI program, switchable from a web interface.
>
> Any hosted or commercial offering is a **separate** concern — **this node must stand alone**,
> fully usable on its own without any external service.
> Living doc; settled calls in §9. Updated 2026-07-01.

## 1. What it is

The original idea, sharpened: a **master process manager for the display.** A small **shell /
supervisor** owns the screen and arbitrates **modes** (exactly one owns the display at a time),
controlled over a **webUI**:

| Mode | Owns the screen via | Tool today |
|------|--------------------|-----------|
| **Web / kiosk** | X11 / Wayland | Chromium under `labwc` (X11/matchbox fallback), driven via CDP |
| **Media** | GUI or KMS | mpv |
| **AirPlay** | KMS framebuffer | uxplay |
| **App** | GUI / CLI / TTY | arbitrary binary |

Open source. Reference node: **disp** (Pi 3) — the hand-built web-kiosk + AirPlay version we're
generalizing from.

**Direction (confirmed):** a **web runtime + a thin native shell.** The shell can drive **just the
web runtime** (a managed kiosk) or **the whole Debian OS** (full mode-switching). That spectrum is
the delivery model ↓.

**How a mode owns the screen — two paths, supervisor-arbitrated.** Only one thing can be DRM master
(own the KMS display) at a time, so the supervisor sequences the handoff via the seat (logind/seatd):
- **Compositor-hosted** — the mode runs as a **`labwc` client** (Chromium, mpv, uxplay `waylandsink`).
  labwc keeps DRM master; remote view/control "just works" (wayvnc, §4); a little compositing overhead.
- **Direct-KMS / framebuffer / console** — the mode **owns DRM/KMS itself, no compositor** (uxplay
  `-vs kmssink`, mpv `--vo=drm`, a raw KMS/fbdev app, a TTY/CLI program). **Faster** (no compositing —
  notably AirPlay on weak hardware) but **not live-viewable via wayvnc** → falls back to periodic
  framebuffer screenshots for "view." The supervisor **tears labwc down** to grant KMS, restarts it on exit.

**Framebuffer/console is a first-class mode, not an escape hatch.** For modes that can do both (uxplay,
mpv) it's a **per-mode toggle in the webUI: Fast (direct KMS) ↔ Remote-viewable (Wayland client)**.

## 2. Delivery = how much OS the shell owns

| Form | The shell owns… | Bulletproofness | Use |
|------|----------------|-----------------|-----|
| **App** — run our binary on your Debian | the web runtime (a kiosk) | basic | least intrusive; keep your own OS |
| **Script** — convert an existing Debian install | OS config (autologin, session, watchdog, modes) | strong | turn a stock box into a display node |
| **Image** — flash our prebuilt image | the whole OS (read-only root, A/B updates) | maximum | appliance; hands-off |

Same shell + payload; the forms differ only in how much they configure and lock down. We want all
three eventually. **Start with the script** (fastest, reuses disp), then the image.

## 3. Bulletproof engineering (the actual promise)

Assume cheap hardware + flaky network + **nobody on site**; never blank the screen.

**Always (every tier):**
- **`Restart=always` + a supervisor** on the renderer — disp has **no respawn today** (if the kiosk
  dies the screen stays dead). Fix this first.
- **Hardware watchdog** to reboot a wedged box.
- **Offline-first** — cache the web app / content locally; a network blip must not change the screen.
- **Graceful degradation** — control-plane unreachable → keep showing last-good content, keep retrying.
- **Health heartbeat + health-check** in the webUI (also the trigger for any rollback).
- **Read-only / mostly-ro root, writes → tmpfs** (logs, caches). This fights the **#1 real killer of
  cheap flash — write-wear** — and makes power-loss corruption nearly impossible. Independent of the
  update mechanism; helps every tier. (Probably a bigger reliability win than the update model itself.)

**Update safety, which scales with the tier (the *why* is in §7):**
- **App / script (mutable, apt):** routine `apt upgrade` within a release is **low-risk**. Safety net
  = **btrfs + auto-snapshot before upgrade** (snapper/timeshift) + disciplined `unattended-upgrades`
  (`--force-confold`, scoped, nightly reboot window) + the watchdog/health-check above. **Gate
  major-version upgrades** (e.g. Bookworm→Trixie) behind a controlled rollout — **never auto-`dist-upgrade`** (that, not routine upgrades, is what broke disp).
- **Image (appliance):** **A/B atomic updates + auto rollback** (RAUC/Mender), optionally + dm-verity.
  The only tier where A/B earns its weight.

## 4. WebUI control (open source, self-contained)

- Each node serves a **local control webUI + API**: pick mode, set URL/playlist, status, logs, reboot.
- Optional **lightweight self-hosted aggregator** to view/drive several nodes — a simple fleet
  view, not a multi-tenant hosted service. The node stays fully usable standalone.
- Reach nodes over LAN, or Tailscale/Headscale for remote/support (optional).

**Remote view & control of the screen:**
- **Wayland/labwc → `wayvnc`** (view + keyboard/mouse); **X11 fallback → `x11vnc`**. (`wayvnc` is
  already installed on `disp`.)
- **Embed `noVNC`** (VNC-over-websocket) in the webUI → "click a node, see/control its screen in the
  browser," authed by the panel. Bind VNC to **localhost only** and tunnel over Tailscale/SSH — never
  an open VNC port (VNC's own auth is weak).
- **Cheap-by-default fleet view:** push **periodic screenshot thumbnails** (CDP `captureScreenshot` in
  web mode; `grim`/`scrot`/DRM-grab otherwise) for the always-on overview; start a **full VNC/noVNC
  session only on demand**. Keeps 4G / many-node bandwidth sane.
- Direct-KMS modes have no compositor to capture → thumbnails only, no interactive control while active.

## 5. Component shopping list (debs)

chromium · `labwc` (Wayland; matchbox/X11 fallback) · `seatd`/logind (seat + DRM-master handoff) · mpv ·
uxplay (+avahi +gstreamer) · `wayvnc`/`x11vnc` + `noVNC` + `grim`/`scrot` (remote view & thumbnails) ·
the **sideshow shell/agent** · comitup / wifi-connect (headless WiFi onboarding) · chrony · cec-utils
(scheduled screen on/off) · watchdog. **Hard per-board bit:** GPU/video **hardware decode** varies a
lot — expect software-decode fallback on weak units; treat HW accel as per-platform tuning.

## 6. Reuse vs. build (prior art, OSS-node lens)

- **Reuse outright:** **UxPlay** (AirPlay), **mpv** (media), **Chromium / cage / Ubuntu Frame**
  (kiosk), **comitup** (onboarding), **RAUC** or **Mender** (A/B for the image form).
- **Reference:** **Thymis** (NixOS fleet running arbitrary apps + kiosk with a webUI — closest
  "run-any-app + webUI" pattern, but not Debian, not mode-switching, no AirPlay); **FullPageOS**
  (single-mode Pi kiosk image).
- **The gap we fill:** a **Debian** node that switches **web / media / AirPlay / arbitrary app /
  framebuffer** from a **webUI**. No OSS does this — Anthias even closed an AirPlay-mode PR (#2634,
  stale 2026-06-18). This is the "extra display software" worth open-sourcing.

## 7. Update & integrity models — why A/B is image-tier-only

Three *different* risks, three different tools — don't conflate them:

- **Silent corruption / bit-rot** → **filesystem checksums** (btrfs/ZFS). A/B+ext4 wouldn't even
  *detect* it. (On a single SD, btrfs DUP covers bad sectors but not wear-out — and write-wear is the
  bigger flash killer, so the read-only-root point in §3 matters more here.)
- **Bad-but-installed update** (a regression, or your stack breaks) → **rollback**. btrfs **snapshots**
  (snapper/timeshift) give this for the apt model: auto-snapshot before upgrade, roll back if broken —
  the "poor-man's A/B," and it fits "keep using apt." Caveat: rollback is usually interactive at boot
  (grub-btrfs), clean on amd64/GRUB but messier on a **Pi** (no GRUB — boots `kernel8.img` from FAT).
- **Atomicity + automatic *headless* rollback** → **A/B roots** (RAUC/Mender). The running slot is
  never touched; an update writes a whole new rootfs to the other slot; the bootloader switches and
  **auto-reverts on a failed health-check, no human needed**. Only worth it for **shipped images** —
  it doubles rootfs storage, can't retrofit onto "convert my Debian," and updates become "replace
  image," not `apt install`. config/state on a separate data partition; `/etc`,`/var` on an overlay.
  **RAUC** = lean OSS mechanism; **Mender** = mechanism + self-hostable server/UI (`mender-convert`
  turns a Debian image A/B).

**Honest read on plain `apt upgrade`:** low risk for routine within-release updates — the safety
net's value scales with *unattended + cheap hardware + nobody on site*, not with day-to-day apt
danger. So: **snapshots + hygiene for the mutable tiers; A/B only for shipped images.**

## 8. Open questions (technical)

- **Mode handoff (the core build task)** — implementing the supervisor's DRM-master handoff between
  labwc-hosted and direct-KMS modes via seatd/logind (approach decided in §1; the implementation is
  the main technical risk — clean release/acquire with no black-screen flap).
- **Node API** — define it once; the local webUI, the optional aggregator, and any future SaaS all speak it.
- **Image build tooling** — pi-gen / debos / armbian-build; which boards to bless first.
- **Snapshot/rollback UX per platform** — btrfs + snapper/timeshift rollback is clean on amd64/GRUB,
  but the Pi has no GRUB (boots from FAT) — how do we get auto-rollback there? Plus read-only-root +
  A/B feasibility across Debian/Raspbian/Armbian (boot differs per platform).

## 9. Decisions

- **2026-06-27** — This project = the **OSS Debian display node + webUI**. Any hosted/commercial
  offering is a separate concern; the node must stand alone.
- **2026-06-27** — **Web runtime + thin native shell**; the shell can drive **just the web runtime or
  the whole Debian OS** → that spectrum = the **app / script / image** delivery forms.
- Don't ship **Raspbian as the only base** (Pi-only); target generic **Debian + Raspbian + Armbian**.
- Reuse **UxPlay** (AirPlay) + **mpv** (media); don't adopt balena or an AGPL CMS as the base.
- **2026-06-27** — **A/B roots are image-tier-only.** Mutable (app/script) tiers use **btrfs
  snapshots + read-only-root + gated major-version upgrades** instead. Routine `apt upgrade` is
  low-risk (disp broke on a *dist-upgrade*, not routine updates). Treat the risks separately:
  filesystem **checksums** (corruption) ≠ **snapshots** (rollback) ≠ **A/B** (atomic auto-rollback).
- **2026-06-27** — **Display: Wayland + `labwc`, no display manager.** The supervisor runs as a
  systemd service (`Restart=always`, seat via logind/seatd) and launches labwc + the active mode.
  **X11 + matchbox** (agent-owned Xorg via `-start-x`) is the fallback for Wayland-hostile old
  hardware. (Dropped lightdm — it's the very failure mode that broke disp; the agent starts + owns
  Xorg itself.)
- **2026-06-27** — **Framebuffer/direct-KMS is a first-class mode.** Modes run either compositor-hosted
  (labwc client, remote-viewable) or direct-KMS (faster — e.g. uxplay `kmssink`, mpv `--vo=drm`, raw
  fbdev/TTY apps). For dual-capable modes the webUI offers a **Fast (KMS) ↔ Remote-viewable (Wayland)**
  toggle; the supervisor arbitrates DRM master.
- **2026-06-27** — **Remote view/control = `wayvnc` (Wayland) / `x11vnc` (X11) + `noVNC` in the webUI**,
  localhost-bound + tunneled; screenshot thumbnails by default, full VNC on demand.
- **2026-06-27** — **Browser control via the DevTools Protocol (CDP), attach-not-launch.** The
  supervisor runs Chromium as a supervised process (`--remote-debugging-pipe` / localhost port, kiosk,
  ozone-wayland) and the agent **attaches** over CDP (decouples browser lifecycle from the controller →
  more robust than disp's puppeteer-launches-and-owns model). Client = **`chromedp`** / raw CDP (agent
  is Go, below) — not puppeteer or Playwright (Playwright = a multi-browser *test* framework, overkill
  for one kiosk Chromium). CDP also yields screenshots (thumbnails) + JS/CSS injection (e.g. dark mode).
  Bind debugging to **localhost only** (CDP is unauthenticated).
- **2026-06-27** — **Agent/supervisor language = Go.** Single static binary; trivial cross-compile
  (arm64 / amd64 / armhf); small footprint + fast start on cheap hardware; goroutines fit the job
  ("supervise N processes + serve the local API/webUI + heartbeat + stream thumbnails"); mature CDP
  (`chromedp`); dead-simple **self-update** (swap one signed binary). Consistency bonus: Tailscale (our
  connectivity layer) is Go. **Rust** is the runner-up (smaller/safer, no GC) — choose it only if the
  team is already Rust-strong or wants absolute-minimal footprint; the agent is *orchestration*, not
  perf/safety-critical, so Go's velocity wins. **Node stays for the web layers, not the daemon** — the
  content runtime (inside Chromium) and the local webUI frontend remain TS/web; only the system
  supervisor is Go.
- **2026-06-27** — **v0 agent built and live-validated on disp** ([`agent/`](../agent/),
  [`node-api.md`](node-api.md)). Confirms the decided stack on real hardware: Go supervisor as a
  `Restart=always` systemd service, CDP **attach-not-launch** via `chromedp`, `:80` webUI/API,
  child privilege-drop to the seat user. Two interim choices for v0: (a) **reuse disp's existing
  X11/openbox session** rather than converting to Wayland/labwc first (the handoff's "don't get
  blocked" call) — labwc is still the target; (b) **`display:kms` and airplay/media return HTTP
  400** until the DRM-master handoff (§8) exists, since tearing down the working mode to launch
  something that fails on the only screen is worse than refusing. Adversarial review (21
  confirmed findings) hardened the supervisor before deploy: deadlock-free context-driven
  teardown, crash-safe switch flag, pre-flight-before-teardown (never blank on a bad switch).
- **2026-06-27** — **The Pi 3B is marginal for this workload.** Steady-state (one Chromium
  kiosk) is fine (load ~1), but load storms (two Chromiums, repeated software-rendered
  screenshots) starve systemd PID1 and the active watchdog reboots the box. Implication for the
  bulletproof tiers: prefer HW-accel where available, keep exactly one display owner, and treat
  the watchdog as a real actor, not just a backstop. **Deploys must be done idle + one step at a
  time** (atomic binary swap, isolated restart) — rsync + restart + Chromium cold-start stacked
  together has twice tripped the watchdog into a reboot.
- **2026-06-27** — **Display modes are two surfaces, handed off by VT switching.** A persistent
  **compositor** surface (web/app on Xorg's VT) and an on-demand **foreground** surface
  (`console` TTY app / future `kms`) on a dedicated VT (tty8); `chvt` is the DRM-master handoff
  (logind drops/grants master on VT switch). Validated live with `htop` (renders on the console
  VT; auto-recovers to the compositor if it crash-loops). Console children need `TERM=linux` and
  must **not** get a controlling TTY via `Setctty` (TIOCSCTTY EPERMs after the setuid drop;
  ncurses reads the inherited fd directly). **Caveat:** on the Pi's vc4 driver Chromium does not
  survive a VT switch, so the compositor surface can't truly stay "warm" behind a foreground
  mode here — it cold-restarts on return. The two-tier model is still right for hardware where
  the compositor survives VT switches; on the Pi it degrades to a clean restart.
- **2026-06-30** — **cog (WPE WebKit) framebuffer kiosk = controlled over D-Bus, not WebDriver; no
  dark mode on the current stack.** The `web` + `display:kms` mode runs `cog` directly on DRM/KMS
  (no X, ~½ the RAM and ~⅓ the CPU of software-Chromium on the Pi 3B) and is driven in place via
  cog's built-in D-Bus actions (`cogctl open`/`reload` → `/api/url`, `/api/reload`). A
  WebDriver-managed cog (for screenshot/zoom/JS) was prototyped and **dropped** — cog's `--automation`
  is unreliable on the DRM backend ("automation is not allowed in the context" → hangs/exits), and
  cog is **EOL** (0.18.x is the final series). So `/api/theme` + `/api/zoom` → **501**, `/api/screenshot`
  → **503**; use the **Chromium/CDP kiosk** for pages needing theme/zoom/screenshots.
  - **Dark mode is genuinely not reachable on our WPE 2.48.3** (Debian Trixie): no public GLib
    color-scheme API; the override (`Page::setUseDarkAppearanceOverride`) is compiled in but only
    reachable over WebKit's **private remote-inspector** transport, which (verified empirically on
    disp) exposes no usable HTTP/WS entry point — no maintained client. Forcing CSS
    `@media (prefers-color-scheme)` from injected JS/CSS is impossible by design.
  - **The real path exists upstream, just newer than Debian ships:** WPE's **new WPEPlatform API**
    has a `WPESettings` registry with **`WPE_SETTINGS_DARK_MODE`** (`[wpe-platform] dark-mode=true`),
    with dark mode **fixed in WPE WebKit 2.49.2**; MiniBrowser exposes it via `--config-file`. Our
    2.48.3 lib has **zero** WPEPlatform/WPESettings symbols (confirmed), cog 0.18 uses the *old*
    plugin model, and the WPEPlatform API is still **"preview"** as of 2.52. So enabling dark on WPE
    needs **(a) a ≥2.49.2 WPE built outside Debian** (meta-webkit/Yocto — see [the RPi
    wiki](https://github.com/Igalia/meta-webkit/wiki/RPi) — or a backport/Forky) **and (b) a
    WPEPlatform launcher** (new MiniBrowser / future cog / a small custom launcher). That also moves
    off EOL cog onto the actively-developed platform API.
  - **Decision: leave cog as-is** (Debian 2.48.3 + D-Bus, theme/zoom 501) and use Chromium for
    dark-needing pages. Revisit WPEPlatform `dark-mode` only if/when we commit to a newer-than-Debian
    WPE stack. Refs: [WPE 2.49.2 notes](https://wpewebkit.org/release/wpewebkit-2.49.2.html) ·
    [WPESettings API](https://www.mail-archive.com/webkit-changes@lists.webkit.org/msg221604.html) ·
    [cog#775](https://github.com/Igalia/cog/issues/775).

## 10. Phasing

1. **Harden `disp`** as the reference node — ✅ **done 2026-06-27.** Kernel rebooted onto
   6.18.34; the no-respawn gap is fixed by running the agent as a `Restart=always` systemd
   service (the supervisor restarts the kiosk on crash). Hardware watchdog already active
   (BCM2835, `RuntimeWatchdogUSec=1min`). Cold power-cycle reboot validated — the service
   comes up automatically into `web/running`.
2. **Mode abstraction + node API** — ✅ **v0 done.** Spec in [`node-api.md`](node-api.md);
   `mode = {type, params, display}`, REST on `:80` (`/api/status`, `/api/mode`, `/api/url`,
   `/api/screenshot`, `/`).
3. **Shell/supervisor + local webUI** — ✅ **built + live-validated on disp + `disp-deb-air`**
   ([`agent/`](../agent/), Go), now broad. Working: `web` (Chromium via CDP attach-not-launch,
   dark mode, screenshots) + `app` (compositor) modes, restart-on-exit, privilege drop, embedded
   webUI; **`console` mode** — a TTY app on a dedicated VT via the **VT-switch DRM-master
   handoff** (logind), with automatic recovery to the compositor on a foreground crash. Since
   shipped: the **direct-KMS path landed** — `airplay` (uxplay, X11 client *or* `-vs kmssink` on
   its own VT), `media` (mpv, X11 client *or* `--vo=drm`), and `web`/`app` on `display:kms`;
   validated on both an X11 (disp) and a Wayland (`disp-deb-air`) node. Plus **Wayland/labwc**
   (the seatd/seat-user GPU refactor, ✅ on `disp-deb-air`), **agent-owned Xorg + matchbox**
   (`-start-x`; disp dropped lightdm+openbox), a **universal DRM/KMS screenshot** (`kmsshot`),
   **embedded VNC/noVNC** (x11vnc for X / wayvnc for labwc), extra receivers (`moonlight`,
   `steamlink`, experimental `miracast`), CEC, power/reboot, watchdog, signage/playlist,
   heartbeat, and **headless WiFi onboarding** (nmcli `WifiConnect` + a `-comitup` recovery-AP
   fallback). **Pi note:** the vc4 KMS driver does **not** keep Chromium alive across a VT switch,
   so each console excursion costs one Chromium cold-start on return (bounded, but a load spike on
   the marginal Pi 3B).
4. **Convert-Debian script + first-run install** — 🟡 **landed.** `scripts/install.sh` brings the
   agent up on a fresh Debian/Raspbian node (install binary → generate auth key → write + enable
   the `sideshow-agent` systemd unit → start it) and hands off to a **browser `/setup` wizard**
   (`/api/setup*`) that detects the platform and installs the compositor feature packages; the
   richer `scripts/sideshow-deploy.sh` covers build/deploy/prereqs/x-own. Next: a **flashable
   image** with read-only root + A/B updates.
5. Optional **self-hosted aggregator** panel for several nodes.
