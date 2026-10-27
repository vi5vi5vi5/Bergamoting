//go:build linux

// Package clock — монотонные часы в тех же единицах, что trigger_time в /proc/asound: CLOCK_MONOTONIC.
package clock

import (
	"syscall"
	"unsafe"
)

// Mono возвращает CLOCK_MONOTONIC в наносекундах.
func Mono() int64 {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0)
	return ts.Nano()
}
