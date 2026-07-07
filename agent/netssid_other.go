//go:build !linux

package main

// wifiSSID is a no-op off Linux (the WEXT ioctl and /proc/net/wireless are
// Linux-only); the agent runs on the nodes, and the host build only needs to
// compile + test the pure parsers.
func wifiSSID(string) string { return "" }
