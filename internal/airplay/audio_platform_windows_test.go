//go:build windows

package airplay

import (
	"context"
	"strings"
	"testing"
)

func TestWindowsAudioCaptureRejectsMissingLoopbackPlugin(t *testing.T) {
	// No installed runtime can be discovered in the empty directory. The
	// production path must fail visibly, not fall back to a microphone or tone.
	t.Setenv("PATH", t.TempDir())
	capture, err := StartAudioCapture(context.Background(), false, AudioCodecALAC)
	if capture != nil {
		capture.Stop()
		t.Fatal("missing WASAPI plugin unexpectedly started capture")
	}
	if err == nil || !strings.Contains(err.Error(), "wasapi2src") {
		t.Fatalf("missing loopback plugin error = %v, want wasapi2src requirement", err)
	}
}

func TestWindowsAudioCaptureRejectsUnsupportedCodecBeforeLaunching(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	capture, err := StartAudioCapture(context.Background(), false, AudioCodec(-1))
	if capture != nil {
		capture.Stop()
		t.Fatal("unsupported codec unexpectedly started capture")
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported audio codec") {
		t.Fatalf("unsupported codec error = %v", err)
	}
}
