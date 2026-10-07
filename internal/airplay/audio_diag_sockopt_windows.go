package airplay

import (
	"net"
	"syscall"

	"golang.org/x/sys/windows"
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
		h := windows.Handle(fd)
		for _, opt := range []struct {
			name         string
			level, value int
		}{
			{"so_sndbuf", windows.SOL_SOCKET, windows.SO_SNDBUF},
			{"so_rcvbuf", windows.SOL_SOCKET, windows.SO_RCVBUF},
			{"ip_tos", windows.IPPROTO_IP, windows.IP_TOS},
		} {
			if v, err := windows.GetsockoptInt(h, opt.level, opt.value); err == nil {
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
