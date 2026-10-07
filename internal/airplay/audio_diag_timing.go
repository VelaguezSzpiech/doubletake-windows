package airplay

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

// Timing-channel diagnostics (NTP-style timing on the sender's timing socket).
//
// kind values:
//
//	timing.rx      one datagram from the receiver on the timing socket
//	timing.reply   our reply to a receiver timing request
//	timing.probe   one sender-initiated timing probe we sent
//
// A receiver request (0xd2) carries the receiver's transmit time T1 in bytes
// 24..32. We cannot know the one-way delay, so request_offset_us is
// (T1 - our boot-relative time at receipt) = clock offset + one-way delay;
// offset_change_us is its change since the previous request. A receiver
// response (0xd3) to our probe carries our T1 (ref), its T2/T3; with our
// receive time T4 the classic offset ((T2-T1)+(T3-T4))/2 and delay
// (T4-T1)-(T3-T2) are computed. PTP sessions have no sender-side timing
// handler, so nothing is logged for them.
//
// Wiring (ntpTimingResponder / sendNTPTimingProbes in mirror.go):
//
//	td := newNTPTimingDiag()
//	... after ReadFrom:   td.onPacket(buf[:n], addr, time.Now())
//	... after WriteTo:    td.onReply(addr, receivedAt, now, time.Since(writeStarted), err)
//	... after probe send: td.onProbe(seq, addr, ts, err)

type ntpTimingDiag struct {
	mu            sync.Mutex
	rxTotal       uint64
	lastRx        time.Time
	prevReqOK     bool
	prevReqOffset int64 // ns
	prevOffOK     bool
	prevOffset    int64 // ns
	prevDelay     int64 // ns
	limiter       diagRateLimiter
}

func newNTPTimingDiag() *ntpTimingDiag {
	return &ntpTimingDiag{limiter: diagRateLimiter{limit: 100}}
}

// ntpFixedToNanos converts 32.32 fixed point seconds to nanoseconds.
func ntpFixedToNanos(ts uint64) int64 {
	sec := int64(ts >> 32)
	frac := int64(ts & 0xffffffff)
	return sec*int64(time.Second) + (frac*int64(time.Second))>>32
}

// ntpDiffNanos is a-b for 32.32 timestamps as signed nanoseconds.
func ntpDiffNanos(a, b uint64) int64 {
	d := int64(a - b)
	return (d>>32)*int64(time.Second) + ((d&0xffffffff)*int64(time.Second))>>32
}

func (d *ntpTimingDiag) onPacket(p []byte, from net.Addr, receivedAt time.Time) {
	if d == nil {
		return
	}
	localNTP := ntpBootTimestamp() // taken first: closest to receipt we can get
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rxTotal++
	ok, suppressed := d.limiter.allow(receivedAt)
	rec := map[string]any{"len": len(p), "from": diagAddr(from), "rx_total": d.rxTotal, "suppressed_n": suppressed,
		"local_ntp": fmt.Sprintf("0x%016x", localNTP)}
	if !d.lastRx.IsZero() {
		rec["since_prev_ms"] = receivedAt.Sub(d.lastRx).Milliseconds()
	}
	d.lastRx = receivedAt
	if len(p) < 32 || p[0] != 0x80 {
		rec["type"] = "unknown"
		rec["hex"] = diagHexPrefix(p, 64)
		if ok {
			diagEmit("timing.rx", rec)
		}
		return
	}
	rec["type_byte"] = fmt.Sprintf("0x%02x", p[1])
	rec["seq"] = binary.BigEndian.Uint16(p[2:4])
	ref, recv, xmit := binary.BigEndian.Uint64(p[8:16]), binary.BigEndian.Uint64(p[16:24]), binary.BigEndian.Uint64(p[24:32])
	rec["ref_ts"] = fmt.Sprintf("0x%016x", ref)
	rec["recv_ts"] = fmt.Sprintf("0x%016x", recv)
	rec["xmit_ts"] = fmt.Sprintf("0x%016x", xmit)
	switch p[1] {
	case 0xd2:
		rec["type"] = "request"
		offset := ntpDiffNanos(xmit, localNTP)
		rec["request_offset_us"] = offset / 1000
		if d.prevReqOK {
			rec["offset_change_us"] = (offset - d.prevReqOffset) / 1000
		}
		d.prevReqOK, d.prevReqOffset = true, offset
	case 0xd3:
		rec["type"] = "response"
		// ref = our T1, recv = receiver T2, xmit = receiver T3, localNTP = our T4.
		offset := (ntpDiffNanos(recv, ref) + ntpDiffNanos(xmit, localNTP)) / 2
		delay := ntpDiffNanos(localNTP, ref) - ntpDiffNanos(xmit, recv)
		rec["offset_us"] = offset / 1000
		rec["delay_us"] = delay / 1000
		if d.prevOffOK {
			rec["offset_change_us"] = (offset - d.prevOffset) / 1000
			rec["delay_change_us"] = (delay - d.prevDelay) / 1000
		}
		d.prevOffOK, d.prevOffset, d.prevDelay = true, offset, delay
	default:
		rec["type"] = "unknown"
		rec["hex"] = diagHexPrefix(p, 64)
	}
	if ok {
		diagEmit("timing.rx", rec)
	}
}

// onReply records our reply to a request: replyTS is the timestamp we put in
// it, took the WriteTo duration.
func (d *ntpTimingDiag) onReply(to net.Addr, receivedAt time.Time, replyTS uint64, took time.Duration, err error) {
	if d == nil {
		return
	}
	rec := map[string]any{"to": diagAddr(to), "reply_ts": fmt.Sprintf("0x%016x", replyTS), "write_us": micros(took),
		"turnaround_us": micros(time.Since(receivedAt))}
	if err != nil {
		rec["error"] = err.Error()
		rec["timeout"] = diagIsTimeout(err)
	}
	diagEmit("timing.reply", rec)
}

// onProbe records one sender-initiated probe.
func (d *ntpTimingDiag) onProbe(seq uint16, to net.Addr, ts uint64, err error) {
	if d == nil {
		return
	}
	rec := map[string]any{"seq": seq, "to": diagAddr(to), "xmit_ts": fmt.Sprintf("0x%016x", ts)}
	if err != nil {
		rec["error"] = err.Error()
	}
	diagEmit("timing.probe", rec)
}
