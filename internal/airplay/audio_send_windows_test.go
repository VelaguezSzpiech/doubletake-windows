package airplay

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

type pressurePacketConn struct {
	*recordingPacketConn
	failures int
	err      error
	attempts [][]byte
}

func (c *pressurePacketConn) WriteTo(packet []byte, addr net.Addr) (int, error) {
	c.attempts = append(c.attempts, append([]byte(nil), packet...))
	if c.failures > 0 {
		c.failures--
		return 0, c.err
	}
	return c.recordingPacketConn.WriteTo(packet, addr)
}

func audioBufferPressureError() error {
	return &net.OpError{Op: "write", Net: "udp", Err: &os.SyscallError{Syscall: "wsasendto", Err: windows.WSAENOBUFS}}
}

func TestAudioSendRecoversWindowsBufferPressureWithoutChangingCiphertext(t *testing.T) {
	aead, err := newAudioChaCha64AEAD(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	conn := &pressurePacketConn{recordingPacketConn: &recordingPacketConn{}, failures: 1, err: audioBufferPressureError()}
	stream := &AudioStream{conn: conn, remoteAddr: &netUDPAddrForAudioTest, chachaCipher: aead, chachaNonceMode: audioChaChaNonceCounter, chachaAADMode: audioChaChaAADTimestampSSRC}
	payload := []byte("audio survives a rejected datagram")
	if err := stream.sendAudioPacketWithSeq(payload, 352, 1); err != nil {
		t.Fatalf("transient pressure ended audio: %v", err)
	}
	if len(conn.packets) != 1 || len(conn.attempts) != 2 {
		t.Fatalf("delivered=%d attempts=%d", len(conn.packets), len(conn.attempts))
	}
	if !bytes.Equal(conn.attempts[0], conn.attempts[1]) {
		t.Fatal("retry changed encrypted packet/nonce")
	}
	packet := conn.packets[0]
	var nonce [audioChaChaNonceSize]byte
	copy(nonce[:], packet[len(packet)-8:])
	plain, err := aead.Open(nil, nonce[:], packet[12:len(packet)-8], packet[4:12])
	if err != nil || !bytes.Equal(plain, payload) {
		t.Fatalf("receiver decrypt: %q, %v", plain, err)
	}
	if err := stream.sendAudioPacketWithSeq(payload, 704, 2); err != nil {
		t.Fatal(err)
	}
	next := conn.packets[1]
	if binary.LittleEndian.Uint64(next[len(next)-8:]) != binary.LittleEndian.Uint64(packet[len(packet)-8:])+1 {
		t.Fatal("retry consumed an extra encryption nonce")
	}
}

func TestAudioSendStopsOnPersistentPressureOrPermanentError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"persistent pressure", audioBufferPressureError(), 4},
		{"closed", net.ErrClosed, 1},
		{"permission", windows.WSAEACCES, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &pressurePacketConn{recordingPacketConn: &recordingPacketConn{}, failures: 100, err: tc.err}
			stream := &AudioStream{conn: conn, remoteAddr: &netUDPAddrForAudioTest}
			err := stream.sendAudioPacketWithSeq([]byte("audio"), 352, 1)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error=%v, want original %v", err, tc.err)
			}
			if len(conn.attempts) != tc.attempts || len(conn.packets) != 0 {
				t.Fatalf("attempts=%d delivered=%d", len(conn.attempts), len(conn.packets))
			}
			if stream.rtpTime != 0 {
				t.Fatal("failed send advanced delivered RTP time")
			}
		})
	}
}
