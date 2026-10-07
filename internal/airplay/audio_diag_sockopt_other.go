//go:build !windows && !unix

package airplay

import "net"

func audioSocketOptions(conn net.PacketConn) map[string]any {
	return map[string]any{"error": "socket options unavailable on this platform"}
}
