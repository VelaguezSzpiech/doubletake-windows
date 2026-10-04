package airplay

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func selectCapturePreparationKind(goos string, cfg CaptureConfig, display, wayland string) (capturePreparationKind, error) {
	if goos == "windows" {
		if cfg.X11WindowID != 0 || cfg.X11WindowName != "" {
			return 0, fmt.Errorf("X11 window selection is not supported by Windows desktop capture")
		}
		return capturePreparationWindows, nil
	}
	if (cfg.X11WindowID != 0 || cfg.X11WindowName != "") && display != "" {
		return capturePreparationX11, nil
	}
	if wayland != "" {
		return capturePreparationWayland, nil
	}
	if display != "" {
		return capturePreparationX11, nil
	}
	return 0, fmt.Errorf("no display server detected (neither WAYLAND_DISPLAY nor DISPLAY is set)")
}

// startPreparedWindowsCapture uses the DXGI Desktop Duplication source from
// GStreamer's d3d11 plugin. monitor-index=-1 selects the primary monitor, not
// the entire virtual desktop. Keep textures on the GPU through conversion and
// scaling when supported; otherwise download for the existing CPU preprocessing.
// Preserve source PTS through the shared RTP/ONVIF suffix in either case.
func startPreparedWindowsCapture(ctx context.Context, cfg CaptureConfig, encoder encoderResult, timestampedOutput bool) (*ScreenCapture, error) {
	captureCtx, cancel := context.WithCancel(ctx)
	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	source := gstStage{
		"d3d11screencapturesrc", "monitor-index=-1",
		fmt.Sprintf("show-cursor=%t", cfg.ShowCursor),
	}
	beforeConvert := []gstStage{
		{fmt.Sprintf("video/x-raw(memory:D3D11Memory),framerate=%d/1", fps)},
	}
	if !encoder.d3d11Convert {
		beforeConvert = append(beforeConvert, gstStage{"d3d11download"})
	}
	beforeConvert = append(beforeConvert, lowLatencyVideoQueueStage())
	args := buildGstVideoPipeline(source, beforeConvert, nil, encoder, cfg.MaxWidth, cfg.MaxHeight, timestampedOutput)
	dbg("[CAPTURE] gst-launch-1.0 (windows) %s", strings.Join(args, " "))
	cmd := exec.CommandContext(captureCtx, "gst-launch-1.0", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdout.Close()
		return nil, fmt.Errorf("gst stderr pipe: %w", err)
	}
	waitResult, err := startGStreamerCommand(cmd)
	if err != nil {
		cancel()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start Windows desktop capture: %w", err)
	}
	go logStderr("GST", stderr)
	capture := &ScreenCapture{
		cmd: cmd, stdout: stdout, cancel: cancel, waitCh: make(chan struct{}),
	}
	if timestampedOutput {
		capture.frames = newRTPVideoAccessUnitReader(stdout, encoder.codec)
	}
	go func() {
		capture.waitErr = <-waitResult
		close(capture.waitCh)
	}()
	return capture, nil
}
