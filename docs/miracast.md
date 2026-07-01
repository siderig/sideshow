# Miracast on a Sideshow node — why it's gated, and how to run it safely

Miracast lets a phone/laptop mirror its screen to a node (`mode: miracast`, an
X11 wireless-display **sink** launched via `-miracast-cmd`, default
`gnome-network-displays`). It is **off by default** behind `-allow-miracast`, and
for a real hardware reason — not caution theatre.

## Why it's behind a flag

Miracast is **Wi-Fi Direct (Wi-Fi P2P)**, not a normal client connection to your
access point. The problem is one hardware fact:

> The Pi's built-in Wi-Fi (Broadcom `brcmfmac`) has a **single radio**. At any
> instant it can be tuned to exactly one channel.

In normal operation the radio is in **STA mode** — associated to your AP on some
channel, carrying everything you care about: SSH, the webUI, the heartbeat. A
Miracast receiver must **form/join a P2P group** with the casting device — a
second link that very often lands on a *different* channel or band (the sender may
push 5 GHz; the group owner may pick a "social" channel 1/6/11).

Serving an AP and a P2P group on two different channels needs **multi-channel
concurrency** (rapid time-slicing) that the Pi's chip barely does. So the moment
P2P negotiates a channel ≠ your AP's, the radio leaves the AP's channel and **the
uplink stalls or drops** — and on a headless node you can no longer reach it to
*stop* the mode. Worst case: an on-site power-cycle.

Secondary reasons: bringing up P2P can reset the STA association (driver
dependent); and the Linux Miracast *sink* stack (`gnome-network-displays` /
`miraclecast` + GStreamer + wpa_supplicant P2P) is immature and **unvalidated
end-to-end in this project** — the flag also means "experimental."

## Mitigations (best → situational)

The agent implements the agent-controllable ones as settings (webUI **Settings →
Miracast**, API `GET|POST /api/miracast`, persisted `miracast.json`, seeded from
flags). The rest are deployment guidance.

1. **Uplink over Ethernet** *(guidance — best fix)*. If the node reaches your
   LAN/webUI over `eth0`, the Wi-Fi radio is *free* to do P2P with zero
   contention — the control channel is never affected. For a fixed install, wire
   it and don't rely on Wi-Fi for control.
2. **Dedicated second Wi-Fi adapter** *(`-miracast-iface` / Settings)*. A cheap
   USB dongle: `wlan0` = infra uplink, `wlan1` = P2P. No shared radio → no
   contention. The agent exports the chosen interface to the launcher as
   **`SIDESHOW_MIRACAST_IFACE`**; a wrapper `-miracast-cmd` (or a
   `gnome-network-displays` build that honours it) binds the sink there. Set the
   interface in Settings → Miracast.
3. **Same-channel P2P** *(guidance)*. Force the P2P group owner onto your AP's
   exact channel so a single radio can sometimes sustain STA+P2P (single-channel
   concurrency). Fragile — breaks the instant the sender forces 5 GHz — and is a
   wpa_supplicant/driver setting the agent doesn't control.
4. **Session time-box** *(`-miracast-max-minutes`, default 30 / Settings)*. The
   guard auto-stops Miracast after N minutes so it can't hold the radio
   indefinitely. `0` = unlimited.
5. **Uplink auto-abort** *(`-miracast-abort-after`, default 30 s / Settings)*.
   While Miracast is on screen, a guard goroutine dials the connectivity probe
   (`-watchdog-probe`); if the uplink stays down for N seconds, it **stops
   Miracast and restores the previous mode** — the node self-heals in ~30 s
   instead of bricking until someone visits. `0` = disabled. (Caveat: the probe
   tests general connectivity, so a true internet outage while the LAN is fine
   would also trigger an abort — err toward recovery. Point `-watchdog-probe` at
   your gateway for LAN-specific detection.)

## Not yet: comitup recovery AP *(Phase 3)*

When Phase 3 lands **comitup**, a node that loses its Wi-Fi uplink can raise a
recovery AP (`sideshow-XXXX`) so you can reconnect and stop the mode — a
last-resort safety net for the worst case. Until then, the auto-abort (5) is the
recovery path; combine it with Ethernet (1) or a second adapter (2) for a node
you actually cast to.

## Enabling it

1. Start the agent with `-allow-miracast` (the hard gate) — ideally also
   `-miracast-iface wlan1` if you added a second adapter, or just run the uplink
   on Ethernet.
2. Tune the time-box / auto-abort in **Settings → Miracast** (safe defaults:
   30 min / 30 s).
3. Install the sink: `sideshow-deploy.sh` … (a `-miracast-cmd` that exists on the
   node; `gnome-network-displays` for the default).
4. Switch to it from **Now → Miracast**. The tile's Start stays disabled until
   `-allow-miracast` is set.
