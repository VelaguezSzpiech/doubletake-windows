//go:build !linux && !windows

package airplay

import "time"

// Platforms without a system boot clock retain the process-relative fallback.
func bootRelativeNow() time.Duration {
	return time.Since(appStartTime)
}
