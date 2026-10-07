//go:build unix

package airplay

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// audioSocketOptions reads diagnostic socket options; failures are reported
// in the map instead of being fatal.
func audioSocketOptions(conn net.PacketConn) map[string]any {
	out := map[string]any{}
	sc, ok := conn.(syscall.Conn)
	if !ok {
		out["error"] = "not a syscall.Conn"
		return out
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	ctrlErr := raw.Control(func(fd uintptr) {
		for _, opt := range []struct {
			name         string
			level, value int
		}{
			{"so_sndbuf", unix.SOL_SOCKET, unix.SO_SNDBUF},
			{"so_rcvbuf", unix.SOL_SOCKET, unix.SO_RCVBUF},
			{"ip_tos", unix.IPPROTO_IP, unix.IP_TOS},
		} {
			if v, err := unix.GetsockoptInt(int(fd), opt.level, opt.value); err == nil {
				out[opt.name] = v
			} else {
				out[opt.name+"_error"] = err.Error()
			}
		}
	})
	if ctrlErr != nil {
		out["error"] = ctrlErr.Error()
	}
	return out
}
