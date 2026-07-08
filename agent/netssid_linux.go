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

// iwPoint is the leading member of the kernel's `union iwreq_data` that
// SIOCGIWESSID uses: a userspace buffer pointer plus its length and flags.
type iwPoint struct {
	pointer uintptr
	length  uint16
	flags   uint16
}

// iwreq mirrors the kernel `struct iwreq`: a 16-byte interface name followed by
// the `union iwreq_data`. The union is fixed at 16 bytes (its largest member is a
// 16-byte sockaddr), and we overlay iwPoint on its start — so struct iwreq is 32
// bytes on EVERY arch, and [2]uint64 also forces the 8-byte alignment the pointer
// needs on 64-bit. A bare {pointer,length,flags} would be only 24 bytes on 32-bit
// (armhf, a released build): SIOCGIWESSID's copy_to_user(sizeof(struct iwreq)=32)
// would then overrun the following SSID buffer and corrupt the result there.
type iwreq struct {
	name [ifNameSize]byte
	data [2]uint64 // union iwreq_data (16 bytes); only the leading iwPoint is used
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

// Compile-time guard: struct iwreq must be exactly 32 bytes on EVERY arch (incl.
// 32-bit armhf), or SIOCGIWESSID's copy_to_user(sizeof(struct iwreq)) overruns buf.
// These two constants fail to compile if the size drifts either way.
const (
	_ = uint(unsafe.Sizeof(iwreq{}) - 32)
	_ = uint(32 - unsafe.Sizeof(iwreq{}))
)

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
	pt := (*iwPoint)(unsafe.Pointer(&q.req.data)) // overlay the iw_point on the union
	pt.pointer = uintptr(unsafe.Pointer(&q.buf[0]))
	pt.length = iwEssidMaxSize

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), siocgiwessid, uintptr(unsafe.Pointer(&q.req)))
	runtime.KeepAlive(&q) // the kernel wrote into q.buf via the overlaid pointer
	if errno != 0 {
		return ""
	}
	n := int(pt.length) // the kernel wrote the SSID length back into the iw_point
	if n > len(q.buf) {
		n = len(q.buf)
	}
	return string(bytes.TrimRight(q.buf[:n], "\x00"))
}
