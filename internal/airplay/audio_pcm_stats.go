package airplay

import "math"

const (
	// pcmClipLevel is the magnitude from which a 16-bit sample counts as clipped.
	pcmClipLevel = 32700
	// pcmJumpLevel is the sample-to-sample step on one channel that real music
	// at 44.1 kHz essentially never makes (a full-scale 20 kHz tone steps about
	// 0.9 * 32767 only at the very top of its range). Steps this large are the
	// signature of a click, a dropped or repeated block, or a wrapped sample.
	pcmJumpLevel = 28000
)

// audioPCMStats summarizes one codec frame of interleaved little-endian 16-bit
// stereo PCM. It is measured on exactly the samples handed to the encoder, so a
// crackle that exists in the data we send is visible without recording audio.
type audioPCMStats struct {
	Samples int
	Peak    int
	SumSq   float64
	Clipped int
	Jumps   int
	MaxStep int
	DCSum   int64
	Silent  bool
}

func measurePCM(pcm []byte, channels int) audioPCMStats {
	var s audioPCMStats
	if channels < 1 {
		channels = 2
	}
	prev := make([]int, channels)
	havePrev := false
	for off, ch := 0, 0; off+1 < len(pcm); off += 2 {
		v := int(int16(uint16(pcm[off]) | uint16(pcm[off+1])<<8))
		a := v
		if a < 0 {
			a = -a
		}
		if a > s.Peak {
			s.Peak = a
		}
		if a >= pcmClipLevel {
			s.Clipped++
		}
		s.SumSq += float64(v) * float64(v)
		s.DCSum += int64(v)
		if havePrev {
			step := v - prev[ch]
			if step < 0 {
				step = -step
			}
			if step > s.MaxStep {
				s.MaxStep = step
			}
			if step >= pcmJumpLevel {
				s.Jumps++
			}
		}
		prev[ch] = v
		s.Samples++
		if ch++; ch == channels {
			ch = 0
			havePrev = true
		}
	}
	s.Silent = s.Peak == 0
	return s
}

// audioPCMInterval accumulates audioPCMStats over a health interval.
type audioPCMInterval struct {
	frames, silentFrames, samples int
	peak, clipped, jumps, maxStep int
	clippedFrames, jumpFrames     int
	sumSq                         float64
	dcSum                         int64
}

func (a *audioPCMInterval) add(s audioPCMStats) {
	if s.Samples == 0 {
		return
	}
	a.frames++
	a.samples += s.Samples
	a.sumSq += s.SumSq
	a.dcSum += s.DCSum
	a.clipped += s.Clipped
	a.jumps += s.Jumps
	if s.Clipped > 0 {
		a.clippedFrames++
	}
	if s.Jumps > 0 {
		a.jumpFrames++
	}
	if s.Silent {
		a.silentFrames++
	}
	if s.Peak > a.peak {
		a.peak = s.Peak
	}
	if s.MaxStep > a.maxStep {
		a.maxStep = s.MaxStep
	}
}

func dbfs(level float64) float64 {
	if level <= 0 {
		return -120
	}
	return math.Round(20*math.Log10(level/32768)*10) / 10
}

func (a *audioPCMInterval) record() map[string]any {
	rec := map[string]any{
		"frames_iv":         a.frames,
		"silent_frames_iv":  a.silentFrames,
		"clipped_iv":        a.clipped,
		"clipped_frames_iv": a.clippedFrames,
		"jumps_iv":          a.jumps,
		"jump_frames_iv":    a.jumpFrames,
		"max_step":          a.maxStep,
		"peak_dbfs":         dbfs(float64(a.peak)),
	}
	if a.samples > 0 {
		rec["rms_dbfs"] = dbfs(math.Sqrt(a.sumSq / float64(a.samples)))
		rec["dc_mean"] = math.Round(float64(a.dcSum)/float64(a.samples)*10/1) / 10
	}
	return rec
}
