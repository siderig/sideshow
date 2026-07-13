#!/bin/sh
# Provision a target rootfs into a sideshow display-node appliance. Runs AS ROOT
# INSIDE the image being built — the Pi OS arm64 rootfs (arm-runner-action's qemu
# chroot) or the Debian amd64 rootfs (debos). NOT for a live node: that is
# install.sh (hosted one-liner) / sideshow-deploy.sh (maintainer deploy).
#
# It installs deps, the prebuilt agent, a rootfs-compiled disp-kmsshot, the seat
# user, the Plymouth theme, the systemd unit, and a first-boot service that mints a
# UNIQUE per-node auth key (never baked into the image). The node boots straight
# into the agent-owned display; the on-screen setup wizard runs on a fresh node.
#
# Inputs (env):
#   COMPOSITOR  x11 | wayland   x11 = Xorg+matchbox (weak GPU/Pi); wayland = labwc+seatd (capable GPU)
#   SEATUSER    seat user to create (default sideshow)
#   AGENT_BIN   path to the prebuilt sideshow-agent binary (already in the rootfs)
#   SRC_DIR     the repo's agent/ dir (holds kmsshot/, assets/, scripts/{run-wayland.sh,firstboot/})
set -eu

COMPOSITOR="${COMPOSITOR:-x11}"
SEATUSER="${SEATUSER:-sideshow}"
AGENT_BIN="${AGENT_BIN:-/tmp/sideshow/sideshow-agent}"
SRC_DIR="${SRC_DIR:-/tmp/sideshow}"
export DEBIAN_FRONTEND=noninteractive
echo ">> [provision] compositor=$COMPOSITOR seat-user=$SEATUSER"

# --- packages ---------------------------------------------------------------
# CORE must succeed (a broken core = an unusable image); OPTIONAL is best-effort
# per package (a suite may lack uxplay/wayvnc — the matching mode just 400s).
# Chromium is installed separately: the package is `chromium` on Debian but
# `chromium-browser` on Raspberry Pi OS.
CORE="ca-certificates curl plymouth plymouth-themes network-manager \
  cloud-guest-utils e2fsprogs unattended-upgrades \
  gcc make pkg-config libdrm-dev libegl-dev libgles-dev libgbm-dev"
if [ "$COMPOSITOR" = wayland ]; then
  CORE="$CORE labwc seatd wlr-randr dbus-user-session"
  OPTIONAL="mpv scrot uxplay gstreamer1.0-plugins-bad avahi-daemon wayvnc grim \
    xdg-desktop-portal xdg-desktop-portal-wlr gsettings-desktop-schemas adwaita-icon-theme"
else
  CORE="$CORE xserver-xorg xinit xauth matchbox-window-manager"
  OPTIONAL="mpv scrot uxplay gstreamer1.0-plugins-bad avahi-daemon x11vnc unclutter-xfixes \
    xdg-desktop-portal xdg-desktop-portal-gtk gsettings-desktop-schemas xsettingsd adwaita-icon-theme"
fi

apt-get update
# shellcheck disable=SC2086
apt-get install -y --no-install-recommends $CORE
# Chromium: package name differs by base (Debian `chromium` / Pi OS `chromium-browser`).
apt-get install -y --no-install-recommends chromium \
  || apt-get install -y --no-install-recommends chromium-browser \
  || { echo "FATAL: no chromium package available"; exit 1; }
for p in $OPTIONAL; do
  apt-get install -y --no-install-recommends "$p" || echo "   WARN: '$p' unavailable in this suite (its feature will be degraded)"
done

# --- Tailscale (preinstalled, opt-in) ---------------------------------------
# Ship the tailscale CLI + daemon so an operator can CHOOSE to join a tailnet
# from the setup wizard or Settings (encrypted remote access + an optional real
# ts.net HTTPS cert). It is installed LOGGED OUT — the node never joins anything
# on its own. A pre-auth key staged at /etc/sideshow/tailscale.authkey (dropped at
# flash time) is consumed and shredded by the agent on first boot; without one it
# just stays idle. Its own apt repo is added by the official installer.
if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sh \
    || echo "   WARN: tailscale install failed — the tailnet option stays unavailable until installed"
fi

# --- comitup (headless Wi-Fi onboarding, opt-in via -comitup) ----------------
# Recovery AP for a node that boots with NO network: comitup raises a Wi-Fi AP +
# captive portal so the operator can join the node to a Wi-Fi network without a
# screen or keyboard. Not in the base repos — add davesteele's apt source (a .deb
# that drops the repo + signing key), then install comitup. Best-effort + ISOLATED
# in a subshell: a failure here (offline builder, repo down) must NEVER fail the
# core image. comitup drives NetworkManager (already installed) and stays dormant
# while the node has a connection, only raising the AP when it is disconnected.
if ! command -v comitup >/dev/null 2>&1; then
  ( set -e
    deb=/tmp/davesteele-comitup-apt-source.deb
    curl -fsSL -o "$deb" https://davesteele.github.io/comitup/latest/davesteele-comitup-apt-source_latest.deb
    apt-get install -y "$deb"
    apt-get update
    apt-get install -y --no-install-recommends comitup
    rm -f "$deb"
  ) && echo ">> comitup installed (headless recovery AP)" \
    || echo "   WARN: comitup install failed — the -comitup recovery AP stays unavailable"
fi
# display=kms modes run as root; clear root's stale GStreamer registry so the
# freshly-installed plugins (kmssink) are found on first use.
rm -rf /root/.cache/gstreamer-1.0 2>/dev/null || true

# --- agent binary -----------------------------------------------------------
install -m 0755 "$AGENT_BIN" /usr/local/bin/sideshow-agent
echo ">> installed sideshow-agent: $(/usr/local/bin/sideshow-agent -h 2>&1 | head -1 || true)"

# --- disp-kmsshot: compiled in the rootfs against its own Mesa ---------------
if [ -f "$SRC_DIR/kmsshot/kmsshot.c" ]; then
  if ( cd "$SRC_DIR/kmsshot" && make clean >/dev/null 2>&1 || true; make ); then
    install -m 0755 "$SRC_DIR/kmsshot/disp-kmsshot" /usr/local/bin/disp-kmsshot
    echo ">> built disp-kmsshot"
  else
    echo "   WARN: disp-kmsshot build failed — /api/screenshot falls back to CDP/scrot/grim"
  fi
fi

# --- seat user (+ groups, chroot-safe linger via the marker file) -----------
id "$SEATUSER" >/dev/null 2>&1 || useradd -m -s /bin/bash "$SEATUSER"
for g in video render input; do getent group "$g" >/dev/null 2>&1 && usermod -aG "$g" "$SEATUSER" || true; done
mkdir -p /var/lib/systemd/linger && : > "/var/lib/systemd/linger/$SEATUSER"
if [ "$COMPOSITOR" = wayland ]; then
  getent group _seatd >/dev/null 2>&1 && usermod -aG _seatd "$SEATUSER" || true
  install -m 0755 "$SRC_DIR/scripts/run-wayland.sh" "/home/$SEATUSER/run-wayland.sh"
  chown "$SEATUSER:$SEATUSER" "/home/$SEATUSER/run-wayland.sh"
fi

# --- Plymouth boot splash ---------------------------------------------------
if [ -d "$SRC_DIR/assets/plymouth/sideshow" ]; then
  mkdir -p /usr/share/plymouth/themes/sideshow
  cp -a "$SRC_DIR/assets/plymouth/sideshow/." /usr/share/plymouth/themes/sideshow/
  plymouth-set-default-theme sideshow 2>/dev/null || true
fi

# --- systemd unit (compositor-specific flags) -------------------------------
# -init-auth-key: the agent mints a UNIQUE per-node key at /etc/sideshow/agent.key
# on first run if it is missing — so the image ships no shared secret (and needs no
# separate first-boot service). -auto-hostname renames a stock-default hostname.
SEATUID="$(id -u "$SEATUSER" 2>/dev/null || echo 1000)"
if [ "$COMPOSITOR" = wayland ]; then
  EXECFLAGS="-addr :80 -seat-user $SEATUSER -auth-key-file /etc/sideshow/agent.key -init-auth-key -start-mode wayland -wayland-launcher /home/$SEATUSER/run-wayland.sh -auto-hostname -comitup"
  AFTER="network-online.target seatd.service systemd-user-sessions.service"
  # labwc-as-seat-user needs the seat user's XDG_RUNTIME_DIR (/run/user/$SEATUID),
  # created by logind for the lingering user — wait so a cold boot can't race it.
  PRE="ExecStartPre=/bin/sh -c 'i=0; while [ ! -d /run/user/$SEATUID ] && [ \$i -lt 30 ]; do i=\$((i+1)); sleep 1; done'"
else
  EXECFLAGS="-addr :80 -seat-user $SEATUSER -auth-key-file /etc/sideshow/agent.key -init-auth-key -start-x -auto-hostname -comitup"
  AFTER="network-online.target systemd-user-sessions.service"
  PRE=""
fi
{
  echo "[Unit]"
  echo "Description=Sideshow display-node agent"
  echo "After=$AFTER"
  echo "Wants=network-online.target"
  echo ""
  echo "[Service]"
  echo "Type=simple"
  [ -n "$PRE" ] && echo "$PRE"
  echo "ExecStart=/usr/local/bin/sideshow-agent $EXECFLAGS"
  echo "Restart=always"
  echo "RestartSec=2"
  echo ""
  echo "[Install]"
  echo "WantedBy=multi-user.target"
} > /etc/systemd/system/sideshow-agent.service

# --- first-boot: expand the root fs to fill the flashed media ---------------
install -d /usr/local/lib/sideshow
install -m 0755 "$SRC_DIR/scripts/firstboot/sideshow-expand-rootfs.sh" /usr/local/lib/sideshow/sideshow-expand-rootfs.sh
install -m 0644 "$SRC_DIR/scripts/firstboot/sideshow-expand-rootfs.service" /etc/systemd/system/sideshow-expand-rootfs.service

# --- unattended SECURITY upgrades -------------------------------------------
# Turn the mechanism on (the distro's own 50unattended-upgrades already scopes it
# to the -security origin, correct for both Debian and Raspberry Pi OS — we don't
# touch origins). The drop-in below only sets appliance policy.
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
cat > /etc/apt/apt.conf.d/52sideshow-unattended.conf <<'EOF'
// Sideshow appliance policy for unattended-upgrades (security origins come from
// the distro default 50unattended-upgrades). Userspace security fixes apply
// automatically. Auto-reboot is OFF by default so the display never reboots
// unexpectedly — kernel/libc fixes then wait for the next reboot. For a truly
// hands-off node, uncomment the three lines below to reboot (only when an update
// needs it) at a quiet hour; the agent restores its display on boot.
Unattended-Upgrade::Remove-Unused-Dependencies "true";
Unattended-Upgrade::Automatic-Reboot "false";
//Unattended-Upgrade::Automatic-Reboot "true";
//Unattended-Upgrade::Automatic-Reboot-WithUsers "true";
//Unattended-Upgrade::Automatic-Reboot-Time "03:30";
EOF

# --- boot into the appliance ------------------------------------------------
systemctl set-default multi-user.target 2>/dev/null || true
systemctl enable sideshow-expand-rootfs.service 2>/dev/null || true
systemctl enable sideshow-agent.service 2>/dev/null || true
systemctl enable NetworkManager.service 2>/dev/null || true
# tailscaled ready-but-idle (logged out) so the opt-in tailnet join works instantly.
command -v tailscale >/dev/null 2>&1 && systemctl enable tailscaled.service 2>/dev/null || true
[ "$COMPOSITOR" = wayland ] && systemctl enable seatd.service 2>/dev/null || true
command -v systemctl >/dev/null 2>&1 && systemctl enable avahi-daemon.service 2>/dev/null || true
# unattended-upgrades runs off the apt-daily timers (staggered by a randomized delay).
systemctl enable apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true

echo ">> [provision] done"
