//go:build linux

package main

import (
	"bytes"
	"runtime"
	"syscall"
	"unsafe"
)

// SIOCGIWESSID is the wireless-extensions ioctl that reads the SSID an interface
// is associated with (what `iwgetid -r` uses). IW_ESSID_MAX_SIZE bounds the name.
const (
	siocgiwessid   = 0x8B1B
	iwEssidMaxSize = 32
	ifNameSize     = 16
)

// iwreq mirrors the kernel `struct iwreq` for the ESSID query on 64-bit: a
// 16-byte interface name followed by `struct iw_point { void *pointer; __u16
// length; __u16 flags; }`. Both node arches (arm64, amd64) are 64-bit
// little-endian, so this single layout is correct for the agent's targets; the
// pointer field lands at the 8-aligned offset 16, matching the C union.
type iwreq struct {
	name    [ifNameSize]byte
	pointer uintptr
	length  uint16
	flags   uint16
}

// essidQuery bundles the request with the SSID buffer it points at in one struct,
// so the buffer lives inside the same object the compiler pins across the ioctl
// (via the &q.req conversion in the Syscall argument list). That keeps req.pointer
// — a bare uintptr the stack copier can't fix up — pointing at memory that is
// guaranteed not to move for the duration of the call.
type essidQuery struct {
	req iwreq
	buf [iwEssidMaxSize + 1]byte
}

// wifiSSID returns the SSID the wireless interface is associated with, or "" if
// it is not associated or the driver has no WEXT-compat for this get. It is a
// single ioctl (a syscall, not a subprocess), so it is cheap enough to run on
// the snapshot poll. cfg80211 keeps this compat get on the drivers our nodes use
// (brcmfmac on the Pi, iwlwifi/ath on x86); anywhere it is missing the ioctl
// fails and we degrade to an empty SSID.
func wifiSSID(iface string) string {
	if iface == "" || len(iface) >= ifNameSize {
		return ""
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return ""
	}
	defer syscall.Close(fd)

	var q essidQuery
	copy(q.req.name[:], iface)
	q.req.pointer = uintptr(unsafe.Pointer(&q.buf[0]))
	q.req.length = iwEssidMaxSize

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), siocgiwessid, uintptr(unsafe.Pointer(&q.req)))
	runtime.KeepAlive(&q) // the kernel wrote into q.buf via q.req.pointer
	if errno != 0 {
		return ""
	}
	n := int(q.req.length)
	if n > len(q.buf) {
		n = len(q.buf)
	}
	return string(bytes.TrimRight(q.buf[:n], "\x00"))
}
