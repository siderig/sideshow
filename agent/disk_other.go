//go:build !linux

package main

// readDisk is a no-op off Linux (dev builds on macOS); the production target is
// linux/arm64, where disk_linux.go provides the real statfs-backed reading.
func readDisk(string) DiskStats { return DiskStats{} }
