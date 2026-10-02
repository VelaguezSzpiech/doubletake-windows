//go:build windows

package airplay

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var queryInterruptTimePrecise = windows.NewLazySystemDLL("api-ms-win-core-realtime-l1-1-1.dll").NewProc("QueryInterruptTimePrecise")

// bootRelativeNow returns the system monotonic boot clock, including suspend,
// matching Linux CLOCK_BOOTTIME rather than time since process initialization.
// QueryInterruptTimePrecise (Windows 10 / Server 2016+) returns 100ns units and
// has no error return; a missing API is not hidden by a process-relative fallback.
// https://learn.microsoft.com/en-us/windows/win32/api/realtimeapiset/nf-realtimeapiset-queryinterrupttimeprecise
func bootRelativeNow() time.Duration {
	var ticks uint64
	queryInterruptTimePrecise.Call(uintptr(unsafe.Pointer(&ticks)))
	return time.Duration(ticks) * 100 * time.Nanosecond
}
