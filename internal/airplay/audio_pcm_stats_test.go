package airplay

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func pcmFrame(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func TestMeasurePCMCleanBassHasNoJumpsOrClipping(t *testing.T) {
	samples := make([]int16, 352*2)
	for i := 0; i < 352; i++ {
		v := int16(26000 * math.Sin(2*math.Pi*55*float64(i)/44100))
		samples[2*i], samples[2*i+1] = v, v
	}
	s := measurePCM(pcmFrame(samples), 2)
	if s.Clipped != 0 || s.Jumps != 0 || s.Peak < 25900 || s.Peak > 26000 {
		t.Fatalf("clean 55 Hz bass: %+v", s)
	}
}

func TestMeasurePCMFlagsClickClippingAndSilence(t *testing.T) {
	samples := make([]int16, 352*2)
	samples[100], samples[101] = 20000, 20000
	samples[102], samples[103] = -20000, -20000 // adjacent frame: a 40000 step on each channel
	s := measurePCM(pcmFrame(samples), 2)
	if s.Jumps != 2 || s.MaxStep != 40000 {
		t.Fatalf("click not flagged: %+v", s)
	}
	clipped := make([]int16, 352*2)
	for i := range clipped {
		clipped[i] = 32767
	}
	if c := measurePCM(pcmFrame(clipped), 2); c.Clipped != 704 || c.Jumps != 0 {
		t.Fatalf("clipping stats: %+v", c)
	}
	if z := measurePCM(make([]byte, 1408), 2); !z.Silent {
		t.Fatalf("silence not detected: %+v", z)
	}
}

func TestAudioHealthReportsPCMAndFlagsJumps(t *testing.T) {
	clean := healthFor(t, func(d *audioDiag, start time.Time) { feedHealthy(d, start, 12*time.Millisecond, 600) })
	pcm, ok := clean["pcm"].(map[string]any)
	if !ok || pcm["frames_iv"] == nil {
		t.Fatalf("health has no pcm block: %v", clean["pcm"])
	}
}
