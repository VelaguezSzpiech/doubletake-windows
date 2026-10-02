package airplay

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"howett.net/plist"
)

func captureControlDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previousOutput, previousDebug := log.Writer(), DebugMode()
	log.SetOutput(&output)
	SetDebugMode(true)
	t.Cleanup(func() {
		SetDebugMode(previousDebug)
		log.SetOutput(previousOutput)
	})
	return &output
}

func assertControlDiagnosticsHide(t *testing.T, output string, secrets ...[]byte) {
	t.Helper()
	for _, secret := range secrets {
		for _, encoded := range []string{string(secret), hex.EncodeToString(secret)} {
			if strings.Contains(output, encoded) {
				t.Error("control diagnostics disclose a synthetic secret or protocol payload")
			}
		}
	}
}

func controlDiagnosticFrame(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	length := make([]byte, 2)
	binary.LittleEndian.PutUint16(length, uint16(len(plaintext)))
	return append(length, aead.Seal(nil, make([]byte, 12), plaintext, length)...)
}

func controlDiagnosticConnection(t *testing.T, wire []byte) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go func() {
		defer server.Close()
		_, _ = server.Write(wire)
	}()
	return client
}

func TestControlResponseDiagnosticsDoNotDiscloseSessionData(t *testing.T) {
	key := []byte("synthetic-control-read-key-12345")
	headerSecret := []byte("synthetic-session-header-secret")
	bodySecret := []byte("synthetic-protocol-body-secret")
	for _, encrypted := range []bool{false, true} {
		for _, status := range []int{200, 500} {
			t.Run(fmt.Sprintf("encrypted=%v/status=%d", encrypted, status), func(t *testing.T) {
				output := captureControlDiagnostics(t)
				wire := []byte(fmt.Sprintf("RTSP/1.0 %d Response\r\nX-Apple-Session-ID: %s\r\nContent-Length: %d\r\n\r\n%s", status, headerSecret, len(bodySecret), bodySecret))
				if encrypted {
					wire = controlDiagnosticFrame(t, key, wire)
				}
				client := &AirPlayClient{conn: controlDiagnosticConnection(t, wire), encrypted: encrypted, encReadKey: key}
				body, headers, err := client.readHTTPResponseWithTimeout(time.Second)
				if headers["x-apple-session-id"] != string(headerSecret) {
					t.Fatal("response session header was changed")
				}
				if status == 200 {
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(body, bodySecret) {
						t.Fatal("response body was changed")
					}
				} else {
					var statusErr *HTTPStatusError
					if !errors.As(err, &statusErr) || statusErr.StatusCode != status || !bytes.Equal(statusErr.Body, bodySecret) {
						t.Fatalf("response error lost its status or body: %v", err)
					}
					assertControlDiagnosticsHide(t, err.Error(), bodySecret)
				}
				assertControlDiagnosticsHide(t, output.String(), key, key[:8], headerSecret, bodySecret)
				if !strings.Contains(output.String(), strconv.Itoa(status)) {
					t.Error("debug diagnostics no longer report the response status")
				}
			})
		}
	}
}

func TestControlFailureDiagnosticsDoNotDisclosePartialPayloads(t *testing.T) {
	key := []byte("synthetic-control-read-key-12345")
	secret := []byte("synthetic-partial-payload-secret")
	partial := append([]byte("RTSP/1.0 200 OK\r\nX-Apple-Session-ID: "), secret...)
	badFrame := controlDiagnosticFrame(t, key, secret)
	badFrame[len(badFrame)-1] ^= 1
	for _, tc := range []struct {
		name      string
		encrypted bool
		wire      []byte
	}{
		{name: "plaintext truncated header", wire: partial},
		{name: "encrypted truncated header", encrypted: true, wire: controlDiagnosticFrame(t, key, partial)},
		{name: "invalid encrypted frame", encrypted: true, wire: append([]byte{0, 0}, secret...)},
		{name: "authentication failure", encrypted: true, wire: badFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := captureControlDiagnostics(t)
			client := &AirPlayClient{conn: controlDiagnosticConnection(t, tc.wire), encrypted: tc.encrypted, encReadKey: key}
			_, _, err := client.readHTTPResponseWithTimeout(time.Second)
			if err == nil {
				t.Fatal("malformed or truncated response was accepted")
			}
			assertControlDiagnosticsHide(t, output.String(), key, key[:8], secret, partial, badFrame[2:34])
			assertControlDiagnosticsHide(t, err.Error(), secret)
		})
	}
}

func TestControlInfoDiagnosticsDoNotDumpReceiverPlist(t *testing.T) {
	output := captureControlDiagnostics(t)
	secret := []byte("synthetic-info-plist-secret")
	payload, err := plist.Marshal(map[string]interface{}{
		"features":     uint64(FeatureScreen),
		"audioFormats": map[string]interface{}{"credential": string(secret)},
		"PTPInfo":      map[string]interface{}{"sessionKey": secret},
		string(secret): "private extension key",
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	clientConn, server := net.Pipe()
	defer clientConn.Close()
	serverResult := make(chan error, 1)
	go func() {
		defer server.Close()
		_ = server.SetDeadline(time.Now().Add(time.Second))
		reader := bufio.NewReader(server)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				serverResult <- readErr
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, writeErr := io.Copy(server, bytes.NewReader(append([]byte(fmt.Sprintf("RTSP/1.0 200 OK\r\nContent-Length: %d\r\n\r\n", len(payload))), payload...)))
		serverResult <- writeErr
	}()
	client := &AirPlayClient{conn: clientConn}
	info, err := client.getInfoWithTimeout(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	if info.Features != FeatureScreen || !info.hasPTPInfo {
		t.Fatal("receiver capability decoding was changed")
	}
	assertControlDiagnosticsHide(t, output.String(), secret)
}
