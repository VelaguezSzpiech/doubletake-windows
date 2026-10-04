package airplay

import (
	"context"
	"strings"
	"testing"
)

func TestReceiverVideoCanvasUsesMaximumOnlyForCapabilityGatedHEVC(t *testing.T) {
	info := &ReceiverInfo{
		Features: uint64(1) << featureScreenMultiCodec,
		Displays: []DisplayInfo{{
			WidthPixels: 1280, HeightPixels: 720,
			WidthPixelsMax: 3840, HeightPixelsMax: 2160,
		}},
	}
	if width, height, err := info.videoCanvas(VideoCodecH264); err != nil || width != 1280 || height != 720 {
		t.Fatalf("H.264 canvas = %dx%d err=%v, want nominal 1280x720", width, height, err)
	}
	if width, height, err := info.videoCanvas(VideoCodecHEVC); err != nil || width != 3840 || height != 2160 {
		t.Fatalf("HEVC canvas = %dx%d err=%v, want maximum 3840x2160", width, height, err)
	}
	info.Features &^= uint64(1) << featureScreenMultiCodec
	if _, _, err := info.videoCanvas(VideoCodecHEVC); err == nil {
		t.Fatal("HEVC canvas accepted without feature 42")
	}
}

func TestAutomaticVideoSelectionRequiresReceiverMaximumAndLocalHardware(t *testing.T) {
	highResolution := &ReceiverInfo{
		Features: uint64(1) << featureScreenMultiCodec,
		Displays: []DisplayInfo{{
			WidthPixels: 1280, HeightPixels: 720,
			WidthPixelsMax: 3840, HeightPixelsMax: 2160,
		}},
	}
	tests := []struct {
		name      string
		info      *ReceiverInfo
		localHEVC bool
		wantCodec VideoCodec
		wantW     int
		wantH     int
	}{
		{name: "all gates", info: highResolution, localHEVC: true, wantCodec: VideoCodecHEVC, wantW: 3840, wantH: 2160},
		{name: "local hardware unavailable", info: highResolution, wantCodec: VideoCodecH264, wantW: 1280, wantH: 720},
		{name: "receiver codec capability absent", info: &ReceiverInfo{Displays: highResolution.Displays}, localHEVC: true, wantCodec: VideoCodecH264, wantW: 1280, wantH: 720},
		{name: "maximum is only 1080p", info: &ReceiverInfo{
			Features: uint64(1) << featureScreenMultiCodec,
			Displays: []DisplayInfo{{WidthPixels: 1280, HeightPixels: 720, WidthPixelsMax: 1920, HeightPixelsMax: 1080}},
		}, localHEVC: true, wantCodec: VideoCodecH264, wantW: 1280, wantH: 720},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, err := test.info.selectVideo(VideoCodecAuto, test.localHEVC, false)
			if err != nil {
				t.Fatal(err)
			}
			if selection.codec != test.wantCodec || selection.width != test.wantW || selection.height != test.wantH {
				t.Fatalf("selection = %s %dx%d, want %s %dx%d", selection.codec, selection.width, selection.height, test.wantCodec, test.wantW, test.wantH)
			}
		})
	}
}

func TestAutomaticVideoSelectionCapsMaximumPreservingAspect(t *testing.T) {
	info := &ReceiverInfo{
		Features: uint64(1) << featureScreenMultiCodec,
		Displays: []DisplayInfo{{
			WidthPixels: 1920, HeightPixels: 1080,
			WidthPixelsMax: 7680, HeightPixelsMax: 2160,
		}},
	}
	selection, err := info.selectVideo(VideoCodecAuto, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if selection.codec != VideoCodecHEVC || selection.width != 3840 || selection.height != 1080 {
		t.Fatalf("selection = %s %dx%d, want HEVC 3840x1080", selection.codec, selection.width, selection.height)
	}
	forced, err := info.selectVideo(VideoCodecHEVC, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if forced.width != 3840 || forced.height != 1080 {
		t.Fatalf("forced HEVC canvas = %dx%d, want capped 3840x1080", forced.width, forced.height)
	}
}

func TestExplicitVideoCodecOverridesAutomaticPolicy(t *testing.T) {
	info := &ReceiverInfo{
		Features: uint64(1) << featureScreenMultiCodec,
		Displays: []DisplayInfo{{
			WidthPixels: 1280, HeightPixels: 720,
			WidthPixelsMax: 3840, HeightPixelsMax: 2160,
		}},
	}
	h264, err := info.selectVideo(VideoCodecH264, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if h264.codec != VideoCodecH264 || h264.width != 1280 || h264.height != 720 {
		t.Fatalf("explicit H.264 = %s %dx%d", h264.codec, h264.width, h264.height)
	}

	hevc, err := info.selectVideo(VideoCodecHEVC, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if hevc.codec != VideoCodecHEVC || hevc.width != 3840 || hevc.height != 2160 {
		t.Fatalf("explicit HEVC = %s %dx%d", hevc.codec, hevc.width, hevc.height)
	}
}

func TestFullHDVideoSelectionHonorsExplicitMaximum(t *testing.T) {
	tests := []struct {
		name      string
		info      *ReceiverInfo
		requested VideoCodec
		wantCodec VideoCodec
		wantW     int
		wantH     int
	}{
		{name: "no display metadata is an explicit request", info: &ReceiverInfo{}, requested: VideoCodecH264, wantCodec: VideoCodecH264, wantW: 1920, wantH: 1080},
		{name: "nominal is not an explicit maximum", info: &ReceiverInfo{Displays: []DisplayInfo{{WidthPixels: 1280, HeightPixels: 720}}}, requested: VideoCodecH264, wantCodec: VideoCodecH264, wantW: 1920, wantH: 1080},
		{name: "4K maximum does not select 4K or HEVC", info: &ReceiverInfo{Features: uint64(1) << featureScreenMultiCodec, Displays: []DisplayInfo{{WidthPixelsMax: 3840, HeightPixelsMax: 2160}}}, requested: VideoCodecAuto, wantCodec: VideoCodecH264, wantW: 1920, wantH: 1080},
		{name: "explicit 720p ceiling remains 720p", info: &ReceiverInfo{Displays: []DisplayInfo{{WidthPixelsMax: 1280, HeightPixelsMax: 720}}}, requested: VideoCodecH264, wantCodec: VideoCodecH264, wantW: 1280, wantH: 720},
		{name: "narrow ceiling preserves requested aspect", info: &ReceiverInfo{Displays: []DisplayInfo{{WidthPixelsMax: 1440, HeightPixelsMax: 1080}}}, requested: VideoCodecH264, wantCodec: VideoCodecH264, wantW: 1440, wantH: 810},
		{name: "explicit HEVC remains capability gated and capped", info: &ReceiverInfo{Features: uint64(1) << featureScreenMultiCodec, Displays: []DisplayInfo{{WidthPixelsMax: 3840, HeightPixelsMax: 2160}}}, requested: VideoCodecHEVC, wantCodec: VideoCodecHEVC, wantW: 1920, wantH: 1080},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, err := test.info.selectVideo(test.requested, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if selection.codec != test.wantCodec || selection.width != test.wantW || selection.height != test.wantH {
				t.Fatalf("selection = %s %dx%d, want %s %dx%d", selection.codec, selection.width, selection.height, test.wantCodec, test.wantW, test.wantH)
			}
		})
	}
	if _, err := (&ReceiverInfo{}).selectVideo(VideoCodecHEVC, true, true); err == nil {
		t.Fatal("explicit Full HD HEVC request accepted without feature 42")
	}
}

func TestValidateVideoCodecAcceptsAuto(t *testing.T) {
	for _, codec := range []string{"", "auto", "h264", "hevc"} {
		if err := ValidateVideoCodec(codec); err != nil {
			t.Errorf("ValidateVideoCodec(%q): %v", codec, err)
		}
	}
	if err := ValidateVideoCodec("vp9"); err == nil || !strings.Contains(err.Error(), "auto, h264, or hevc") {
		t.Fatalf("ValidateVideoCodec(vp9) error = %v", err)
	}
}

func TestLegacyMirrorPreparationAPIRejectsAutomaticCodec(t *testing.T) {
	client := &AirPlayClient{}
	if _, err := client.SetupMirror(context.Background(), StreamConfig{VideoCodec: VideoCodecAuto}); err == nil || !strings.Contains(err.Error(), "VideoCodecPreparation") {
		t.Fatalf("SetupMirror(auto) error = %v", err)
	}
	if _, err := client.SetupMirrorWithVideoPreparation(context.Background(), StreamConfig{VideoCodec: VideoCodecAuto}, func(int, int) error { return nil }); err == nil || !strings.Contains(err.Error(), "VideoCodecPreparation") {
		t.Fatalf("legacy preparation(auto) error = %v", err)
	}
}
