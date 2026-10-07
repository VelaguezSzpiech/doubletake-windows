//go:build !windows

package airplay

import (
	"context"
	"errors"
	"net"
	"runtime"
	"time"
)

// Host telemetry is Windows-only; other platforms report the clock, scheduler
// and Go facts and mark the rest unavailable once.
func startSystemDiagPlatform(ctx context.Context, s *sysDiag, run func(string, func(context.Context))) {
	run("health", func(ctx context.Context) {
		diagEmit("sys.start", map[string]any{
			"go_version": runtime.Version(),
			"gomaxprocs": runtime.GOMAXPROCS(0),
			"cpu_cores":  runtime.NumCPU(),
			"os":         runtime.GOOS,
			"receiver":   s.receiver,
		})
		s.unavail.report("sys.host_probes", errors.New("not implemented on "+runtime.GOOS))
		ticker := time.NewTicker(sysHealthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			f := map[string]any{}
			s.sched.take(f)
			diagEmit("sys.health", f)
		}
	})
}

func newSysPinger(net.IP) (sysPinger, error) {
	return nil, errors.New("ICMP probe not implemented on " + runtime.GOOS)
}

func sysWallNow() time.Time { return time.Now().Round(0) }
