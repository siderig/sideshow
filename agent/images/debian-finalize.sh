#!/bin/sh
# debos chroot hook: finalize the Debian sideshow image after provisioning.
# Generic hostname 'debian' → the agent's -auto-hostname renames it to
# sideshow-<serial4> on first boot. Sets a default locale and removes the source
# tree + apt lists so they don't bloat the shipped image.
set -eu

echo "debian" > /etc/hostname
printf '127.0.1.1\tdebian\n' >> /etc/hosts

# a minimal default locale (avoids noisy perl locale warnings)
echo "en_US.UTF-8 UTF-8" > /etc/locale.gen
locale-gen 2>/dev/null || true
echo "LANG=en_US.UTF-8" > /etc/default/locale

# drop the build-time source tree + apt cache (not needed on the node)
rm -rf /opt/sideshow-src
apt-get clean 2>/dev/null || true
rm -rf /var/lib/apt/lists/* 2>/dev/null || true
