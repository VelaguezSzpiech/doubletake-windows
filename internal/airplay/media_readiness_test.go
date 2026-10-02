package airplay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func assertMediaNotStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
		t.Fatal("media reported ready before a successful media write")
	default:
	}
}

func awaitMediaStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("successful media write did not report readiness")
	}
}

func awaitMediaStreamResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("media stream did not stop")
		return nil
	}
}

func TestVideoStartedRequiresCompletePresentableFrame(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "successful media write"
		if fail {
			name = "failed media write"
		}
		t.Run(name, func(t *testing.T) {
			sender, receiver := net.Pipe()
			defer sender.Close()
			defer receiver.Close()
			if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			session := &MirrorSession{dataConn: sender, firstFrameSent: make(chan struct{})}
			capture := &ScreenCapture{
				frames: &sliceVideoAccessUnitReader{frames: []VideoAccessUnit{{AnnexB: joinAnnexBNALs(
					[]byte{0x67, 0x42, 0x00, 0x1f, 0xf8, 0x0a, 0x00, 0xb7, 0x20},
					[]byte{0x68, 0xce, 0x06, 0xe2},
					[]byte{0x65, 0x80, 0x01},
				)}}},
				waitCh: make(chan struct{}),
			}
			started := session.VideoStarted()
			assertMediaNotStarted(t, started)
			done := make(chan error, 1)
			go func() { done <- session.StreamFrames(context.Background(), capture, 0) }()

			var header [128]byte
			if _, err := io.ReadFull(receiver, header[:]); err != nil {
				t.Fatal(err)
			}
			if header[4] != 1 {
				t.Fatalf("first packet type = %d, want codec configuration", header[4])
			}
			codec := make([]byte, binary.LittleEndian.Uint32(header[:4]))
			if _, err := io.ReadFull(receiver, codec); err != nil {
				t.Fatal(err)
			}
			assertMediaNotStarted(t, started)
			if _, err := io.ReadFull(receiver, header[:]); err != nil {
				t.Fatal(err)
			}
			if header[4] != 0 || header[5] != 0x10 {
				t.Fatalf("media header = %02x, want presentable IDR", header[4:6])
			}
			// The header alone is not a complete frame. The pipe keeps the media
			// write blocked until its payload is consumed or the peer disconnects.
			assertMediaNotStarted(t, started)
			if fail {
				_ = receiver.Close()
				if err := awaitMediaStreamResult(t, done); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("StreamFrames error = %v, want closed pipe", err)
				}
				assertMediaNotStarted(t, started)
				return
			}
			payload := make([]byte, binary.LittleEndian.Uint32(header[:4]))
			if _, err := io.ReadFull(receiver, payload); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload, []byte{0, 0, 0, 3, 0x65, 0x80, 0x01}) {
				t.Fatalf("video payload = %x, want complete IDR", payload)
			}
			awaitMediaStarted(t, started)
			if err := awaitMediaStreamResult(t, done); err == nil {
				t.Fatal("StreamFrames did not report capture EOF")
			}
		})
	}
}

func TestVideoStartedRemainsPendingOnCanceledStartup(t *testing.T) {
	session := &MirrorSession{firstFrameSent: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.StreamFrames(ctx, &ScreenCapture{}, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("StreamFrames error = %v, want cancellation", err)
	}
	assertMediaNotStarted(t, session.VideoStarted())
}

type gatedMediaPacketConn struct {
	net.PacketConn
	ctx     context.Context
	writes  chan []byte
	release chan error
	short   bool
}

func (c *gatedMediaPacketConn) WriteTo(packet []byte, addr net.Addr) (int, error) {
	select {
	case c.writes <- packet:
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
	select {
	case err := <-c.release:
		if err != nil {
			return 0, err
		}
		if c.short {
			return len(packet) - 1, nil
		}
		return c.PacketConn.WriteTo(packet, addr)
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
}

func TestAudioStartedRequiresSuccessfulMediaWrite(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancellation", "short write", "uninitialized signal"} {
		t.Run(outcome, func(t *testing.T) {
			listen := func() net.PacketConn {
				t.Helper()
				conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				return conn
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctrlConn, ctrlPeer, dataConn, dataPeer := listen(), listen(), listen(), listen()
			gated := &gatedMediaPacketConn{
				PacketConn: dataConn, ctx: ctx, writes: make(chan []byte, 1), release: make(chan error, 1),
				short: outcome == "short write",
			}
			session := &MirrorSession{
				firstFrameSent: make(chan struct{}), firstAudioSent: make(chan struct{}), timingProtocol: timingProtocolNTP,
			}
			if outcome == "uninitialized signal" {
				session.firstAudioSent = nil
			}
			stream := &AudioStream{
				conn: gated, ctrlConn: ctrlConn,
				remoteAddr: dataPeer.LocalAddr().(*net.UDPAddr), ctrlAddr: ctrlPeer.LocalAddr().(*net.UDPAddr),
				spf: 352, ct: byte(AudioCodecALAC), latencySamples: targetLatencySamples44k1(),
			}
			pcm := newGatedPCMReader()
			defer pcm.Close()
			capture := &AudioCapture{pcmPipe: pcm, waitCh: make(chan struct{}), codec: AudioCodecALAC}
			done := make(chan error, 1)
			go func() { done <- session.StreamAudio(ctx, capture, stream) }()
			waitForRead := func() {
				t.Helper()
				select {
				case <-pcm.reads:
				case <-time.After(time.Second):
					t.Fatal("capture did not request a PCM frame")
				}
			}
			feedPCM := func() {
				t.Helper()
				select {
				case pcm.frames <- make([]byte, 352*2*2):
				case <-time.After(time.Second):
					t.Fatal("capture did not consume PCM frame")
				}
			}
			started := session.AudioStarted()
			waitForRead()
			assertMediaNotStarted(t, started)
			// Release the final prewarm read after video is ready. This sample
			// must still be discarded, not mistaken for an audio media send.
			close(session.firstFrameSent)
			feedPCM()
			waitForRead()
			assertMediaNotStarted(t, started)
			feedPCM()
			select {
			case packet := <-gated.writes:
				if len(packet) <= 12 || packet[0] != 0x80 || packet[1] != 0x60 {
					t.Fatalf("packet = %x, want RTP audio with media payload", packet)
				}
			case <-time.After(time.Second):
				t.Fatal("audio did not attempt its first media write")
			}
			// Initial TimeAnnounce has succeeded, but the actual media write is
			// still blocked. Neither captured PCM nor clock sync means ready.
			assertMediaNotStarted(t, started)
			writeErr := errors.New("media send failed")
			switch outcome {
			case "failure":
				gated.release <- writeErr
			case "cancellation":
				cancel()
			default:
				gated.release <- nil
			}
			if outcome == "success" || outcome == "uninitialized signal" {
				if err := dataPeer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				var packet [2048]byte
				if _, _, err := dataPeer.ReadFrom(packet[:]); err != nil {
					t.Fatalf("receive first audio media packet: %v", err)
				}
				if outcome == "success" {
					awaitMediaStarted(t, started)
				}
				cancel()
				_ = pcm.Close()
			}
			err := awaitMediaStreamResult(t, done)
			switch outcome {
			case "failure":
				if !errors.Is(err, writeErr) || errors.Is(err, context.Canceled) {
					t.Fatalf("StreamAudio error = %v, want original send failure, not cancellation", err)
				}
			case "short write":
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("StreamAudio error = %v, want short write", err)
				}
			default:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("StreamAudio error = %v, want cancellation", err)
				}
			}
			if outcome != "success" {
				assertMediaNotStarted(t, started)
			}
		})
	}
}
