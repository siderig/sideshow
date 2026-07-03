#!/bin/sh
# Generic Wayland kiosk launcher: labwc compositor + Chromium as a Wayland client
# on a dedicated VT. The agent (-wayland-launcher) execs this as the seat user; it
# chvt's to the Wayland VT for the DRM-master handoff. User-agnostic:
# XDG_RUNTIME_DIR is derived from `id -u`, so the same script serves any seat user.
#
# Seat backend (set by the agent via env):
#   - seatd  (DEFAULT, run as the seat USER): wlroots uses its GLES2 GPU renderer —
#     the GPU-accelerated path. Needs seatd running + the user in the seatd group.
#   - builtin (run as ROOT, -wayland-root): libseat opens DRM directly; pair with
#     WLR_RENDERER=pixman on weak GPUs.
set -eu

export DISP_URL="${1:-about:blank}"
export DISP_CDP="${CDP_PORT:-9223}"                       # separate from the X kiosk's 9222
export DISP_PROFILE="${PROFILE:-/tmp/sideshow-wl-chromium}"
export DISP_BG="${DISP_BG:-ff000000}"                     # base bg painted before a page loads (kills the white flash)
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
mkdir -p "$XDG_RUNTIME_DIR" 2>/dev/null || true
chmod 700 "$XDG_RUNTIME_DIR" 2>/dev/null || true
LIBSEAT_BACKEND="${LIBSEAT_BACKEND:-seatd}"; export LIBSEAT_BACKEND

# Hand labwc's -s a FIXED-PATH wrapper rather than a command string with the URL
# embedded: labwc's -s parsing never sees the URL (so a crafted URL can't inject
# shell), and the wrapper's own shell expands DISP_* into Chromium's argv as single
# double-quoted args (inert). --no-sandbox is added ONLY when running as root (the
# legacy -wayland-root path); the seatd/user path keeps the sandbox + GPU.
WRAP="${XDG_RUNTIME_DIR}/sideshow-wl-chromium.sh"
cat > "$WRAP" <<'WRAPEOF'
#!/bin/sh
SANDBOX=""; [ "$(id -u)" = 0 ] && SANDBOX="--no-sandbox"
BG=""; [ -n "$DISP_BG" ] && BG="--default-background-color=$DISP_BG"
exec chromium --ozone-platform=wayland --kiosk $SANDBOX --no-first-run --no-default-browser-check \
  --hide-scrollbars --autoplay-policy=no-user-gesture-required $BG \
  --remote-debugging-address=127.0.0.1 --remote-debugging-port="$DISP_CDP" \
  --user-data-dir="$DISP_PROFILE" "$DISP_URL"
WRAPEOF
chmod 755 "$WRAP"

# SIDESHOW_LABWC_CONFIG (set by the agent under -lock-input) points labwc at an
# agent-owned config dir whose rc.xml strips window-switch/close/menu keybinds;
# unset, labwc uses its stock config (the node's own ~/.config/labwc is untouched).
exec env LIBSEAT_BACKEND="$LIBSEAT_BACKEND" ${WLR_RENDERER:+WLR_RENDERER="$WLR_RENDERER"} \
  dbus-run-session -- labwc ${SIDESHOW_LABWC_CONFIG:+-C "$SIDESHOW_LABWC_CONFIG"} -s "$WRAP"
