//go:build windows

package airplay

import (
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsBootClockUsesSystemBootDomain(t *testing.T) {
	queryInterruptTime := windows.NewLazySystemDLL("api-ms-win-core-realtime-l1-1-1.dll").NewProc("QueryInterruptTime")
	var before, after uint64
	queryInterruptTime.Call(uintptr(unsafe.Pointer(&before)))
	got := bootRelativeNow()
	queryInterruptTime.Call(uintptr(unsafe.Pointer(&after)))

	// QueryInterruptTime is tick-granular; the precise clock can lead its latest
	// tick. A broad allowance avoids tying this test to the host timer period.
	minimum := time.Duration(before) * 100 * time.Nanosecond
	maximum := time.Duration(after)*100*time.Nanosecond + time.Second
	if got < minimum || got > maximum {
		t.Fatalf("boot clock = %v, want system uptime in [%v, %v]", got, minimum, maximum)
	}
}

func TestWindowsBootClockIsMonotonic(t *testing.T) {
	previous := bootRelativeNow()
	for i := range 256 {
		current := bootRelativeNow()
		if current < previous {
			t.Fatalf("boot clock moved backward at sample %d: %v -> %v", i, previous, current)
		}
		previous = current
	}
}

func TestWindowsAudioClockPreservesSourcePTSBeforeProcessStart(t *testing.T) {
	// This source PTS predates the process epoch regardless of when the test
	// runs. The process-relative fallback cannot represent it and would replace
	// it with send time, losing the original source-time delta.
	sourcePTS := appStartTime.Add(-250 * time.Millisecond)
	session := &MirrorSession{}
	beforeLocal := time.Now()
	beforeBoot := bootRelativeNow()
	timestamp, timeline := session.audioClockAt(sourcePTS)
	afterBoot := bootRelativeNow()
	afterLocal := time.Now()

	// Bound both clock reads in audioClockAt by their observed call interval,
	// not a fixed scheduling tolerance or a wall sleep.
	minimum := beforeBoot + sourcePTS.Sub(afterLocal)
	maximum := afterBoot + sourcePTS.Sub(beforeLocal)
	if minimum <= 0 {
		t.Fatalf("source PTS predates system boot: lower boot-time bound = %v", minimum)
	}
	epoch := uint64(secondsFrom1900To1970) << 32
	minimumTimestamp := compactTimestamp(minimum) + epoch
	maximumTimestamp := compactTimestamp(maximum) + epoch
	if timeline != 0 || timestamp < minimumTimestamp || timestamp > maximumTimestamp {
		t.Fatalf("startup source timestamp = %#x timeline = %#x, want NTP timestamp in [%#x, %#x] timeline = 0",
			timestamp, timeline, minimumTimestamp, maximumTimestamp)
	}
}
