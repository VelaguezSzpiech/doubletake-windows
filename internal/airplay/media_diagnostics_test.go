package airplay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

func captureMediaDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previousOutput := log.Writer()
	previousDebug := DebugMode()
	log.SetOutput(&output)
	SetDebugMode(true)
	t.Cleanup(func() {
		SetDebugMode(previousDebug)
		log.SetOutput(previousOutput)
	})
	return &output
}

func requireMediaDiagnosticsRedacted(t *testing.T, output string, values ...[]byte) {
	t.Helper()
	for _, value := range values {
		for _, representation := range []string{string(value), hex.EncodeToString(value), hex.EncodeToString(value[:min(8, len(value))])} {
			if strings.Contains(output, representation) {
				t.Error("media diagnostic disclosed a fixture value or byte prefix")
			}
		}
	}
}

func TestMediaDiagnosticsDoNotExposePlistValues(t *testing.T) {
	output := captureMediaDiagnostics(t)
	secret := []byte("fixture-media-key-not-for-logs")
	identity := "fixture-private-session-identity"
	debugDumpPlist("audio setup plist", map[string]interface{}{
		"shk":  secret,
		"name": identity,
		"nested": map[string]interface{}{
			identity: secret,
		},
		"streams": []interface{}{map[string]interface{}{"shk": secret}},
	})
	requireMediaDiagnosticsRedacted(t, output.String(), secret, []byte(identity))
	if !strings.Contains(output.String(), "audio setup plist") {
		t.Fatal("debug mode lost the setup diagnostic entirely")
	}
}

func TestMediaDiagnosticsHidePayloadsWhileSendingAuthenticPackets(t *testing.T) {
	output := captureMediaDiagnostics(t)
	key := bytes.Repeat([]byte{0xa7}, chacha20poly1305.KeySize)
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := net.Pipe()
	defer sender.Close()
	defer receiver.Close()
	if err := receiver.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	session := &MirrorSession{dataConn: sender, chachaCipher: aead}
	codec := []byte("fixture-codec-configuration-not-for-logs")
	plaintext := []byte("fixture-private-video-frame-not-for-logs")
	done := make(chan error, 1)
	go func() {
		if err := session.sendCodecFrame(codec, 0); err != nil {
			done <- err
			return
		}
		done <- session.sendFrame(plaintext, true, 0, 0)
	}()
	codecPacket := make([]byte, 128+len(codec))
	if _, err := io.ReadFull(receiver, codecPacket); err != nil {
		t.Fatal(err)
	}
	videoPacket := make([]byte, 128+len(plaintext)+aead.Overhead())
	if _, err := io.ReadFull(receiver, videoPacket); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if codecPacket[4] != 0x01 || !bytes.Equal(codecPacket[128:], codec) {
		t.Fatal("codec packet no longer carries the decoder configuration unchanged")
	}
	if videoPacket[4] != 0x00 || videoPacket[5] != 0x10 || int(binary.LittleEndian.Uint32(videoPacket[:4])) != len(videoPacket)-128 {
		t.Fatal("encrypted keyframe header no longer describes the emitted payload")
	}
	var nonce [12]byte
	decrypted, err := aead.Open(nil, nonce[:], videoPacket[128:], videoPacket[:128])
	if err != nil {
		t.Fatalf("receiver could not authenticate the emitted video frame: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatal("receiver got different video content")
	}
	requireMediaDiagnosticsRedacted(t, output.String(), key, codec, plaintext, videoPacket[128:])
	if !strings.Contains(output.String(), "[SEND]") || !strings.Contains(output.String(), "[CHACHA]") {
		t.Fatal("debug mode lost frame status or encryption diagnostics")
	}
}

func TestReplayDiagnosticsHideKeysAndPlaintextWhileReencrypting(t *testing.T) {
	output := captureMediaDiagnostics(t)
	captureKey := bytes.Repeat([]byte{0xb3}, 16)
	key, iv := deriveVideoKeys(captureKey, 7)
	captureCipher, err := newMirrorCipher(key, iv)
	if err != nil {
		t.Fatal(err)
	}
	codec := []byte("fixture-replay-codec-not-for-logs")
	plaintext := []byte("fixture-replay-private-frame-not-for-logs")
	var capture bytes.Buffer
	for _, frame := range []struct {
		kind    byte
		payload []byte
	}{{0x01, codec}, {0x00, captureCipher.EncryptFrame(plaintext)}} {
		var header [128]byte
		binary.LittleEndian.PutUint32(header[:4], uint32(len(frame.payload)))
		header[4] = frame.kind
		capture.Write(header[:])
		capture.Write(frame.payload)
	}
	captureFile := filepath.Join(t.TempDir(), "media-capture.bin")
	if err := os.WriteFile(captureFile, capture.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	sessionKey := bytes.Repeat([]byte{0xd5}, 16)
	sessionIV := bytes.Repeat([]byte{0xe6}, 16)
	sessionCipher, err := newMirrorCipher(sessionKey, sessionIV)
	if err != nil {
		t.Fatal(err)
	}
	receiverCipher, err := newMirrorCipher(sessionKey, sessionIV)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := net.Pipe()
	defer sender.Close()
	defer receiver.Close()
	if err := receiver.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &MirrorSession{dataConn: sender, streamCipher: sessionCipher.EncryptFrame, firstFrameSent: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- session.ReplayFrames(ctx, ReplayConfig{CaptureFile: captureFile, AESKeyHex: hex.EncodeToString(captureKey), SCID: 7})
	}()
	codecPacket := make([]byte, 128+len(codec))
	if _, err := io.ReadFull(receiver, codecPacket); err != nil {
		t.Fatal(err)
	}
	videoPacket := make([]byte, 128+len(plaintext))
	if _, err := io.ReadFull(receiver, videoPacket); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("replay cancellation = %v, want context.Canceled", err)
	}
	if codecPacket[4] != 0x01 || !bytes.Equal(codecPacket[128:], codec) {
		t.Fatal("replayed codec packet changed")
	}
	if videoPacket[4] != 0x00 || !bytes.Equal(receiverCipher.EncryptFrame(videoPacket[128:]), plaintext) {
		t.Fatal("replayed frame did not preserve the captured video after re-encryption")
	}
	select {
	case <-session.firstFrameSent:
	default:
		t.Fatal("replay did not signal the first delivered video frame")
	}
	requireMediaDiagnosticsRedacted(t, output.String(), captureKey, key, iv, codec, plaintext)
	if !strings.Contains(output.String(), "[REPLAY]") {
		t.Fatal("debug mode lost replay status diagnostics")
	}
}
