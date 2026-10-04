//go:build !windows

package airplay

import "net"

func writeAudioDatagram(conn net.PacketConn, packet []byte, addr net.Addr) (int, error) {
	return conn.WriteTo(packet, addr)
}
