package airplay

import (
	"errors"
	"net"
	"time"

	"golang.org/x/sys/windows"
)

// writeAudioDatagram retries only datagrams rejected before transmission by
// Windows buffer pressure. Reuse the sealed bytes: rebuilding an audio packet
// here would consume another nonce and change retransmission history. A short
// retry budget prevents persistent pressure from building a stale audio queue.
func writeAudioDatagram(conn net.PacketConn, packet []byte, addr net.Addr) (int, error) {
	for attempt := 0; ; attempt++ {
		n, err := conn.WriteTo(packet, addr)
		if n != 0 || !errors.Is(err, windows.WSAENOBUFS) || attempt == 3 {
			return n, err
		}
		// No sleeping or extra allocation on the successful steady-state path.
		time.Sleep(2 * time.Millisecond)
	}
}
