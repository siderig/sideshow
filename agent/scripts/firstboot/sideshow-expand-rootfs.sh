#!/bin/sh
# First-boot: grow the ROOT partition + filesystem to fill the media the image was
# flashed onto (SD card / eMMC / SSD / USB), like Pi OS init_resize / Armbian /
# cloud-init growpart. Only ever touches the root's OWN disk + partition (derived
# from where / is mounted), and is idempotent — a no-op once the partition already
# fills the device. Online ext4 resize works on the mounted root, so no reboot.
set -eu

root_part="$(findmnt -no SOURCE / 2>/dev/null || true)"   # e.g. /dev/mmcblk0p2, /dev/sda2, /dev/nvme0n1p2
case "$root_part" in
  /dev/*) : ;;
  *) echo "expand-rootfs: root is not a plain block device ($root_part) — skipping"; exit 0 ;;
esac

part_base="${root_part#/dev/}"
disk="$(lsblk -no pkname "$root_part" 2>/dev/null || true)"          # parent disk, e.g. mmcblk0 / sda / nvme0n1
partnum="$(cat "/sys/class/block/$part_base/partition" 2>/dev/null || true)"
if [ -z "$disk" ] || [ -z "$partnum" ]; then
  echo "expand-rootfs: could not resolve disk/partition for $root_part — skipping"
  exit 0
fi

# Grow the partition to the end of the disk. growpart exits non-zero (NOCHANGE)
# when it already fills the device — tolerate it; then grow the fs (a no-op when
# the fs already fills the partition).
if growpart "/dev/$disk" "$partnum"; then
  echo "expand-rootfs: grew /dev/$disk partition $partnum"
else
  echo "expand-rootfs: partition already fills the disk (or growpart declined)"
fi
resize2fs "$root_part" || echo "expand-rootfs: resize2fs skipped/failed (already full?)"
