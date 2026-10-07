package airplay

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"howett.net/plist"
)

// Structured video, shared-clock and session diagnostics ([DIAG] records, see
// audio_diag.go for the format and unit conventions).
//
// kind values:
//
//	video.health            every videoHealthInterval from the session's own
//	                        ticker: frame/byte counters, histograms, PTS age,
//	                        queue depth, encoder settings, Go runtime stats
//	video.gap               > videoGapAnomaly between encoded frames arriving
//	video.idr_large         an IDR of at least videoLargeIDRBytes, with send time
//	video.send_slow         one frame write took at least videoSlowSend
//	video.send_error        a frame write failed
//	video.resolution_change the encoded size changed mid-session
//	video.sink_detached     a fan-out sink exceeded its queue budget
//	clock.feedback          every POST /feedback exchange (record=exchange: times,
//	                        decoded response, raw clock sample, reanchor result)
//	clock.state             every second: mapping, projection, wall/boot steps
//	clock.wall_step         the wall clock moved relative to monotonic time
//	clock.timeline_change   the receiver PTP timeline id changed
//	clock.timeline_update   the receiver sent new timingPeerInfo on the event channel
//	video.ts_step           mapped video timestamp stepped against capture time
//	session.negotiated      SETUP/RECORD/SET_PARAMETER/TEARDOWN summaries; keys,
//	                        IVs, FairPlay and pairing material are redacted and
//	                        binary values are reduced to their length
//
// Anomaly kinds are rate-limited to diagAnomaliesPerSec per kind; the next
// record of that kind carries suppressed_n. Per-frame work is limited to
// counter and histogram updates under an uncontended mutex; JSON is produced
// only by the ticker goroutine.
const (
	videoHealthInterval  = time.Second
	videoGapAnomaly      = 100 * time.Millisecond
	videoLargeIDRBytes   = 256 * 1024
	videoSlowSend        = 25 * time.Millisecond
	videoLateMargin      = 5 * time.Millisecond
	diagAnomaliesPerSec  = 20
	clockWallStepAnomaly = 10 * time.Millisecond
	tcpSegmentEstimate   = 1448
	// A mapped video timestamp moving this far from its capture-time delta
	// between consecutive frames is a visible step in the shared clock mapping.
	videoTimestampStepAnomaly = 5 * time.Millisecond
)

// diagHistogram counts values into buckets bounded by ascending exclusive
// upper limits; the final bucket holds everything at or above the last limit.
type diagHistogram struct {
	upper  []int64
	labels []string
	counts []uint64
}

func newDiagHistogram(unit string, upper ...int64) diagHistogram {
	labels := make([]string, len(upper)+1)
	for i := range labels {
		switch {
		case i == 0:
			labels[i] = fmt.Sprintf("lt%d%s", upper[0], unit)
		case i == len(upper):
			labels[i] = fmt.Sprintf("ge%d%s", upper[i-1], unit)
		default:
			labels[i] = fmt.Sprintf("%d_%d%s", upper[i-1], upper[i], unit)
		}
	}
	return diagHistogram{upper: upper, labels: labels, counts: make([]uint64, len(upper)+1)}
}

func (h *diagHistogram) bucket(v int64) int {
	for i, limit := range h.upper {
		if v < limit {
			return i
		}
	}
	return len(h.upper)
}

func (h *diagHistogram) observe(v int64) { h.counts[h.bucket(v)]++ }

func (h *diagHistogram) snapshot() map[string]uint64 {
	out := make(map[string]uint64, len(h.counts))
	for i, n := range h.counts {
		out[h.labels[i]] = n
	}
	return out
}

func (h *diagHistogram) reset() {
	for i := range h.counts {
		h.counts[i] = 0
	}
}

// diagAnomalyLimiter allows at most perSec records per kind per one-second
// window and reports how many were suppressed since the last allowed one.
type diagAnomalyLimiter struct {
	perSec       int
	kinds        map[string]*diagAnomalyWindow
	suppressedIV uint64
}

type diagAnomalyWindow struct {
	start      time.Time
	n          int
	suppressed int
}

func newDiagAnomalyLimiter(perSec int) *diagAnomalyLimiter {
	return &diagAnomalyLimiter{perSec: perSec, kinds: map[string]*diagAnomalyWindow{}}
}

func (l *diagAnomalyLimiter) allow(kind string, now time.Time) (bool, int) {
	w := l.kinds[kind]
	if w == nil {
		w = &diagAnomalyWindow{start: now}
		l.kinds[kind] = w
	}
	if now.Sub(w.start) >= time.Second {
		w.start, w.n = now, 0
	}
	if w.n >= l.perSec {
		w.suppressed++
		l.suppressedIV++
		return false, 0
	}
	w.n++
	suppressed := w.suppressed
	w.suppressed = 0
	return true, suppressed
}

func (l *diagAnomalyLimiter) takeSuppressed() uint64 {
	n := l.suppressedIV
	l.suppressedIV = 0
	return n
}

type diagAnomaly struct {
	kind   string
	at     time.Time
	fields map[string]any
}

// videoDiag accumulates per-interval statistics for one mirror session.
type videoDiag struct {
	mu       sync.Mutex
	start    time.Time
	lastEmit time.Time
	capture  *ScreenCapture

	auTotal, sentTotal, codecTotal, idrTotal, bytesTotal, unprimedTotal, sendErrTotal, lateTotal uint64

	ivAU, ivSent, ivCodec, ivIDR, ivBytes, ivIDRBytes, ivUnprimed, ivSendErr, ivLate, ivWrites uint64
	ivIDRMax, ivFrameMax                                                                       int
	sizeHist, outGapHist, ptsGapHist, sendHist                                                 diagHistogram
	ivOutGapMax, ivPTSGapMax, ivSendMax, ivReadWaitMax                                         time.Duration
	ageMin, ageMax, ageSum, lateAgeMax                                                         time.Duration
	ageN                                                                                       int

	tsClampTotal, ivTSClamp  uint64
	ivTSStepMin, ivTSStepMax time.Duration
	ivTSStepN                int
	prevMapRaw               uint64
	prevMapCaptured          time.Time
	prevMapOK                bool

	lastArrival, lastPTS, lastSent time.Time
	sinceIDR, lastGOP              uint64
	width, height                  int
	codec                          VideoCodec

	anomalies      chan diagAnomaly
	anomalyDropped atomic.Uint64
}

func newVideoDiag(now time.Time) *videoDiag {
	return &videoDiag{
		start:      now,
		lastEmit:   now,
		sizeHist:   newDiagHistogram("kb", 4, 16, 64, 128, 256, 512),
		outGapHist: newDiagHistogram("ms", 20, 40, 60, 100),
		ptsGapHist: newDiagHistogram("ms", 20, 40, 60, 100),
		sendHist:   newDiagHistogram("ms", 1, 5, 10, 25, 50, 100),
		anomalies:  make(chan diagAnomaly, 128),
	}
}

// post hands an anomaly to the ticker goroutine without ever blocking.
func (d *videoDiag) post(kind string, at time.Time, fields map[string]any) {
	select {
	case d.anomalies <- diagAnomaly{kind: kind, at: at, fields: fields}:
	default:
		d.anomalyDropped.Add(1)
	}
}

func (d *videoDiag) setCapture(capture *ScreenCapture, codec VideoCodec) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.capture = capture
	d.codec = normalizeVideoCodec(codec)
	d.mu.Unlock()
}

// onAccessUnit records one encoded access unit read from the capture pipeline.
func (d *videoDiag) onAccessUnit(readStart, readEnd, pts time.Time, size int) {
	if d == nil {
		return
	}
	var gap, ptsGap time.Duration
	d.mu.Lock()
	d.auTotal++
	d.ivAU++
	if wait := readEnd.Sub(readStart); wait > d.ivReadWaitMax {
		d.ivReadWaitMax = wait
	}
	if !d.lastArrival.IsZero() {
		gap = readEnd.Sub(d.lastArrival)
		d.outGapHist.observe(gap.Milliseconds())
		if gap > d.ivOutGapMax {
			d.ivOutGapMax = gap
		}
	}
	if !pts.IsZero() && !d.lastPTS.IsZero() {
		ptsGap = pts.Sub(d.lastPTS)
		d.ptsGapHist.observe(ptsGap.Milliseconds())
		if ptsGap > d.ivPTSGapMax {
			d.ivPTSGapMax = ptsGap
		}
	}
	d.lastArrival = readEnd
	if !pts.IsZero() {
		d.lastPTS = pts
	}
	d.mu.Unlock()
	if gap > videoGapAnomaly || ptsGap > videoGapAnomaly {
		d.post("video.gap", readEnd, map[string]any{
			"arrival_gap_ms": gap.Milliseconds(),
			"pts_gap_ms":     ptsGap.Milliseconds(),
			"read_wait_ms":   readEnd.Sub(readStart).Milliseconds(),
			"au_bytes":       size,
		})
	}
}

// onUnprimed records an access unit dropped before the first decodable IDR.
func (d *videoDiag) onUnprimed() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.unprimedTotal++
	d.ivUnprimed++
	d.mu.Unlock()
}

// onPTSAge records the age of a frame's capture PTS when it is mapped to the
// receiver clock, against the session's playout lead.
func (d *videoDiag) onPTSAge(age, bias time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.ageN == 0 || age < d.ageMin {
		d.ageMin = age
	}
	if d.ageN == 0 || age > d.ageMax {
		d.ageMax = age
	}
	d.ageSum += age
	d.ageN++
	if age > bias-videoLateMargin {
		d.lateTotal++
		d.ivLate++
		if age > d.lateAgeMax {
			d.lateAgeMax = age
		}
	}
	d.mu.Unlock()
}

func (d *videoDiag) onCodecFrame(codec VideoCodec, width, height, size int, at time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.codecTotal++
	d.ivCodec++
	prevW, prevH := d.width, d.height
	d.width, d.height = width, height
	d.mu.Unlock()
	if prevW != 0 && (prevW != width || prevH != height) {
		d.post("video.resolution_change", at, map[string]any{
			"codec": string(normalizeVideoCodec(codec)), "from": fmt.Sprintf("%dx%d", prevW, prevH),
			"to": fmt.Sprintf("%dx%d", width, height), "codec_config_bytes": size,
		})
	}
}

// onSend records one VCL frame write to the data socket.
func (d *videoDiag) onSend(size int, keyframe bool, start, end time.Time, err error) {
	if d == nil {
		return
	}
	took := end.Sub(start)
	d.mu.Lock()
	d.ivWrites++
	d.sendHist.observe(took.Milliseconds())
	if took > d.ivSendMax {
		d.ivSendMax = took
	}
	if err != nil {
		d.sendErrTotal++
		d.ivSendErr++
		d.mu.Unlock()
		d.post("video.send_error", end, map[string]any{"error": err.Error(), "bytes": size, "keyframe": keyframe, "write_us": micros(took)})
		return
	}
	d.sentTotal++
	d.ivSent++
	d.bytesTotal += uint64(size)
	d.ivBytes += uint64(size)
	d.sizeHist.observe(int64(size / 1024))
	if size > d.ivFrameMax {
		d.ivFrameMax = size
	}
	var gop uint64
	if keyframe {
		d.idrTotal++
		d.ivIDR++
		d.ivIDRBytes += uint64(size)
		if size > d.ivIDRMax {
			d.ivIDRMax = size
		}
		gop = d.sinceIDR
		d.lastGOP = d.sinceIDR
		d.sinceIDR = 0
	}
	d.sinceIDR++
	var sinceLast time.Duration
	if !d.lastSent.IsZero() {
		sinceLast = end.Sub(d.lastSent)
	}
	d.lastSent = end
	d.mu.Unlock()
	if keyframe && size >= videoLargeIDRBytes {
		d.post("video.idr_large", end, map[string]any{
			"bytes": size, "write_us": micros(took), "tcp_segments_est": (size + 128 + tcpSegmentEstimate - 1) / tcpSegmentEstimate,
			"gop_frames": gop, "since_prev_frame_ms": sinceLast.Milliseconds(),
		})
	}
	if took >= videoSlowSend {
		d.post("video.send_slow", end, map[string]any{"write_us": micros(took), "bytes": size, "keyframe": keyframe})
	}
}

// onFrameMap records how a frame's capture time was mapped to the receiver
// clock. step is the change of the mapped timestamp minus the change of the
// capture time since the previous frame: a nonzero value means the shared
// clock mapping (feedback re-anchor) moved between two frames. clamped is true
// when the mapped timestamp was not strictly increasing and was nudged forward.
func (d *videoDiag) onFrameMap(captured time.Time, raw, final uint64) {
	if d == nil {
		return
	}
	clamped := raw != final
	var step time.Duration
	haveStep := false
	d.mu.Lock()
	if clamped {
		d.tsClampTotal++
		d.ivTSClamp++
	}
	if d.prevMapOK {
		step = time.Duration(compactTimestampMicros(raw)-compactTimestampMicros(d.prevMapRaw))*time.Microsecond - captured.Sub(d.prevMapCaptured)
		haveStep = true
		if d.ivTSStepN == 0 || step < d.ivTSStepMin {
			d.ivTSStepMin = step
		}
		if d.ivTSStepN == 0 || step > d.ivTSStepMax {
			d.ivTSStepMax = step
		}
		d.ivTSStepN++
	}
	d.prevMapRaw, d.prevMapCaptured, d.prevMapOK = raw, captured, true
	d.mu.Unlock()
	if haveStep && (step > videoTimestampStepAnomaly || step < -videoTimestampStepAnomaly) {
		d.post("video.ts_step", time.Now(), map[string]any{"step_us": micros(step), "clamped": clamped})
	}
}

func durationStats(min, max, sum time.Duration, n int) map[string]int64 {
	return map[string]int64{"min": micros(min), "max": micros(max), "mean": micros(sum / time.Duration(n)), "n": int64(n)}
}

// emitHealth writes one video.health record and resets the interval counters.
func (d *videoDiag) emitHealth(now time.Time, bias time.Duration, suppressed uint64, final bool) {
	d.mu.Lock()
	elapsed := now.Sub(d.lastEmit)
	rec := map[string]any{
		"final":                   final,
		"uptime_s":                math.Round(now.Sub(d.start).Seconds()*10) / 10,
		"interval_s":              math.Round(elapsed.Seconds()*100) / 100,
		"process":                 processStats(),
		"codec":                   string(d.codec),
		"size":                    fmt.Sprintf("%dx%d", d.width, d.height),
		"au_read_total":           d.auTotal,
		"frames_sent_total":       d.sentTotal,
		"codec_frames_total":      d.codecTotal,
		"idr_total":               d.idrTotal,
		"kb_sent_total":           d.bytesTotal / 1024,
		"dropped_unprimed_total":  d.unprimedTotal,
		"send_errors_total":       d.sendErrTotal,
		"late_frames_total":       d.lateTotal,
		"au_read_iv":              d.ivAU,
		"frames_sent_iv":          d.ivSent,
		"codec_frames_iv":         d.ivCodec,
		"dropped_unprimed_iv":     d.ivUnprimed,
		"writes_iv":               d.ivWrites,
		"send_errors_iv":          d.ivSendErr,
		"idr_iv":                  d.ivIDR,
		"idr_kb_iv":               d.ivIDRBytes / 1024,
		"idr_max_kb_iv":           d.ivIDRMax / 1024,
		"bytes_iv":                d.ivBytes,
		"frame_max_kb_iv":         d.ivFrameMax / 1024,
		"frame_size_hist_iv":      d.sizeHist.snapshot(),
		"output_gap_hist_iv":      d.outGapHist.snapshot(),
		"output_gap_max_ms_iv":    d.ivOutGapMax.Milliseconds(),
		"pts_gap_hist_iv":         d.ptsGapHist.snapshot(),
		"pts_gap_max_ms_iv":       d.ivPTSGapMax.Milliseconds(),
		"read_wait_max_us_iv":     micros(d.ivReadWaitMax),
		"send_hist_iv":            d.sendHist.snapshot(),
		"send_max_us_iv":          micros(d.ivSendMax),
		"late_frames_iv":          d.ivLate,
		"ts_clamped_total":        d.tsClampTotal,
		"ts_clamped_iv":           d.ivTSClamp,
		"latency_budget_us":       micros(bias),
		"anomalies_suppressed_iv": suppressed,
		"anomalies_dropped_total": d.anomalyDropped.Load(),
		"gop": map[string]uint64{
			"frames_since_idr": d.sinceIDR, "last_gop_frames": d.lastGOP,
		},
	}
	if elapsed > 0 {
		rec["kbps_iv"] = math.Round(float64(d.ivBytes)*8/elapsed.Seconds()/1000*10) / 10
		rec["fps_iv"] = math.Round(float64(d.ivSent)/elapsed.Seconds()*10) / 10
	}
	if d.ageN > 0 {
		rec["pts_age_us_iv"] = durationStats(d.ageMin, d.ageMax, d.ageSum, d.ageN)
		rec["age_headroom_us"] = micros(bias - videoLateMargin - d.ageMax)
	}
	if d.ivLate > 0 {
		rec["late_age_max_us_iv"] = micros(d.lateAgeMax)
	}
	if d.ivTSStepN > 0 {
		rec["ts_map_step_us_iv"] = map[string]int64{"min": micros(d.ivTSStepMin), "max": micros(d.ivTSStepMax)}
	}
	if !d.lastArrival.IsZero() {
		rec["since_last_au_ms"] = now.Sub(d.lastArrival).Milliseconds()
	}
	if enc := latestVideoEncoder(); enc != nil {
		rec["encoder"] = enc
	}
	if q := captureQueueStats(d.capture); q != nil {
		rec["queue"] = q
	}
	var flags []string
	if d.ivSent == 0 && d.sentTotal > 0 {
		flags = append(flags, "no_frames_sent")
	}
	if d.ivOutGapMax > videoGapAnomaly {
		flags = append(flags, "output_gap")
	}
	if d.ivSendMax >= videoSlowSend {
		flags = append(flags, "send_slow")
	}
	if d.ivLate > 0 {
		flags = append(flags, "late_frames")
	}
	if d.ivSendErr > 0 {
		flags = append(flags, "send_error")
	}
	rec["flags"] = append([]string{}, flags...)

	d.lastEmit = now
	d.ivAU, d.ivSent, d.ivCodec, d.ivIDR, d.ivBytes, d.ivIDRBytes, d.ivUnprimed, d.ivSendErr, d.ivLate, d.ivWrites = 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
	d.ivIDRMax, d.ivFrameMax = 0, 0
	d.ivOutGapMax, d.ivPTSGapMax, d.ivSendMax, d.ivReadWaitMax = 0, 0, 0, 0
	d.ageMin, d.ageMax, d.ageSum, d.ageN, d.lateAgeMax = 0, 0, 0, 0, 0
	d.ivTSClamp, d.ivTSStepN, d.ivTSStepMin, d.ivTSStepMax = 0, 0, 0, 0
	d.sizeHist.reset()
	d.outGapHist.reset()
	d.ptsGapHist.reset()
	d.sendHist.reset()
	d.mu.Unlock()

	diagEmit("video.health", rec)
}

// captureQueueStats samples the fan-out queue between the shared encoder and
// this session's sender, when the session reads through a BroadcastSink.
func captureQueueStats(capture *ScreenCapture) map[string]any {
	if capture == nil || capture.frames == nil {
		return nil
	}
	sink, ok := capture.frames.(*BroadcastSink)
	if !ok {
		return nil
	}
	return sink.diagQueueStats()
}

// videoDiagLoop owns JSON emission for the session: anomalies, video.health
// and clock.state. It exits with the session context.
func (s *MirrorSession) videoDiagLoop(stop <-chan struct{}) {
	d := s.vdiag
	if d == nil {
		return
	}
	ticker := time.NewTicker(videoHealthInterval)
	defer ticker.Stop()
	limiter := newDiagAnomalyLimiter(diagAnomaliesPerSec)
	clockState := newClockStateTracker(time.Now())
	emitAnomaly := func(a diagAnomaly) {
		ok, suppressed := limiter.allow(a.kind, a.at)
		if !ok {
			return
		}
		if suppressed > 0 {
			a.fields["suppressed_n"] = suppressed
		}
		a.fields["at"] = a.at.Format(time.RFC3339Nano)
		diagEmit(a.kind, a.fields)
	}
	for {
		select {
		case <-stop:
			// One last record so the tail of the session is not lost to the tick.
			for drained := false; !drained; {
				select {
				case a := <-d.anomalies:
					emitAnomaly(a)
				default:
					drained = true
				}
			}
			d.emitHealth(time.Now(), s.timestampBias, limiter.takeSuppressed(), true)
			return
		case a := <-d.anomalies:
			emitAnomaly(a)
		case now := <-ticker.C:
			d.emitHealth(now, s.timestampBias, limiter.takeSuppressed(), false)
			for _, a := range clockState.observe(now, s.mediaClock, s.timingProtocol) {
				emitAnomaly(a)
			}
		}
	}
}

// clockStateTracker emits clock.state and detects wall-clock steps (NTP or
// manual time changes) relative to Go's monotonic clock and the boot clock.
type clockStateTracker struct {
	startMono time.Time
	startWall time.Time
	startBoot time.Duration

	prevWallMinusMono, prevBootMinusMono time.Duration
	prevAt                               time.Time
	prevProjected                        uint64
	prevReceiverMinusBoot                int64
	prevTimeline                         uint64
	prevOK                               bool
}

func newClockStateTracker(now time.Time) *clockStateTracker {
	return &clockStateTracker{startMono: now, startWall: now.Round(0), startBoot: bootRelativeNow(), prevAt: now}
}

func compactTimestampMicros(ts uint64) int64 {
	return int64(ts>>32)*1e6 + int64(((ts&0xffffffff)*1e6)>>32)
}

func (t *clockStateTracker) observe(now time.Time, clock *mediaClock, timing string) []diagAnomaly {
	var anomalies []diagAnomaly
	mono := now.Sub(t.startMono)
	wall := now.Round(0).Sub(t.startWall)
	boot := bootRelativeNow() - t.startBoot
	wallMinusMono := wall - mono
	bootMinusMono := boot - mono
	wallStep := wallMinusMono - t.prevWallMinusMono
	rec := map[string]any{
		"timing_protocol":     timing,
		"wall_minus_mono_us":  micros(wallMinusMono),
		"wall_step_us_iv":     micros(wallStep),
		"boot_minus_mono_us":  micros(bootMinusMono),
		"boot_step_us_iv":     micros(bootMinusMono - t.prevBootMinusMono),
		"mono_since_start_ms": mono.Milliseconds(),
		"mono_advance_us_iv":  micros(now.Sub(t.prevAt)),
	}
	if wallStep > clockWallStepAnomaly || wallStep < -clockWallStepAnomaly {
		anomalies = append(anomalies, diagAnomaly{kind: "clock.wall_step", at: now, fields: map[string]any{
			"wall_step_us": micros(wallStep), "wall_minus_mono_us": micros(wallMinusMono), "wall": now.Format(time.RFC3339Nano),
		}})
	}
	if clock != nil {
		snap := clock.diagSnapshot()
		rec["timeline_id"] = fmt.Sprintf("0x%016x", snap.timelineID)
		rec["feedback_total"] = snap.feedbackTotal
		rec["clamped_total"] = snap.clampedTotal
		rec["slew_limited_total"] = snap.slewLimitedTotal
		rec["forward_advance_total_us"] = micros(snap.advanceTotal)
		if !snap.anchorLocal.IsZero() {
			projected := snap.anchorTimestamp + compactTimestamp(now.Sub(snap.anchorLocal))
			receiverMinusBoot := compactTimestampMicros(projected) - bootRelativeNow().Microseconds()
			rec["anchor_local"] = snap.anchorLocal.Format(time.RFC3339Nano)
			rec["anchor_age_ms"] = now.Sub(snap.anchorLocal).Milliseconds()
			rec["anchor_timestamp"] = snap.anchorTimestamp
			rec["anchor_timestamp_us"] = compactTimestampMicros(snap.anchorTimestamp)
			rec["projected_us"] = compactTimestampMicros(projected)
			rec["receiver_minus_boot_us"] = receiverMinusBoot
			if t.prevOK && snap.timelineID == t.prevTimeline {
				projectedAdvance := compactTimestampMicros(projected) - compactTimestampMicros(t.prevProjected)
				rec["projected_advance_us_iv"] = projectedAdvance
				rec["projected_minus_mono_us_iv"] = projectedAdvance - micros(now.Sub(t.prevAt))
				rec["receiver_minus_boot_drift_us_iv"] = receiverMinusBoot - t.prevReceiverMinusBoot
			}
			if t.prevOK && snap.timelineID != t.prevTimeline {
				anomalies = append(anomalies, diagAnomaly{kind: "clock.timeline_change", at: now, fields: map[string]any{
					"from": fmt.Sprintf("0x%016x", t.prevTimeline), "to": fmt.Sprintf("0x%016x", snap.timelineID),
				}})
			}
			t.prevProjected, t.prevReceiverMinusBoot, t.prevTimeline, t.prevOK = projected, receiverMinusBoot, snap.timelineID, true
		}
	}
	t.prevWallMinusMono, t.prevBootMinusMono, t.prevAt = wallMinusMono, bootMinusMono, now
	diagEmit("clock.state", rec)
	return anomalies
}

// --- session.negotiated helpers ---

// diagSensitiveKey reports plist/header keys whose values must never be logged.
func diagSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	switch k {
	case "ekey", "eiv", "shk", "shiv", "pk", "sk", "pin", "salt", "nonce", "authtag":
		return true
	}
	if strings.HasSuffix(k, "key") || strings.HasSuffix(k, "iv") || strings.HasPrefix(k, "fp") {
		return true
	}
	for _, fragment := range []string{"fairplay", "pairing", "secret", "password", "passwd", "token", "signature", "auth", "credential", "cookie"} {
		if strings.Contains(k, fragment) {
			return true
		}
	}
	return false
}

const (
	diagSanitizeDepth  = 8
	diagSanitizeItems  = 32
	diagSanitizeString = 256
)

// diagSanitize converts a decoded plist into a log-safe value: sensitive keys
// are redacted, binary data is reduced to its length, and size is bounded.
func diagSanitize(v any) any { return diagSanitizeDepthLimited(v, 0) }

func diagSanitizeDepthLimited(v any, depth int) any {
	if depth > diagSanitizeDepth {
		return "<depth>"
	}
	switch value := v.(type) {
	case nil:
		return nil
	case map[string]interface{}:
		out := make(map[string]any, len(value))
		for k, item := range value {
			if diagSensitiveKey(k) {
				if b, ok := item.([]byte); ok {
					out[k] = fmt.Sprintf("<redacted %d bytes>", len(b))
				} else {
					out[k] = "<redacted>"
				}
				continue
			}
			out[k] = diagSanitizeDepthLimited(item, depth+1)
		}
		return out
	case []interface{}:
		n := len(value)
		if n > diagSanitizeItems {
			n = diagSanitizeItems
		}
		out := make([]any, 0, n+1)
		for _, item := range value[:n] {
			out = append(out, diagSanitizeDepthLimited(item, depth+1))
		}
		if len(value) > n {
			out = append(out, fmt.Sprintf("<%d more>", len(value)-n))
		}
		return out
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(value))
	case string:
		if len(value) > diagSanitizeString {
			return value[:diagSanitizeString] + "..."
		}
		return value
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return value
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Sprint(value)
		}
		return value
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Sprint(value)
		}
		return value
	case time.Time:
		return value.Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("<%T>", v)
	}
}

// diagSanitizeResponse sanitizes a SETUP response, summarizing the bulky
// receiver "info" dictionary instead of logging it in full.
func diagSanitizeResponse(response map[string]interface{}) any {
	if response == nil {
		return nil
	}
	trimmed := make(map[string]interface{}, len(response))
	for k, v := range response {
		if k == "info" {
			if info, ok := v.(map[string]interface{}); ok {
				trimmed[k] = fmt.Sprintf("<info: %d fields>", len(info))
				continue
			}
		}
		trimmed[k] = v
	}
	return diagSanitize(trimmed)
}

// diagSafeHeaders keeps RTSP response headers that describe timing and
// negotiation, with any credential-like header redacted.
func diagSafeHeaders(headers map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range headers {
		lower := strings.ToLower(k)
		if !strings.HasPrefix(lower, "x-apple-") && lower != "audio-latency" && lower != "server" && lower != "content-type" && lower != "session" {
			continue
		}
		if diagSensitiveKey(lower) || strings.Contains(lower, "key") {
			out[lower] = "<redacted>"
			continue
		}
		if len(v) > diagSanitizeString {
			v = v[:diagSanitizeString] + "..."
		}
		out[lower] = v
	}
	return out
}

// emitSetParameterDiag records one SET_PARAMETER request (volume, progress,
// ...) as a session.negotiated step. The body goes through the shared wire
// redaction, so only plain text parameters survive.
func emitSetParameterDiag(uri string, body []byte, took time.Duration, err error) {
	rec := map[string]any{"step": "set_parameter", "uri": uri, "rtt_us": micros(took), "body_bytes": len(body)}
	if desc, truncated := diagDescribeBody(uri, "text/parameters", body); desc != nil {
		rec["body"] = desc
		if truncated {
			rec["body_truncated"] = true
		}
	}
	if err != nil {
		rec["error"] = err.Error()
	}
	diagEmit("session.negotiated", rec)
}

// feedbackExchange is one POST /feedback round trip for clock.feedback.
type feedbackExchange struct {
	seq             uint64
	requestedAt     time.Time
	respondedAt     time.Time
	prevRequestedAt time.Time
	headers         map[string]string
	body            []byte
	err             error
	errStreak       uint64
	reanchor        mediaClockReanchor
	reanchorErr     error
	reanchored      bool
}

// diagFeedbackExchangeRecord builds the clock.feedback "exchange" record: the
// request/response times, everything the receiver reported, the raw clock
// sample and what the shared media clock did with it. mediaClock.recordFeedback
// emits a second clock.feedback record (without a "record" field) per sample
// with the receiver-minus-local offset and its change; the two are adjacent in
// the log and share the sample's phase_us.
func diagFeedbackExchangeRecord(x feedbackExchange) map[string]any {
	rtt := x.respondedAt.Sub(x.requestedAt)
	rec := map[string]any{
		"record":       "exchange",
		"seq":          x.seq,
		"requested_at": x.requestedAt.Format(time.RFC3339Nano),
		"responded_at": x.respondedAt.Format(time.RFC3339Nano),
		"rtt_us":       micros(rtt),
	}
	if !x.prevRequestedAt.IsZero() {
		rec["since_prev_request_ms"] = x.requestedAt.Sub(x.prevRequestedAt).Milliseconds()
	}
	if x.err != nil {
		rec["error"] = x.err.Error()
		rec["consecutive_errors"] = x.errStreak
		return rec
	}
	rec["response_headers"] = diagSafeHeaders(x.headers)
	rec["body_bytes"] = len(x.body)
	if len(x.body) > 0 {
		var decoded map[string]interface{}
		if _, err := plist.Unmarshal(x.body, &decoded); err == nil {
			rec["body"] = diagSanitize(decoded)
		} else {
			rec["body_format"] = "non-plist"
		}
	}
	if x.reanchorErr != nil {
		rec["reanchor_error"] = x.reanchorErr.Error()
		return rec
	}
	if !x.reanchored {
		return rec
	}
	r := x.reanchor
	rec["receiver_request_received_ms"] = r.receivedMillis
	rec["receiver_processing_ms"] = r.processingMillis
	rec["net_rtt_est_us"] = micros(rtt) - int64(r.processingMillis)*1000
	rec["sample_us"] = compactTimestampMicros(r.sample)
	rec["applied_us"] = compactTimestampMicros(r.applied)
	rec["had_anchor"] = r.hadAnchor
	if r.hadAnchor {
		rec["projected_us"] = compactTimestampMicros(r.projected)
		rec["since_anchor_ms"] = r.sinceAnchor.Milliseconds()
		rec["phase_us"] = micros(r.phase)
		rec["advance_us"] = micros(r.advance)
	}
	rec["clamped"] = r.clamped
	rec["slew_limited"] = r.slewLimited
	return rec
}
