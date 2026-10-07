package airplay

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync/atomic"
	"time"
)

// Audio data-plane diagnostics.
//
// The media loop only performs atomic counter/histogram updates per packet
// (no formatting, no locks, no allocation). A separate goroutine emits one
// audio.wire record per second and drains anomaly events, rate-limited.
//
// kind values:
//
//	audio.socket        once per session: sockets, buffer sizes, TOS, codec/security mode
//	audio.wire          per audioWireInterval aggregate of the data socket
//	audio.wire_anomaly  a fresh frame's seq step != 1 or RTP step != spf (rate-limited)
//	audio.ctrl_rx       one control-socket datagram from the receiver (rate-limited)
//	audio.ctrl_closed   the control-socket reader ended
//	audio.time_announce one TimeAnnounce (sync) packet we sent
const (
	audioWireInterval      = time.Second
	audioWireBurstWindow   = time.Millisecond
	audioWireBurstPackets  = 3 // a burst is more than this many packets within audioWireBurstWindow
	audioWireAnomalyPerSec = 20
	audioCtrlRxPerSec      = 100
	audioAssumedMTU        = 1500
)

// Histogram upper bounds (exclusive); the last bucket is open-ended.
var (
	audioGapBucketBoundsUS  = [...]int64{1000, 2000, 5000, 10000, 20000, 50000}
	audioGapBucketNames     = [...]string{"lt1ms", "1_2ms", "2_5ms", "5_10ms", "10_20ms", "20_50ms", "ge50ms"}
	audioSendBucketBoundsUS = [...]int64{50, 100, 250, 500, 1000, 5000}
	audioSendBucketNames    = [...]string{"lt50us", "50_100us", "100_250us", "250_500us", "500us_1ms", "1_5ms", "ge5ms"}
)

const audioHistBuckets = 7

// audioHistBucket returns the bucket index of v for the given upper bounds.
func audioHistBucket(bounds []int64, v int64) int {
	for i, b := range bounds {
		if v < b {
			return i
		}
	}
	return len(bounds)
}

type audioWireAnomaly struct {
	at                time.Time
	kind              string // "seq_step" or "rtp_step"
	seq, prevSeq      uint16
	rtp, prevRTP      uint32
	seqStep, rtpStep  int64
	expectedRTPStep   int64
	sinceLastFreshUS  int64
	frameBytes        int
}

// audioWireStats is written by exactly one goroutine (the media loop) and
// read/reset by the emitter goroutine through atomics.
type audioWireStats struct {
	spf uint32

	// Media-loop-only state.
	lastSend, lastFresh time.Time
	burstStart          time.Time
	burstPackets        int
	burstCounted        bool
	haveFresh           bool
	prevSeq             uint16
	prevRTP             uint32

	packets, freshPackets, fecPackets, bytes, sendErrors atomic.Uint64
	gapHist, sendHist                                    [audioHistBuckets]atomic.Uint64
	gapMaxUS, sendMaxUS                                  atomic.Int64
	bursts, burstMaxPackets                              atomic.Uint64
	frameMin, frameMax, frameSum, frameN                 atomic.Int64
	seqAnomalies, rtpAnomalies                           atomic.Uint64 // cumulative
	lastSeq, lastRTP                                     atomic.Uint32

	anomalies       chan audioWireAnomaly
	anomalyDropped  atomic.Uint64
	anomalyLimiter  diagRateLimiter
	prevRetries     uint64
	prevRetransReq  uint64
	lastEmit, start time.Time
}

func newAudioWireStats(spf uint32, now time.Time) *audioWireStats {
	return &audioWireStats{
		spf:            spf,
		anomalies:      make(chan audioWireAnomaly, 64),
		anomalyLimiter: diagRateLimiter{limit: audioWireAnomalyPerSec},
		prevRetries:    audioSendRetries.Load(),
		lastEmit:       now,
		start:          now,
	}
}

func atomicMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// atomicMinPositive keeps the minimum of positive values; zero means unset.
func atomicMinPositive(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if (cur != 0 && cur <= v) || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// onPacket records one datagram write on the data socket (fresh or FEC copy).
// Media-loop only; atomics only.
func (w *audioWireStats) onPacket(started time.Time, took time.Duration, wireBytes int, fec bool, err error) {
	if err != nil {
		w.sendErrors.Add(1)
		return
	}
	w.packets.Add(1)
	w.bytes.Add(uint64(wireBytes))
	if fec {
		w.fecPackets.Add(1)
	}
	tookUS := took.Microseconds()
	w.sendHist[audioHistBucket(audioSendBucketBoundsUS[:], tookUS)].Add(1)
	atomicMax(&w.sendMaxUS, tookUS)

	if w.burstStart.IsZero() || started.Sub(w.burstStart) >= audioWireBurstWindow || started.Before(w.burstStart) {
		w.burstStart, w.burstPackets, w.burstCounted = started, 0, false
	}
	w.burstPackets++
	if w.burstPackets > audioWireBurstPackets {
		if !w.burstCounted {
			w.bursts.Add(1)
			w.burstCounted = true
		}
		for {
			cur := w.burstMaxPackets.Load()
			if uint64(w.burstPackets) <= cur || w.burstMaxPackets.CompareAndSwap(cur, uint64(w.burstPackets)) {
				break
			}
		}
	}
	w.lastSend = started
}

// onFresh records one newly sent media frame (not an FEC copy): send gap,
// frame size and seq/RTP continuity. Media-loop only.
func (w *audioWireStats) onFresh(now time.Time, seq uint16, rtp uint32, frameBytes int) {
	w.freshPackets.Add(1)
	fb := int64(frameBytes)
	atomicMinPositive(&w.frameMin, fb)
	atomicMax(&w.frameMax, fb)
	w.frameSum.Add(fb)
	w.frameN.Add(1)
	w.lastSeq.Store(uint32(seq))
	w.lastRTP.Store(rtp)

	var gapUS int64
	if w.haveFresh {
		gapUS = now.Sub(w.lastFresh).Microseconds()
		w.gapHist[audioHistBucket(audioGapBucketBoundsUS[:], gapUS)].Add(1)
		atomicMax(&w.gapMaxUS, gapUS)
		seqStep := int64(int16(seq - w.prevSeq))
		rtpStep := int64(int32(rtp - w.prevRTP))
		if seqStep != 1 || rtpStep != int64(w.spf) {
			kind := "rtp_step"
			if seqStep != 1 {
				kind = "seq_step"
				w.seqAnomalies.Add(1)
			} else {
				w.rtpAnomalies.Add(1)
			}
			select {
			case w.anomalies <- audioWireAnomaly{at: now, kind: kind, seq: seq, prevSeq: w.prevSeq, rtp: rtp, prevRTP: w.prevRTP,
				seqStep: seqStep, rtpStep: rtpStep, expectedRTPStep: int64(w.spf), sinceLastFreshUS: gapUS, frameBytes: frameBytes}:
			default:
				w.anomalyDropped.Add(1)
			}
		}
	}
	w.haveFresh, w.lastFresh, w.prevSeq, w.prevRTP = true, now, seq, rtp
}

func histMap(names [audioHistBuckets]string, hist *[audioHistBuckets]atomic.Uint64) map[string]uint64 {
	out := make(map[string]uint64, audioHistBuckets)
	for i := range hist {
		out[names[i]] = hist[i].Swap(0)
	}
	return out
}

// emit writes one audio.wire record and resets the interval counters.
func (w *audioWireStats) emit(now time.Time, as *AudioStream) {
	rec := map[string]any{
		"interval_ms":         now.Sub(w.lastEmit).Milliseconds(),
		"uptime_s":            math.Round(now.Sub(w.start).Seconds()*10) / 10,
		"packets_iv":          w.packets.Swap(0),
		"fresh_packets_iv":    w.freshPackets.Swap(0),
		"fec_packets_iv":      w.fecPackets.Swap(0),
		"bytes_iv":            w.bytes.Swap(0),
		"send_errors_iv":      w.sendErrors.Swap(0),
		"gap_hist_iv":         histMap(audioGapBucketNames, &w.gapHist),
		"gap_max_us_iv":       w.gapMaxUS.Swap(0),
		"send_hist_iv":        histMap(audioSendBucketNames, &w.sendHist),
		"send_max_us_iv":      w.sendMaxUS.Swap(0),
		"bursts_iv":           w.bursts.Swap(0),
		"burst_max_pkts_iv":   w.burstMaxPackets.Swap(0),
		"seq_anomalies_total": w.seqAnomalies.Load(),
		"rtp_anomalies_total": w.rtpAnomalies.Load(),
		"last_seq":            w.lastSeq.Load(),
		"last_rtp":            w.lastRTP.Load(),
		"anomalies_dropped_total":  w.anomalyDropped.Load(),
		"anomalies_suppressed_iv":  w.anomalyLimiter.pending(),
	}
	n := w.frameN.Swap(0)
	sum := w.frameSum.Swap(0)
	fmin, fmax := w.frameMin.Swap(0), w.frameMax.Swap(0)
	if n > 0 {
		rec["frame_bytes_iv"] = map[string]int64{"min": fmin, "max": fmax, "mean": sum / n, "n": n}
	}
	retries := audioSendRetries.Load()
	rec["wsaenobufs_retries_iv"] = retries - w.prevRetries
	w.prevRetries = retries
	if as != nil {
		req := as.retransmitRequests.Load()
		rec["retransmit_requests_iv"] = req - w.prevRetransReq
		w.prevRetransReq = req
	}
	w.lastEmit = now
	diagEmit("audio.wire", rec)
}

func (w *audioWireStats) emitAnomaly(a audioWireAnomaly) {
	ok, suppressed := w.anomalyLimiter.allow(a.at)
	if !ok {
		return
	}
	diagEmit("audio.wire_anomaly", map[string]any{
		"anomaly": a.kind, "seq": a.seq, "prev_seq": a.prevSeq, "seq_step": a.seqStep,
		"rtp": a.rtp, "prev_rtp": a.prevRTP, "rtp_step": a.rtpStep, "expected_rtp_step": a.expectedRTPStep,
		"rtp_step_err_samples": a.rtpStep - a.expectedRTPStep, "since_last_fresh_us": a.sinceLastFreshUS,
		"frame_bytes": a.frameBytes, "suppressed_n": suppressed, "detected_at": a.at.Format(time.RFC3339Nano),
	})
}

// run emits audio.wire every interval and anomalies as they arrive.
func (w *audioWireStats) run(ctx context.Context, as *AudioStream) {
	ticker := time.NewTicker(audioWireInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.emit(time.Now(), as)
			return
		case now := <-ticker.C:
			w.emit(now, as)
		case a := <-w.anomalies:
			w.emitAnomaly(a)
		}
	}
}

// audioCtrlPacketRecord decodes one control-socket datagram for audio.ctrl_rx.
// Control packets carry no media or key material; unknown types include the
// hex of their first 64 bytes.
func audioCtrlPacketRecord(p []byte, maybeTruncated bool) map[string]any {
	rec := map[string]any{"len": len(p)}
	if maybeTruncated {
		rec["maybe_truncated"] = true
	}
	if len(p) < 2 {
		rec["type"] = "short"
		rec["hex"] = diagHexPrefix(p, 64)
		return rec
	}
	rec["type_byte"] = fmt.Sprintf("0x%02x", p[1])
	rec["version"] = p[0] >> 6
	rec["b0"] = fmt.Sprintf("0x%02x", p[0])
	be16 := func(off int) uint16 { return binary.BigEndian.Uint16(p[off : off+2]) }
	be32 := func(off int) uint32 { return binary.BigEndian.Uint32(p[off : off+4]) }
	be64 := func(off int) uint64 { return binary.BigEndian.Uint64(p[off : off+8]) }
	switch {
	case p[1] == audioRetransmitRequestPayloadType && len(p) >= 8:
		rec["type"] = "retransmit_request"
		rec["request_seq"], rec["first_seq"], rec["count"] = be16(2), be16(4), be16(6)
		if len(p) != 8 {
			rec["hex"] = diagHexPrefix(p, 64)
		}
	case p[1] == audioRetransmitResponsePayloadType && len(p) >= 6:
		rec["type"] = "retransmit_response"
		rec["request_seq"], rec["seq"] = be16(2), be16(4)
	case p[1] == audioSyncPayloadTypeNTP && len(p) >= 20:
		rec["type"] = "sync_ntp"
		rec["seq"], rec["sync_rtp"], rec["network_ts"], rec["rtp"] = be16(2), be32(4), fmt.Sprintf("0x%016x", be64(8)), be32(16)
	case p[1] == audioSyncPayloadTypePTP && len(p) >= 28:
		rec["type"] = "sync_ptp"
		rec["seq"], rec["sync_rtp"], rec["network_ns"], rec["rtp"], rec["timeline_id"] = be16(2), be32(4), be64(8), be32(16), fmt.Sprintf("0x%016x", be64(20))
	case p[1] >= 200 && p[1] <= 207 && len(p) >= 4:
		// RTCP compound packet: decode each header and receiver report blocks.
		rec["type"] = "rtcp"
		var parts []map[string]any
		for off := 0; off+4 <= len(p) && len(parts) < 8; {
			length := (int(be16(off+2)) + 1) * 4
			part := map[string]any{"pt": p[off+1], "count": p[off] & 0x1f, "bytes": length}
			if (p[off+1] == 201 || p[off+1] == 200) && off+8 <= len(p) {
				part["ssrc"] = be32(off + 4)
				blocks := off + 8
				if p[off+1] == 200 {
					blocks = off + 28
				}
				for i := 0; i < int(p[off]&0x1f) && blocks+24 <= len(p) && blocks+24 <= off+length; i++ {
					part[fmt.Sprintf("rb%d", i)] = map[string]any{
						"ssrc": be32(blocks), "fraction_lost": p[blocks+4], "cumulative_lost": be32(blocks+4) & 0xffffff,
						"ext_highest_seq": be32(blocks + 8), "jitter": be32(blocks + 12), "lsr": be32(blocks + 16), "dlsr": be32(blocks + 20),
					}
					blocks += 24
				}
			}
			parts = append(parts, part)
			if length <= 0 {
				break
			}
			off += length
		}
		rec["rtcp"] = parts
		rec["hex"] = diagHexPrefix(p, 64)
	default:
		rec["type"] = "unknown"
		rec["hex"] = diagHexPrefix(p, 64)
	}
	if len(p) >= 4 {
		if _, ok := rec["seq"]; !ok && rec["type"] != "rtcp" {
			rec["hdr_seq"] = be16(2)
		}
	}
	return rec
}

// wirePacketLen is the datagram size for a payload of n bytes.
func (as *AudioStream) wirePacketLen(n int) int {
	size := 12 + n
	if as.chachaCipher != nil {
		size += as.chachaCipher.Overhead() + audioChaChaNonceSize
	}
	return size
}

func diagAddr(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

// emitAudioSocket writes the one-time audio.socket record.
func emitAudioSocket(as *AudioStream, timingProtocol string, fec bool) {
	security := "none"
	switch {
	case as.chachaCipher != nil:
		security = "chacha20-poly1305-64x64"
	case as.cipher != nil:
		security = "aes-128-cbc"
	}
	rec := map[string]any{
		"codec_ct": as.ct, "spf": as.spf, "latency_samples": as.latencySamples, "ssrc": as.ssrc,
		"security": security, "fec": fec, "timing_protocol": timingProtocol,
		"mtu_assumed": audioAssumedMTU, "max_frame_payload_bytes": 8192,
		"retransmit_history_packets": audioRetransmitHistoryPackets,
	}
	if as.chachaCipher != nil {
		rec["chacha_nonce_mode"] = as.chachaNonceMode.String()
		rec["chacha_aad_mode"] = as.chachaAADMode.String()
	}
	if as.conn != nil {
		rec["data_local"] = diagAddr(as.conn.LocalAddr())
		rec["data_sockopt"] = audioSocketOptions(as.conn)
	}
	if as.remoteAddr != nil {
		rec["data_remote"] = as.remoteAddr.String()
	}
	if as.ctrlConn != nil {
		rec["ctrl_local"] = diagAddr(as.ctrlConn.LocalAddr())
		rec["ctrl_sockopt"] = audioSocketOptions(as.ctrlConn)
	}
	if as.ctrlAddr != nil {
		rec["ctrl_remote"] = as.ctrlAddr.String()
	}
	diagEmit("audio.socket", rec)
}
