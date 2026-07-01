#!/bin/sh
# Sideshow one-line installer — bring the agent up on a fresh Debian/Raspbian node,
# then finish in the browser via the webUI setup wizard (GET /setup).
#
# HOSTED one-liner (once a release is published — the base URL is baked in below,
# or override it): auto-detects the arch, downloads the matching prebuilt binary,
# and verifies it against the release SHA256SUMS.
#
#   curl -fsSL https://<host>/install.sh | sudo sh
#   # or point at any release host:
#   curl -fsSL https://<host>/install.sh | sudo SIDESHOW_BASE_URL=https://<host> sh
#
# Other binary sources (dev / self-host):
#   sudo SIDESHOW_BINARY=./dist/sideshow-agent-arm64 sh scripts/install.sh   # a local build
#   sudo SIDESHOW_BINARY_URL=https://ex/sideshow-agent-arm64 [SIDESHOW_BINARY_SHA256=…] sh scripts/install.sh
#
# Precedence: SIDESHOW_BINARY (local) > SIDESHOW_BINARY_URL (single file) >
# SIDESHOW_BASE_URL (auto arch + SHA256SUMS verify). Set SIDESHOW_SKIP_VERIFY=1 to
# skip the checksum check (not recommended). Build the release with
# scripts/build-release.sh.
#
# What it does: install the binary → create the seat user → generate an auth key →
# write + enable the systemd unit → start it → print the setup URL(s). It does NOT
# install a compositor or flip the boot target — do that from the /setup wizard
# (feature packages) and, for an X11 kiosk, the deploy script's `x_own` step.
set -eu

BIN=/usr/local/bin/sideshow-agent
KEYFILE=/etc/sideshow/agent.key
UNIT=/etc/systemd/system/sideshow-agent.service
SEATUSER="${SEATUSER:-sideshow}"
ADDR="${ADDR:-:80}"
AGENT_ARGS="${SIDESHOW_AGENT_ARGS:-}"
# Baked default release host — build-release.sh / the release process rewrites this
# placeholder, or override it with SIDESHOW_BASE_URL. While it is the placeholder,
# the hosted path is disabled and you must pass SIDESHOW_BINARY/_URL.
BASE_URL="${SIDESHOW_BASE_URL:-https://github.com/siderig/sideshow/releases/latest/download}"

[ "$(id -u)" = "0" ] || { echo "ERROR: run as root (sudo)."; exit 1; }

arch="$(dpkg --print-architecture 2>/dev/null || uname -m)"
case "$arch" in
  arm64|aarch64)       asset=arm64 ;;
  amd64|x86_64)        asset=amd64 ;;
  armhf|armv7l|armv6l) asset=armhf ;;
  *) echo "ERROR: unsupported arch '$arch' (need arm64/amd64/armhf)"; exit 1 ;;
esac
echo ">> Sideshow installer — arch=$arch (asset=$asset) addr=$ADDR seat-user=$SEATUSER"

# --- obtain the binary -------------------------------------------------------
tmp="$(mktemp)"; sums="$(mktemp)"
trap 'rm -f "$tmp" "$sums"' EXIT
if [ -n "${SIDESHOW_BINARY:-}" ]; then
  echo ">> using local binary: $SIDESHOW_BINARY"
  cp "$SIDESHOW_BINARY" "$tmp"
elif [ -n "${SIDESHOW_BINARY_URL:-}" ]; then
  echo ">> downloading: $SIDESHOW_BINARY_URL"
  curl -fsSL "$SIDESHOW_BINARY_URL" -o "$tmp"
elif [ "$BASE_URL" != "__SIDESHOW_BASE_URL__" ] && [ -n "$BASE_URL" ]; then
  echo ">> downloading sideshow-agent-$asset from $BASE_URL"
  curl -fsSL "$BASE_URL/sideshow-agent-$asset" -o "$tmp"
  if [ "${SIDESHOW_SKIP_VERIFY:-0}" != "1" ]; then
    echo ">> verifying against $BASE_URL/SHA256SUMS"
    curl -fsSL "$BASE_URL/SHA256SUMS" -o "$sums"
    want="$(awk -v f="sideshow-agent-$asset" '$2==f || $2=="*"f {print $1}' "$sums" | head -1)"
    [ -n "$want" ] || { echo "ERROR: no SHA256SUMS entry for sideshow-agent-$asset"; exit 1; }
    got="$(sha256sum "$tmp" | awk '{print $1}')"
    [ "$want" = "$got" ] || { echo "ERROR: checksum mismatch (want $want, got $got)"; exit 1; }
    echo ">> checksum OK"
  fi
else
  echo "ERROR: no binary source. Set one of:"
  echo "   SIDESHOW_BASE_URL=<release-host>   (auto-picks sideshow-agent-$asset + verifies SHA256SUMS)"
  echo "   SIDESHOW_BINARY_URL=<direct-url>   (a single binary; add SIDESHOW_BINARY_SHA256 to verify)"
  echo "   SIDESHOW_BINARY=<local-path>       (build: GOOS=linux GOARCH=$asset CGO_ENABLED=0 go build -o sideshow-agent .)"
  exit 1
fi

# Optional explicit single-file checksum (for the SIDESHOW_BINARY_URL path).
if [ -n "${SIDESHOW_BINARY_SHA256:-}" ]; then
  echo ">> verifying sha256"
  echo "$SIDESHOW_BINARY_SHA256  $tmp" | sha256sum -c - >/dev/null || { echo "ERROR: checksum mismatch"; exit 1; }
fi

install -m 0755 "$tmp" "$BIN"
echo ">> installed $BIN ($("$BIN" -h 2>&1 | head -1 || true))"

# --- seat user ---------------------------------------------------------------
# The agent resolves + priv-drops to the seat user at startup (Config.resolve),
# so it MUST exist before the unit starts or the agent fatally fails to boot and
# crash-loops — leaving /setup unreachable. Create it here (mirroring the deploy
# script's base setup); the seatd group + a compositor arrive later via the
# wizard, then a restart.
if ! id "$SEATUSER" >/dev/null 2>&1; then
  echo ">> creating seat user $SEATUSER"
  useradd -m -s /bin/bash "$SEATUSER"
fi
for g in video render input; do
  getent group "$g" >/dev/null 2>&1 && usermod -aG "$g" "$SEATUSER" 2>/dev/null || true
done
loginctl enable-linger "$SEATUSER" >/dev/null 2>&1 || true

# --- auth key (generate once) ------------------------------------------------
mkdir -p "$(dirname "$KEYFILE")"
chmod 700 "$(dirname "$KEYFILE")" 2>/dev/null || true
if [ ! -s "$KEYFILE" ]; then
  # Generate under a tight umask so the key is NEVER world-readable, not even in
  # the window before chmod (the file is created by the redirect, not chmod).
  ( umask 077
    if command -v openssl >/dev/null 2>&1; then
      openssl rand -base64 24 | tr -d '\n' > "$KEYFILE"
    else
      head -c 24 /dev/urandom | base64 | tr -d '\n' > "$KEYFILE"
    fi )
  chmod 600 "$KEYFILE"
  echo ">> generated auth key at $KEYFILE"
else
  echo ">> keeping existing auth key at $KEYFILE"
fi

# --- systemd unit ------------------------------------------------------------
# Minimal, compositor-agnostic: the agent + webUI come up on $ADDR immediately so
# the /setup wizard is reachable. Add -start-x / Wayland flags after the wizard
# installs a compositor (see docs). Restart=always keeps it as a supervisor.
cat > "$UNIT" <<EOF
[Unit]
Description=Sideshow display-node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN -addr $ADDR -seat-user $SEATUSER -auth-key-file $KEYFILE $AGENT_ARGS
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable sideshow-agent >/dev/null 2>&1 || true
systemctl restart sideshow-agent
sleep 2
state="$(systemctl is-active sideshow-agent || true)"
echo ">> sideshow-agent: $state"

# --- print setup URLs --------------------------------------------------------
port="${ADDR##*:}"; [ "$port" = "$ADDR" ] && port=80
suffix=""; [ "$port" != "80" ] && suffix=":$port"
# Detect the LAN IP(s) via net-tools, else fall back to iproute2 (present on
# minimal Debian where net-tools `hostname -I` is not).
ips="$(hostname -I 2>/dev/null || true)"
[ -n "$ips" ] || ips="$(ip -o -4 addr show scope global 2>/dev/null | awk '{split($4,a,"/"); print a[1]}' || true)"
echo ""
echo "=============================================================="
echo " Sideshow is running. Finish setup in a browser:"
if [ -n "$ips" ]; then
  for ip in $ips; do echo "   http://$ip$suffix/setup"; done
else
  echo "   http://<this-node-ip>$suffix/setup   (could not detect IP; run: ip -4 addr)"
fi
echo "   (auth key: $KEYFILE — needed once setup is finished)"
echo "=============================================================="
