package airplay

import "testing"

func TestCapturePlatformSelection(t *testing.T) {
	for _, test := range []struct {
		name, goos, display, wayland string
		cfg                          CaptureConfig
		want                         capturePreparationKind
		wantErr                      bool
	}{
		{name: "Windows without Unix display", goos: "windows", want: capturePreparationWindows},
		{name: "Windows ignores stale Unix environment", goos: "windows", display: ":0", wayland: "wayland-0", want: capturePreparationWindows},
		{name: "Windows rejects X11 window ID", goos: "windows", cfg: CaptureConfig{X11WindowID: 1}, wantErr: true},
		{name: "Windows rejects X11 window name", goos: "windows", cfg: CaptureConfig{X11WindowName: "window"}, wantErr: true},
		{name: "Linux prefers Wayland", goos: "linux", display: ":0", wayland: "wayland-0", want: capturePreparationWayland},
		{name: "Linux explicit window prefers X11", goos: "linux", display: ":0", wayland: "wayland-0", cfg: CaptureConfig{X11WindowID: 1}, want: capturePreparationX11},
		{name: "Linux X11 only", goos: "linux", display: ":0", want: capturePreparationX11},
		{name: "Linux no display", goos: "linux", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectCapturePreparationKind(test.goos, test.cfg, test.display, test.wayland)
			if (err != nil) != test.wantErr {
				t.Fatalf("selection error = %v, want error %t", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("selected kind = %v, want %v", got, test.want)
			}
		})
	}
}
