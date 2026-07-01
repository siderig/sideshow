#!/bin/sh
# Deploy + drive the sideshow agent on the reference node `disp`.
#
# disp now runs AGENT-OWNED Xorg + matchbox (no lightdm, no openbox): the agent
# starts the X server itself via -start-x and boots through multi-user.target (the
# `xown` subcommand performs that cutover). Rebooting disp is fine — it comes back
# to the agent-owned kiosk on its own. The test nodes are disposable — there is no
# rollback path; re-flash or re-run `install` to recover.
#
# Usage:
#   scripts/sideshow-deploy.sh build            # cross-compile $ARCH → ./dist/
#   scripts/sideshow-deploy.sh push             # build + rsync to disp:/usr/local/bin
#   scripts/sideshow-deploy.sh stop-legacy      # stop the puppeteer kiosk (keeps Xorg+openbox)
#   scripts/sideshow-deploy.sh run              # run the agent in the foreground (dev; Ctrl-C to stop)
#   scripts/sideshow-deploy.sh deploy           # atomic binary swap + restart (in-place update)
#   scripts/sideshow-deploy.sh install          # push + install as the systemd kiosk service (boot)
#
# Targets other than the default Pi via env vars, e.g. the x86 Wayland node:
#   ARCH=amd64 SEATUSER=displinux NODE=root@disp-deb-air.local scripts/sideshow-deploy.sh deploy
set -eu

NODE="${NODE:-root@disp.local}"
URL="${URL:-about:blank}"        # initial kiosk URL for `run` (dev); set real content via the webUI
ARCH="${ARCH:-arm64}"            # build target: arm64 (Pi) | amd64 (x86) | arm (armhf, +GOARM=7)
SEATUSER="${SEATUSER:-sideshow}" # user to run display children as (override per node, e.g. SEATUSER=…)
REMOTE_BIN=/usr/local/bin/sideshow-agent
HERE="$(cd "$(dirname "$0")/.." && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
# Snapshot dir for THIS node: <name>/fs mirrors its real abs paths, so the
# launcher/config we push come from (and stay in sync with) the target's own
# snapshot. <name> is the short hostname of $NODE (root@disp.local -> disp).
# Maintainer snapshots live under .private/nodes/ (gitignored, not published);
# fall back to a tracked nodes/ if one exists.
NODENAME="${NODE#*@}"; NODENAME="${NODENAME%%.*}"
FS="$REPO/.private/nodes/$NODENAME/fs"; [ -d "$FS" ] || FS="$REPO/nodes/$NODENAME/fs"
DIST="$HERE/dist"

# Auto memory-cap policy (used by `install` and the `memcap` auto path). A node
# with LESS than the threshold RAM gets a proportional cgroup cap so a runaway
# page OOM-kills the browser, not the box; a capable machine is left
# unconstrained (a heavy WebGL scene must run free). Percentages are of total RAM;
# swap is bounded to ~zram. Override any of these via the environment.
MEMCAP="${MEMCAP:-auto}"                          # auto | off | "<high> <max> [swap]"
MEMCAP_THRESHOLD_MB="${MEMCAP_THRESHOLD_MB:-1536}" # auto-cap nodes with < this much RAM
MEMCAP_HIGH_PCT="${MEMCAP_HIGH_PCT:-66}"
MEMCAP_MAX_PCT="${MEMCAP_MAX_PCT:-78}"
MEMCAP_SWAP_PCT="${MEMCAP_SWAP_PCT:-40}"

build() {
  echo ">> cross-compiling linux/$ARCH"
  mkdir -p "$DIST"
  ( cd "$HERE" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$DIST/sideshow-agent-$ARCH" . )
  ls -la "$DIST/sideshow-agent-$ARCH"
}

push() {
  build
  echo ">> rsync → $NODE:$REMOTE_BIN"
  rsync -avz "$DIST/sideshow-agent-$ARCH" "$NODE:$REMOTE_BIN"
  ssh "$NODE" "chmod 755 $REMOTE_BIN"
}

stop_legacy() {
  echo ">> stopping legacy puppeteer kiosk on $NODE (Xorg + openbox stay up)"
  # Kill the node launcher and any chromium it owns; lightdm does NOT respawn
  # run-chromium.sh, so this is stable until the agent is (re)installed.
  ssh "$NODE" "pkill -u $SEATUSER -f 'node index.mjs' || true; sleep 1; pkill -u $SEATUSER chromium || true; sleep 1; pgrep -u $SEATUSER chromium >/dev/null && echo 'WARN: chromium still running' || echo 'legacy kiosk stopped'"
}

run() {
  echo ">> running agent on $NODE :80 (children dropped to $SEATUSER on DISPLAY :0)"
  echo "   webUI: http://${NODE#*@}/   — Ctrl-C to stop"
  ssh -t "$NODE" "DISPLAY=:0 $REMOTE_BIN -addr :80 -seat-user $SEATUSER -url '$URL' -start-mode web"
}

deploy() {
  # Safe in-place UPDATE of an already-installed agent (binary swap + restart).
  # Atomic: rsync to a temp name, verify, mv into place, then restart ALONE
  # (never stacks rsync+restart+cold-start load — the watchdog lesson, §8).
  build
  echo ">> node load before deploy:"; ssh "$NODE" "cat /proc/loadavg"
  echo ">> rsync → $NODE:$REMOTE_BIN.new (atomic swap)"
  rsync -avz "$DIST/sideshow-agent-$ARCH" "$NODE:$REMOTE_BIN.new"
  # Keep the Wayland launcher in sync (it carries the URL shell-injection fix);
  # warn but don't abort the binary swap if it can't be shipped.
  rsync -avz "$FS/home/$SEATUSER/run-wayland.sh" "$NODE:/home/$SEATUSER/run-wayland.sh" \
    || echo "   WARN: run-wayland.sh sync failed — Wayland launcher NOT updated"
  ssh "$NODE" "chown $SEATUSER:$SEATUSER /home/$SEATUSER/run-wayland.sh 2>/dev/null; chmod 755 /home/$SEATUSER/run-wayland.sh 2>/dev/null; \
               ls -l $REMOTE_BIN.new && chmod 755 $REMOTE_BIN.new && mv -f $REMOTE_BIN.new $REMOTE_BIN && systemctl restart sideshow-agent && sleep 3 && systemctl is-active sideshow-agent && echo restarted"
  echo "   webUI: http://${NODE#*@}/   (give cold Chromium ~30s to attach CDP)"
}

prereqs() {
  # Install the optional node-side tools the newer features need, under
  # nice/ionice and with needrestart in list-only mode (it stalls otherwise).
  #   x11vnc        → live view (the X surface)
  #   wayvnc        → live view under the Wayland (labwc) primary (x11vnc can't see it)
  #   mpv + scrot   → native media mode (mpv as an X11 client) + the X11
  #                   screenshot fallback /api/screenshot uses for media/app
  #   labwc + grim  → Wayland kiosk
  #   wlr-randr     → screen sleep/wake under the Wayland (labwc) primary
  #   seatd         → run labwc as the seat USER (GPU/GLES2 path); the user is
  #                   added to the seatd socket's group. RESTART the agent after
  #                   (children inherit the seat user's groups at agent startup).
  #   uxplay        → AirPlay receiver mode (X11 client; needs avahi-daemon for
  #                   discovery — pulled in as a dep, started below)
  #   moonlight-qt  → Moonlight receiver mode (X11 client; may be in a separate repo)
  #   plymouth      → boot splash (/api/plymouth); the sideshow theme is installed
  #                   by `install`, not here.
  #   gcc/make/pkg-config + libdrm/egl/gles2/gbm -dev → toolchain to build
  #                   disp-kmsshot (the universal DRM screenshot helper), compiled
  #                   on the node by `install` / the `kmsshot` subcommand.
  # Run when the node is idle. Uses the script-level $SEATUSER (default sideshow).
  # Validated end-to-end on disp-deb-air (Intel x86, Mesa HD 5000 / GLES 3.2):
  # GPU-accelerated labwc as the seat user. RESTART the agent after (children
  # inherit the seat user's groups at agent startup). On a headless box (no DM)
  # the seat user is created and given a lingering session so /run/user/<uid> exists.
  # uxplay/moonlight-qt may not be in every Debian suite; '|| true' keeps the rest
  # of the install from aborting when one package is unavailable.
  echo ">> installing feature prerequisites on $NODE (rsync, chromium, x11vnc, wayvnc, mpv, scrot, labwc, grim, wlr-randr, seatd, uxplay, plymouth, + disp-kmsshot build deps)"
  ssh "$NODE" "nice -n 19 ionice -c3 apt-get -y -o needrestart::mode=l install rsync chromium x11vnc wayvnc mpv scrot labwc grim wlr-randr seatd dbus-user-session plymouth plymouth-themes gcc make pkg-config libdrm-dev libegl-dev libgles-dev libgbm-dev xdg-desktop-portal xdg-desktop-portal-gtk gsettings-desktop-schemas xsettingsd adwaita-icon-theme matchbox-window-manager xauth unclutter-xfixes
    # uxplay (AirPlay) + gstreamer1.0-plugins-bad (provides kmssink for display=kms,
    # the performant framebuffer path) + avahi (mDNS discovery).
    nice -n 19 ionice -c3 apt-get -y -o needrestart::mode=l install uxplay gstreamer1.0-plugins-bad avahi-daemon || echo 'WARN: uxplay not available in this suite (AirPlay mode will 400 until installed)'
    nice -n 19 ionice -c3 apt-get -y -o needrestart::mode=l install moonlight-qt || echo 'WARN: moonlight-qt not in apt (add the Moonlight repo, or skip; Moonlight mode will 400)'
    # display=kms modes (uxplay -vs kmssink, mpv --vo=drm) run as ROOT for DRM master,
    # so root's GStreamer registry must list the freshly-installed plugins — a stale
    # /root/.cache fails with \"plugin not found\". Clear it so it rebuilds on first use.
    rm -rf /root/.cache/gstreamer-1.0 2>/dev/null || true
    systemctl enable --now avahi-daemon 2>/dev/null || true
    systemctl enable --now seatd; sleep 1
    id $SEATUSER >/dev/null 2>&1 || useradd -m -s /bin/bash $SEATUSER
    usermod -aG video,render,input $SEATUSER
    G=\$(stat -c %G /run/seatd.sock 2>/dev/null || echo _seatd)
    usermod -aG \"\$G\" $SEATUSER && echo \"added $SEATUSER to groups video,render,input,\$G\"
    loginctl enable-linger $SEATUSER && echo \"linger on for $SEATUSER (/run/user/\$(id -u $SEATUSER))\"
    echo '--- versions ---'
    chromium --version 2>&1 | head -1; labwc --version 2>&1 | head -1
    seatd --version 2>&1 | head -1; echo \"seatd=\$(systemctl is-active seatd)\"
    echo 'NOTE: restart the agent so the Wayland child picks up the seatd group.'"
}

install() {
  push
  kmsshot_setup || echo "   WARN: disp-kmsshot build failed (run prereqs first?); /api/screenshot falls back to CDP/scrot"
  echo ">> installing as the systemd kiosk service on $NODE"
  # Neuter the legacy kiosk launch so only the agent runs Chromium (two Chromiums
  # overload the Pi). No session backup — the test nodes are disposable.
  rsync -avz "$FS/home/$SEATUSER/.xsession" "$NODE:/home/$SEATUSER/.xsession"
  rsync -avz "$FS/home/$SEATUSER/.xinitrc"  "$NODE:/home/$SEATUSER/.xinitrc"
  rsync -avz "$FS/etc/systemd/system/sideshow-agent.service" "$NODE:/etc/systemd/system/sideshow-agent.service"
  ssh "$NODE" "chown $SEATUSER:$SEATUSER /home/$SEATUSER/.xsession /home/$SEATUSER/.xinitrc; \
               chmod 755 /home/$SEATUSER/.xsession /home/$SEATUSER/.xinitrc; \
               systemctl daemon-reload; systemctl enable sideshow-agent"
  stop_legacy
  ssh "$NODE" "systemctl start sideshow-agent; sleep 2; systemctl is-active sideshow-agent"
  auto_memcap   # cap memory on low-RAM nodes; capable machines stay unconstrained (MEMCAP=off to skip)
  echo "   webUI: http://${NODE#*@}/   (give cold Chromium ~30s to attach CDP)"
}

plymouth_setup() {
  # Install the sideshow Plymouth theme and arm the boot splash. Separate from
  # `install` because it edits cmdline.txt/config.txt and rebuilds the initramfs
  # (slow + boot-affecting). Needs `prereqs` (plymouth, plymouth-themes) first.
  # After this, the agent's /api/plymouth can toggle the splash + swap the image
  # + change the message at runtime. Takes effect on the NEXT boot.
  echo ">> installing the sideshow Plymouth theme on $NODE"
  ssh "$NODE" "mkdir -p /usr/share/plymouth/themes/sideshow"
  rsync -avz "$HERE/assets/plymouth/sideshow/" "$NODE:/usr/share/plymouth/themes/sideshow/"
  ssh "$NODE" "set -e
    command -v plymouth >/dev/null || { echo 'ERROR: plymouth not installed — run prereqs first'; exit 1; }
    plymouth-set-default-theme sideshow
    # RPi: build an initramfs and let the bootloader load it; Debian uses initramfs-tools.
    grep -q '^auto_initramfs=1' /boot/firmware/config.txt 2>/dev/null || \
      ([ -f /boot/firmware/config.txt ] && echo 'auto_initramfs=1' >> /boot/firmware/config.txt && echo 'set auto_initramfs=1')
    # Ensure 'quiet splash' are on the kernel cmdline so the splash actually shows.
    CL=/boot/firmware/cmdline.txt
    if [ -f \"\$CL\" ]; then
      grep -qw quiet  \"\$CL\" || sed -i 's/\$/ quiet/'  \"\$CL\"
      grep -qw splash \"\$CL\" || sed -i 's/\$/ splash/' \"\$CL\"
      echo \"cmdline: \$(cat \"\$CL\")\"
    fi
    update-initramfs -u 2>/dev/null || echo 'NOTE: update-initramfs not available (non-initramfs node)'
    echo 'Plymouth theme armed — reboot to see the splash.'"
}

moonlight_setup() {
  # Install Moonlight (moonlight-qt) from the official Cloudsmith apt repo. We set
  # the repo up MANUALLY (fetch the GPG key + the source line as DATA) rather than
  # piping setup.deb.sh into a shell — same result, no opaque remote-code exec.
  # The binary is 'moonlight-qt'; the agent's moonlight mode runs it as an X11
  # client (needs an Xorg session). Validated: debian/trixie/arm64 → moonlight-qt 6.1.0.
  echo ">> installing Moonlight (moonlight-qt) on $NODE via the Cloudsmith repo"
  ssh "$NODE" 'set -e
    . /etc/os-release; CN=$VERSION_CODENAME; ARCH=$(dpkg --print-architecture)
    KR=/usr/share/keyrings/moonlight-game-streaming-moonlight-qt-archive-keyring.gpg
    curl -1sLf https://dl.cloudsmith.io/public/moonlight-game-streaming/moonlight-qt/gpg.2F6AE14E1C660D44.key -o /tmp/ml.key
    gpg --dearmor -o "$KR" /tmp/ml.key; rm -f /tmp/ml.key
    curl -1sLf "https://dl.cloudsmith.io/public/moonlight-game-streaming/moonlight-qt/config.deb.txt?distro=debian&codename=$CN&arch=$ARCH&component=main" \
      -o /etc/apt/sources.list.d/moonlight-game-streaming-moonlight-qt.list
    apt-get update -qq
    nice -n 19 ionice -c3 apt-get -y -o needrestart::mode=l install moonlight-qt
    command -v moonlight-qt && echo "moonlight-qt installed" || echo "WARN: moonlight-qt not on PATH"'
}

steamlink_setup() {
  # Steam Link is awkward on Debian: the apt "steamlink" package is Raspberry Pi OS
  # only, and the Flathub Flatpak (com.valvesoftware.SteamLink) is x86_64-only. So
  # there is no clean path for arm64 Debian. This tries apt, else points at Flatpak
  # (x86_64) and prints the -steamlink-cmd to set. The steamlink mode is an X11
  # client, so the node also needs an Xorg session to actually run it.
  echo ">> installing Steam Link on $NODE (best-effort; arch-dependent)"
  ssh "$NODE" 'ARCH=$(dpkg --print-architecture)
    if nice -n 19 apt-get -y -o needrestart::mode=l install steamlink 2>/dev/null; then
      echo "installed apt steamlink ($(command -v steamlink)); default -steamlink-cmd works"
    elif [ "$ARCH" = "amd64" ]; then
      echo "no apt steamlink; on x86_64 use Flatpak:"
      echo "  apt-get install -y flatpak"
      echo "  flatpak remote-add --if-not-exists flathub https://flathub.org/repo/flathub.flatpakrepo"
      echo "  flatpak install -y flathub com.valvesoftware.SteamLink"
      echo "  then run the agent with: -steamlink-cmd \"flatpak run com.valvesoftware.SteamLink\""
    else
      echo "no Steam Link package for $ARCH on Debian (apt steamlink is RPi-OS-only; Flathub SteamLink is x86_64-only)."
    fi'
}

kmsshot_setup() {
  # Build the universal DRM/KMS screenshot helper ON the node. It links the GPU
  # stack (EGL/GLES2/GBM/libdrm) that the pure-Go agent deliberately doesn't, so
  # it's compiled against the node's own Mesa rather than cross-compiled. Needs
  # the -dev packages from `prereqs`. Installs /usr/local/bin/disp-kmsshot, which
  # the agent execs for /api/screenshot on EVERY mode (X11/Wayland/cog-KMS/console).
  echo ">> building disp-kmsshot on $NODE (universal DRM screenshot helper)"
  ssh "$NODE" "mkdir -p /usr/local/src/disp-kmsshot"
  rsync -avz "$HERE/kmsshot/kmsshot.c" "$HERE/kmsshot/Makefile" "$NODE:/usr/local/src/disp-kmsshot/"
  ssh "$NODE" "set -e
    command -v pkg-config >/dev/null 2>&1 || { echo 'ERROR: pkg-config + *-dev libs missing — run prereqs first'; exit 1; }
    cd /usr/local/src/disp-kmsshot
    make clean >/dev/null 2>&1 || true
    make
    install -m 0755 disp-kmsshot /usr/local/bin/disp-kmsshot
    echo \"installed \$(command -v disp-kmsshot)\"
    # Post-install sanity: dump the live DRM state (root + a lit CRTC required).
    /usr/local/bin/disp-kmsshot -d 2>&1 | head -8 || echo 'NOTE: -d needs root + a lit CRTC'"
}

x_own() {
  # Convert an X11 node to AGENT-OWNED X: the agent starts Xorg + matchbox itself
  # (-start-x, baked into the unit) — no display manager. Installs the minimal WM,
  # removes openbox, masks lightdm, and boots to multi-user (no DM). One supervisor
  # owns the screen → clean DRM-master handoff with the direct-KMS modes (it also
  # cleared the X↔KMS scanout wedge on disp). Run AFTER `install` (which ships the
  # -start-x unit + binary). REBOOT afterward to validate a clean cold boot.
  echo ">> converting $NODE to agent-owned X (matchbox; no lightdm/openbox)"
  ssh "$NODE" "set -e
    nice -n 19 ionice -c3 apt-get -y -o needrestart::mode=l install matchbox-window-manager xauth unclutter-xfixes
    systemctl mask --now lightdm 2>/dev/null || true   # rollback: systemctl unmask --now lightdm
    systemctl set-default multi-user.target            # rollback: set-default graphical.target
    apt-get -y remove openbox 2>/dev/null || true
    rm -f /etc/systemd/system/sideshow-agent.service.d/20-startx.conf  # any live-test override
    systemctl daemon-reload
    systemctl reenable sideshow-agent   # WantedBy is now multi-user.target — recreate the wants symlink (else it won't start at boot)
    systemctl restart sideshow-agent
    sleep 5
    echo \"agent=\$(systemctl is-active sideshow-agent) lightdm=\$(systemctl is-active lightdm) default=\$(systemctl get-default)\"
    echo 'Reboot to validate cold boot. Rollback if X fails (SSH still works): unmask+start lightdm, restore the old unit, set-default graphical.target.'"
}

MEMCAP_DROPIN=/etc/systemd/system/sideshow-agent.service.d/10-memory-lowmem.conf

# write_memcap <high> <max> [swap] — install the low-end cap drop-in on $NODE
# (persist across reboot) and apply it live, no restart. Values are systemd sizes.
write_memcap() {
  high="$1"; max="$2"; swap="${3:-infinity}"
  echo ">> capping kiosk memory on $NODE: high=$high max=$max swap=$swap"
  ssh "$NODE" "mkdir -p $(dirname $MEMCAP_DROPIN); cat > $MEMCAP_DROPIN <<EOF
# Opt-in memory guardrail for a LOW-END node (written by sideshow-deploy.sh).
# Caps the agent+browser cgroup so a runaway page OOM-kills the renderer instead
# of thrashing the box. Omit on capable machines. Needs cgroup_enable=memory.
[Service]
MemoryHigh=$high
MemoryMax=$max
MemorySwapMax=$swap
EOF
systemctl daemon-reload
systemctl set-property --runtime sideshow-agent.service MemoryHigh=$high MemoryMax=$max MemorySwapMax=$swap
systemctl show sideshow-agent.service -p MemoryHigh -p MemoryMax -p MemorySwapMax"
}

# clear_memcap — remove the cap (unconstrained).
clear_memcap() {
  echo ">> removing memory cap on $NODE (unconstrained)"
  ssh "$NODE" "rm -f $MEMCAP_DROPIN; systemctl daemon-reload; \
    systemctl set-property --runtime sideshow-agent.service MemoryHigh=infinity MemoryMax=infinity MemorySwapMax=infinity; \
    systemctl show sideshow-agent.service -p MemoryHigh -p MemoryMax -p MemorySwapMax"
}

# auto_memcap — decide a cap from $NODE's RAM, honoring the MEMCAP override:
#   MEMCAP=off            never cap;   MEMCAP="480M 560M 300M"  explicit;
#   MEMCAP=auto (default) cap only when RAM < MEMCAP_THRESHOLD_MB, proportionally.
auto_memcap() {
  case "$MEMCAP" in
    off) clear_memcap; return ;;
    auto|"") : ;;                                  # RAM-based, below
    *) set -- $MEMCAP; write_memcap "$@"; return ;; # explicit "high max [swap]"
  esac
  memtotal_kb=$(ssh "$NODE" "awk '/MemTotal/{print \$2}' /proc/meminfo")
  memtotal_mb=$(( ${memtotal_kb:-0} / 1024 ))
  if [ "$memtotal_mb" -le 0 ]; then echo ">> WARN: could not read RAM on $NODE — skipping auto memcap"; return; fi
  if [ "$memtotal_mb" -ge "$MEMCAP_THRESHOLD_MB" ]; then
    echo ">> $NODE: ${memtotal_mb}MB RAM >= ${MEMCAP_THRESHOLD_MB}MB threshold — leaving memory unconstrained"
    clear_memcap
    return
  fi
  echo ">> $NODE: ${memtotal_mb}MB RAM < ${MEMCAP_THRESHOLD_MB}MB threshold — applying proportional cap"
  write_memcap "$(( memtotal_mb * MEMCAP_HIGH_PCT / 100 ))M" \
               "$(( memtotal_mb * MEMCAP_MAX_PCT / 100 ))M" \
               "$(( memtotal_mb * MEMCAP_SWAP_PCT / 100 ))M"
}

# memcap subcommand: `memcap` (auto) | `memcap off` | `memcap <high> <max> [swap]`.
memcap() {
  if [ -z "${2:-}" ]; then auto_memcap; return; fi
  if [ "$2" = "off" ]; then clear_memcap; return; fi
  if [ -z "${3:-}" ]; then echo "usage: $0 memcap [off | <MemoryHigh> <MemoryMax> [MemorySwapMax]]"; exit 2; fi
  write_memcap "$2" "$3" "${4:-infinity}"
}

migrate() {
  # One-time cutover of an EXISTING node from the old displinux naming to sideshow.
  # The agent no longer self-migrates, so run this once when upgrading a node that
  # predates the rename: it moves /etc/displinux → /etc/sideshow and
  # /var/lib/displinux → /var/lib/sideshow, derives a sideshow-agent.service unit
  # from the live displinux-agent one (preserving THIS node's exact flags +
  # seat-user), and swaps the enabled service. Idempotent — safe to re-run (a node
  # already on sideshow just gets the binary refreshed + the service restarted).
  push   # build + rsync the new binary to /usr/local/bin/sideshow-agent
  echo ">> migrating $NODE: displinux → sideshow (paths + systemd unit)"
  ssh "$NODE" 'set -e
    systemctl stop displinux-agent 2>/dev/null || true
    # Move config + state with the old agent stopped; never clobber an existing new dir.
    { [ -d /etc/displinux ] && [ ! -e /etc/sideshow ] && mv /etc/displinux /etc/sideshow && echo "moved /etc/displinux → /etc/sideshow"; } || true
    { [ -d /var/lib/displinux ] && [ ! -e /var/lib/sideshow ] && mv /var/lib/displinux /var/lib/sideshow && echo "moved /var/lib/displinux → /var/lib/sideshow"; } || true
    # Derive the sideshow unit from the live displinux unit: swap ONLY the binary
    # path + cosmetic name, so per-node flags (incl. -seat-user displinux on the
    # Wayland box) survive untouched.
    if [ -f /etc/systemd/system/displinux-agent.service ] && [ ! -f /etc/systemd/system/sideshow-agent.service ]; then
      sed -e "s#/usr/local/bin/displinux-agent#/usr/local/bin/sideshow-agent#g" \
          -e "s/displinux node agent/sideshow node agent/" \
          /etc/systemd/system/displinux-agent.service > /etc/systemd/system/sideshow-agent.service
      if [ -d /etc/systemd/system/displinux-agent.service.d ]; then
        mkdir -p /etc/systemd/system/sideshow-agent.service.d
        cp -a /etc/systemd/system/displinux-agent.service.d/. /etc/systemd/system/sideshow-agent.service.d/
      fi
    fi
    systemctl disable displinux-agent 2>/dev/null || true
    # No rollback kept: delete the old unit + drop-in so nothing displinux-named
    # lingers as a migration artifact (the sideshow unit is already derived above).
    rm -f /etc/systemd/system/displinux-agent.service
    rm -rf /etc/systemd/system/displinux-agent.service.d
    systemctl daemon-reload
    systemctl enable sideshow-agent
    systemctl restart sideshow-agent
    sleep 3
    echo "sideshow-agent=$(systemctl is-active sideshow-agent)"'
  echo "   migrated. webUI: http://${NODE#*@}/"
}

case "${1:-}" in
  build) build ;;
  push) push ;;
  stop-legacy) stop_legacy ;;
  run) run ;;
  deploy) deploy ;;
  migrate) migrate ;;
  prereqs) prereqs ;;
  install) install ;;
  memcap) memcap "$@" ;;
  plymouth) plymouth_setup ;;
  moonlight) moonlight_setup ;;
  steamlink) steamlink_setup ;;
  kmsshot) kmsshot_setup ;;
  xown) x_own ;;
  *) echo "usage: $0 {build|push|stop-legacy|run|deploy|migrate|prereqs|install|memcap|plymouth|moonlight|steamlink|kmsshot|xown}"; exit 2 ;;
esac
