package airplay

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Structured audio diagnostics.
//
// Every record is one JSON object on one log line, prefixed "[DIAG] ". Field
// names carry their unit (_us microseconds, _ms, _ppm, _s seconds); counters
// named *_total are cumulative for the session, *_iv cover the last interval.
// Records never contain PCM, keys or receiver credentials.
//
// kind values:
//
//	audio.start               stream parameters, once per session
//	audio.health              every audioHealthInterval from its own ticker, so a
//	                          stalled capture or send loop is still reported
//	audio.stale_start/_end    a run of frames dropped as late
//	audio.clock_reset         source clock went backwards; new RTP mapping
//	audio.capture_discontinuity  capture RTP sequence/timestamp gap
//	audio.send_slow           a single packet send blocked longer than audioSlowSend
//	audio.stop                the stream ended, with the error if any
//	clock.feedback            one receiver clock sample from /feedback
//
// Every record carries diag_seq, a process-wide emission counter. Wire-level
// and control-plane kinds are listed in audio_diag_dataplane.go and
// audio_diag_wire.go.
//
// audio.health.status is "ok", "degraded" or "failing"; audio.health.flags
// names every condition that raised it. scripts/audio-health.ps1 summarizes them.
const (
	audioHealthInterval = 5 * time.Second
	audioSlowSend       = 20 * time.Millisecond
	audioReadStall      = 50 * time.Millisecond
	// An announce that moves the mapping by more than this relative to a constant
	// sample rate is a step the receiver must slew through.
	audioAnnounceJump = 500 * time.Microsecond
)

// audioSendRetries counts WSAENOBUFS datagram retries (Windows buffer pressure).
var audioSendRetries atomic.Uint64

// diagSeq numbers every [DIAG] record in emission order (diag_seq), so gaps or
// reordering in a collected log are detectable.
var diagSeq atomic.Uint64

func diagEmit(kind string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["kind"] = kind
	fields["ts"] = time.Now().Format(time.RFC3339Nano)
	fields["diag_seq"] = diagSeq.Add(1)
	line, err := json.Marshal(fields)
	if err != nil {
		log.Printf("[DIAG] {\"kind\":\"diag.error\",\"error\":%q}", err.Error())
		return
	}
	log.Printf("[DIAG] %s", line)
	diagArchiveWrite(line)
}

func micros(d time.Duration) int64 { return d.Microseconds() }

type audioDiagSample struct {
	rtp uint32
	pts time.Time
	ok  bool
}

// audioDiag accumulates per-interval statistics from the media loop.
type audioDiag struct {
	mu       sync.Mutex
	start    time.Time
	lastEmit time.Time

	framesRead, framesSent, staleTotal, clockResets uint64
	ivRead, ivSent, ivStale                         uint64

	ageLast, ageMin, ageMax, ageSum time.Duration
	ageN                            int
	sendMax, readMax                time.Duration
	sendSlow, readSlow              int
	lastFrameAt                     time.Time
	lastSeq                         uint16
	lastRTP                         uint32

	correction, prevCorrection time.Duration
	first, last                audioDiagSample

	prevDisc, prevResets, prevRetransReq, prevRetransExpired, prevRetries uint64

	announces, announceErrors atomic.Uint64

	// Announced RTP-to-network-time mapping: deviation of each announce from a
	// constant 44.1 kHz line relative to the previous one.
	annPrevOK            bool
	annPrevRTP           uint32
	annPrevNet           uint64
	annErrMin, annErrMax time.Duration
	annErrN              int
	pcm                  audioPCMInterval
}

func newAudioDiag(now time.Time) *audioDiag {
	return &audioDiag{start: now, lastEmit: now}
}

func (d *audioDiag) onRead(wait time.Duration) {
	d.mu.Lock()
	d.framesRead++
	d.ivRead++
	if wait > d.readMax {
		d.readMax = wait
	}
	if wait > audioReadStall {
		d.readSlow++
	}
	d.mu.Unlock()
}

func (d *audioDiag) onFrame(now time.Time, position audioPCMFramePosition) {
	age := now.Sub(position.PTS)
	d.mu.Lock()
	d.lastFrameAt = now
	d.ageLast = age
	if d.ageN == 0 || age < d.ageMin {
		d.ageMin = age
	}
	if d.ageN == 0 || age > d.ageMax {
		d.ageMax = age
	}
	d.ageSum += age
	d.ageN++
	d.correction = position.ClockCorrection
	d.pcm.add(position.PCM)
	if !d.first.ok {
		d.first = audioDiagSample{position.SourceRTP, position.PTS, true}
	}
	d.last = audioDiagSample{position.SourceRTP, position.PTS, true}
	d.mu.Unlock()
}

func (d *audioDiag) framesSentTotal() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.framesSent
}

// onAnnounce records one TimeAnnounce pair (RTP position, network time in
// 32.32 fixed point). A smooth stream announces a constant-rate line, so the
// error against 44100 samples/s must stay near zero; steps here are what the
// receiver has to slew its playout clock through.
func (d *audioDiag) onAnnounce(rtp uint32, network uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.annPrevOK {
		dNet := float64(int64(network-d.annPrevNet)) / (1 << 32) * float64(time.Second)
		dRTP := float64(int32(rtp-d.annPrevRTP)) / audioSampleRate * float64(time.Second)
		err := time.Duration(dNet - dRTP)
		if d.annErrN == 0 || err < d.annErrMin {
			d.annErrMin = err
		}
		if d.annErrN == 0 || err > d.annErrMax {
			d.annErrMax = err
		}
		d.annErrN++
	}
	d.annPrevOK, d.annPrevRTP, d.annPrevNet = true, rtp, network
}

func (d *audioDiag) onStale() {
	d.mu.Lock()
	d.staleTotal++
	d.ivStale++
	d.mu.Unlock()
}

func (d *audioDiag) onClockReset() {
	d.mu.Lock()
	d.clockResets++
	d.mu.Unlock()
}

func (d *audioDiag) onSendTiming(took time.Duration) {
	d.mu.Lock()
	if took > d.sendMax {
		d.sendMax = took
	}
	if took > audioSlowSend {
		d.sendSlow++
	}
	d.mu.Unlock()
}

func (d *audioDiag) onSent(rtp uint32, seq uint16) {
	d.mu.Lock()
	d.framesSent++
	d.ivSent++
	d.lastRTP, d.lastSeq = rtp, seq
	d.mu.Unlock()
}

// emitHealth writes one audio.health record and resets the interval counters.
func (d *audioDiag) emitHealth(now time.Time, as *AudioStream, capture *AudioCapture, clock *mediaClock) {
	budget := audioLatencyDuration(as.latencySamples)
	threshold := budget - minimumAudioSendLead // age at which a frame is dropped as stale

	d.mu.Lock()
	elapsed := now.Sub(d.lastEmit)
	rec := map[string]any{
		"uptime_s":                 math.Round(now.Sub(d.start).Seconds()*10) / 10,
		"process":                  processStats(),
		"interval_s":               math.Round(elapsed.Seconds()*10) / 10,
		"frames_read_total":        d.framesRead,
		"frames_sent_total":        d.framesSent,
		"frames_read_iv":           d.ivRead,
		"frames_sent_iv":           d.ivSent,
		"frames_stale_total":       d.staleTotal,
		"frames_stale_iv":          d.ivStale,
		"latency_budget_us":        micros(budget),
		"stale_threshold_age_us":   micros(threshold),
		"clock_resets_total":       d.clockResets,
		"clock_correction_us":      micros(d.correction),
		"last_rtp":                 d.lastRTP,
		"last_seq":                 d.lastSeq,
		"send_max_us_iv":           micros(d.sendMax),
		"send_slow_iv":             d.sendSlow,
		"read_wait_max_us_iv":      micros(d.readMax),
		"read_stall_iv":            d.readSlow,
		"announces_total":          d.announces.Load(),
		"announce_errors_total":    d.announceErrors.Load(),
		"wsaenobufs_retries_total": audioSendRetries.Load(),
	}
	if !d.lastFrameAt.IsZero() {
		rec["since_last_frame_ms"] = now.Sub(d.lastFrameAt).Milliseconds()
	}
	headroom := int64(math.MaxInt64)
	if d.ageN > 0 {
		rec["source_age_us"] = map[string]int64{
			"last": micros(d.ageLast), "min": micros(d.ageMin), "max": micros(d.ageMax), "mean": micros(d.ageSum / time.Duration(d.ageN)),
		}
		headroom = micros(threshold - d.ageMax)
		rec["age_headroom_us"] = headroom
	}
	if d.first.ok && d.last.ok && d.last.rtp != d.first.rtp {
		sampleSpan := audioSamplesDuration(uint64(d.last.rtp - d.first.rtp))
		ptsSpan := d.last.pts.Sub(d.first.pts)
		rec["sample_span_us"] = micros(sampleSpan)
		rec["pts_span_us"] = micros(ptsSpan)
		rec["sample_phase_us"] = micros(sampleSpan - ptsSpan)
	}
	driftPPM := 0.0
	if elapsed > 0 {
		driftPPM = float64(d.correction-d.prevCorrection) / float64(elapsed) * 1e6
		rec["drift_ppm"] = math.Round(driftPPM*10) / 10
	}

	disc := capture.captureDiscontinuities()
	retransReq, retransResent, retransExpired := as.retransmitRequests.Load(), as.retransmitResent.Load(), as.retransmitExpired.Load()
	retries := audioSendRetries.Load()
	rec["capture_discontinuities_total"] = disc
	rec["retransmit"] = map[string]uint64{"requests_total": retransReq, "resent_total": retransResent, "expired_total": retransExpired}

	rec["pcm"] = d.pcm.record()
	if d.annErrN > 0 {
		rec["announce_mapping_err_us"] = map[string]int64{"min": micros(d.annErrMin), "max": micros(d.annErrMax), "n_iv": int64(d.annErrN)}
	}
	fb := clock.takeFeedbackStats()
	if fb.count > 0 {
		rec["receiver_feedback"] = map[string]int64{
			"n_iv": int64(fb.count), "clamped_iv": int64(fb.clamped), "phase_min_us": micros(fb.min), "phase_max_us": micros(fb.max),
			"clock_step_max_us": micros(fb.maxStep), "clock_advance_us": micros(fb.advance),
		}
	}

	var flags []string
	status := "ok"
	degrade := func(flag string) {
		flags = append(flags, flag)
		if status == "ok" {
			status = "degraded"
		}
	}
	fail := func(flag string) {
		flags = append(flags, flag)
		status = "failing"
	}
	if d.ivStale > 0 {
		fail("stale_dropping")
	}
	if d.ivSent == 0 && elapsed >= audioHealthInterval/2 {
		fail("no_frames_sent")
	}
	if d.ageN > 0 && headroom < micros(20*time.Millisecond) && d.ivStale == 0 {
		degrade("age_headroom_low")
	}
	if d.sendSlow > 0 {
		degrade("send_slow")
	}
	if d.readSlow > 0 {
		degrade("capture_read_stall")
	}
	if disc > d.prevDisc {
		degrade("capture_discontinuity")
	}
	if d.clockResets > d.prevResets {
		degrade("clock_reset")
	}
	if retransReq > d.prevRetransReq {
		degrade("receiver_requested_retransmit")
	}
	if retransExpired > d.prevRetransExpired {
		degrade("retransmit_history_expired")
	}
	if retries > d.prevRetries {
		degrade("send_buffer_pressure")
	}
	if d.annErrN > 0 && (d.annErrMax > audioAnnounceJump || d.annErrMin < -audioAnnounceJump) {
		degrade("announce_mapping_jump")
	}
	if d.pcm.jumps > 0 {
		degrade("pcm_step_jump")
	}
	if d.pcm.clipped >= 8 {
		degrade("pcm_clipping")
	}
	if math.Abs(driftPPM) > 200 {
		degrade("clock_drift_fast")
	}
	if fb.count > 0 && (fb.max > 10*time.Millisecond || fb.min < -10*time.Millisecond) {
		degrade("receiver_feedback_phase_large")
	}
	rec["status"] = status
	rec["flags"] = append([]string{}, flags...)

	d.prevDisc, d.prevResets, d.prevRetransReq, d.prevRetransExpired, d.prevRetries = disc, d.clockResets, retransReq, retransExpired, retries
	d.prevCorrection = d.correction
	d.lastEmit = now
	d.ivRead, d.ivSent, d.ivStale = 0, 0, 0
	d.ageN, d.ageSum = 0, 0
	d.sendMax, d.readMax, d.sendSlow, d.readSlow = 0, 0, 0, 0
	d.first = d.last
	d.annErrN, d.annErrMin, d.annErrMax = 0, 0, 0
	d.pcm = audioPCMInterval{}
	d.mu.Unlock()

	diagEmit("audio.health", rec)
}

// feedbackStats summarizes receiver clock feedback since the previous call.
type feedbackStats struct {
	count, clamped int
	min, max       time.Duration
	maxStep        time.Duration // largest forward move applied to the shared clock
	advance        time.Duration // total forward movement applied

	// Survive takeFeedbackStats: state for per-sample change detection.
	total                  uint64
	prevOK                 bool
	prevPhase, prevOffset  time.Duration
}

// recordFeedback also emits one clock.feedback record per receiver clock
// sample (every /feedback, ~2 s): the sample's phase against the running
// mapping, what was applied, and the raw receiver-minus-local boot clock
// offset with its change since the previous sample. The offset includes the
// response's one-way network delay, so jitter of a few ms is expected; a step
// in offset_change_us with small rtt (see the matching rtsp.exchange) is a
// receiver clock step.
func (c *mediaClock) recordFeedback(phase, applied time.Duration, clamped bool) {
	c.mu.Lock()
	rec := map[string]any{
		"phase_us": micros(phase), "applied_us": micros(applied), "clamped": clamped,
		"phase_ge_step_threshold": phase >= mediaClockStepThreshold || phase <= -mediaClockStepThreshold,
		"timeline_id":             fmt.Sprintf("0x%016x", c.timelineID),
		"anchor_ts":               fmt.Sprintf("0x%016x", c.anchorTimestamp),
	}
	if !c.anchorLocal.IsZero() {
		receiver := time.Duration(ptpNanoseconds(c.anchorTimestamp)) - applied + phase
		local := bootRelativeNow() - time.Since(c.anchorLocal)
		offset := receiver - local
		rec["receiver_minus_local_us"] = micros(offset)
		if c.fb.prevOK {
			rec["offset_change_us"] = micros(offset - c.fb.prevOffset)
			rec["phase_change_us"] = micros(phase - c.fb.prevPhase)
		}
		c.fb.prevOK, c.fb.prevOffset, c.fb.prevPhase = true, offset, phase
	}
	c.fb.total++
	rec["n_total"] = c.fb.total
	if c.fb.count == 0 || phase < c.fb.min {
		c.fb.min = phase
	}
	if c.fb.count == 0 || phase > c.fb.max {
		c.fb.max = phase
	}
	if applied > c.fb.maxStep {
		c.fb.maxStep = applied
	}
	c.fb.advance += applied
	c.fb.count++
	if clamped {
		c.fb.clamped++
	}
	c.mu.Unlock()
	diagEmit("clock.feedback", rec)
}

func (c *mediaClock) takeFeedbackStats() feedbackStats {
	if c == nil {
		return feedbackStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stats := c.fb
	c.fb = feedbackStats{total: stats.total, prevOK: stats.prevOK, prevPhase: stats.prevPhase, prevOffset: stats.prevOffset}
	return stats
}

// processStats reports Go-side growth indicators so a slow leak or goroutine
// pile-up shows as a trend in the health records instead of a guess.
func processStats() map[string]any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return map[string]any{
		"goroutines":    runtime.NumGoroutine(),
		"heap_alloc_kb": m.HeapAlloc / 1024,
		"heap_sys_kb":   m.HeapSys / 1024,
		"gc_total":      m.NumGC,
		"gc_pause_max_us": func() uint64 {
			var max uint64
			for _, p := range m.PauseNs {
				if p > max {
					max = p
				}
			}
			return max / 1000
		}(),
	}
}
