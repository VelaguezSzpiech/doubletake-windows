package airplay

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	audioSampleRate            = 44100
	audioChannels              = 2
	audioBytesPerSample        = 2
	audioBytesPerSampleFrame   = audioChannels * audioBytesPerSample
	audioCapturePayloadType    = 96
	audioONVIFRTPHeaderProfile = 0xabac
	maxAudioCapturePacketBytes = 65535
)

// audioPCMFrameReader is the timestamp-preserving boundary between the
// GStreamer capture process and the AirPlay audio encoder. The destination is
// always interleaved stereo S16LE PCM and PTS identifies its first sample.
type audioPCMFrameReader interface {
	ReadPCMFrame(dst []byte) (pts time.Time, err error)
}

// audioPCMFramePosition preserves both halves of the capture clock mapping.
// sourceRTP is the sample-domain position of the first PCM sample; PTS is that
// same sample's presentation time in the local monotonic clock domain.
type audioPCMFramePosition struct {
	PTS          time.Time
	SourceRTP    uint32
	HasSourceRTP bool
	// ClockCorrection is the source-clock drift already folded into PTS. It is
	// diagnostic only.
	ClockCorrection time.Duration
	// PCM describes the samples of this frame as handed to the encoder.
	PCM audioPCMStats
}

const (
	audioDriftBucket     = 5 * time.Second
	audioDriftHistory    = 60 // buckets: a five-minute fit window
	audioDriftMinBuckets = 6  // no correction until 30 s of history exists
	// The receiver slews its playback clock to follow TimeAnnounce, so the
	// correction must change smoothly: it follows a long linear fit, and its rate
	// may never leave a crystal's plausible range. A faster apparent change is a
	// real delay and must stay visible as staleness.
	audioDriftMaxSlew  = 200e-6
	audioDriftMaxSlope = 300e-6
)

// audioClockDriftTracker follows the slow divergence between the capture
// device's sample clock (which the ONVIF timestamps are derived from, with a
// wall-clock offset computed once at pipeline start) and the host wall clock.
//
// Without it, a device clock that runs slow relative to the host makes every
// source PTS fall further behind wall time. TimeAnnounce then tells the receiver
// that samples play earlier than they can possibly arrive: packets turn late,
// the receiver glitches, and finally every frame is classed stale and dropped.
//
// The minimum arrival age (now - PTS) of each five-second bucket is the delivery
// floor: scheduling jitter only adds to it, drift moves it. A least-squares line
// through five minutes of those floors gives the drift as a smooth ramp. Tracking
// the raw floor instead made the announced RTP/time mapping wobble by +-1 ms
// (smoothness, not audibility, is what the tests check).
type audioClockDriftTracker struct {
	times      [audioDriftHistory]float64 // bucket start, seconds since origin
	floors     [audioDriftHistory]float64 // bucket minimum age, microseconds
	count      int
	origin     time.Time
	bucketAt   time.Time
	bucketMin  time.Duration
	haveBucket bool

	haveFit    bool
	fitA, fitB float64 // age_us = fitA + fitB*seconds
	ref        float64 // fit value corresponding to zero correction, microseconds
	haveRef    bool
	correction time.Duration
	updatedAt  time.Time
}

func (d *audioClockDriftTracker) closeBucket() {
	if d.count == audioDriftHistory {
		copy(d.times[:], d.times[1:])
		copy(d.floors[:], d.floors[1:])
		d.count--
	}
	d.times[d.count] = d.bucketAt.Sub(d.origin).Seconds()
	d.floors[d.count] = float64(d.bucketMin.Microseconds())
	d.count++
	d.haveFit = false
	if d.count < audioDriftMinBuckets {
		return
	}
	var sx, sy, sxx, sxy float64
	n := float64(d.count)
	for i := 0; i < d.count; i++ {
		sx += d.times[i]
		sy += d.floors[i]
		sxx += d.times[i] * d.times[i]
		sxy += d.times[i] * d.floors[i]
	}
	denom := n*sxx - sx*sx
	if denom <= 0 {
		return
	}
	slope := (n*sxy - sx*sy) / denom // microseconds per second == ppm
	if limit := audioDriftMaxSlope * 1e6; slope > limit {
		slope = limit
	} else if slope < -limit {
		slope = -limit
	}
	// Re-center the line on the mean so clamping the slope keeps it on the data.
	d.fitB = slope
	d.fitA = sy/n - slope*sx/n
	d.haveFit = true
}

// observe records one packet's arrival age and returns the drift correction to
// add to its PTS.
func (d *audioClockDriftTracker) observe(now time.Time, age time.Duration) time.Duration {
	if !d.haveBucket || now.Sub(d.bucketAt) >= audioDriftBucket*audioDriftHistory {
		// First packet, or history too old to describe the present. The
		// correction carries over and the next fit is re-referenced to it.
		d.count, d.haveFit, d.haveRef = 0, false, false
		d.origin, d.bucketAt, d.bucketMin, d.haveBucket = now, now, age, true
		d.updatedAt = now
		return d.correction
	}
	if now.Sub(d.bucketAt) >= audioDriftBucket {
		d.closeBucket()
		d.bucketAt, d.bucketMin = now, age
	} else if age < d.bucketMin {
		d.bucketMin = age
	}

	dt := now.Sub(d.updatedAt)
	d.updatedAt = now
	if !d.haveFit || dt <= 0 {
		return d.correction
	}
	at := d.fitA + d.fitB*now.Sub(d.origin).Seconds()
	if !d.haveRef {
		d.ref, d.haveRef = at-float64(d.correction.Microseconds()), true
	}
	target := time.Duration((at - d.ref) * float64(time.Microsecond))
	maxStep := time.Duration(float64(dt) * audioDriftMaxSlew)
	delta := target - d.correction
	if delta > maxStep {
		delta = maxStep
	} else if delta < -maxStep {
		delta = -maxStep
	}
	d.correction += delta
	return d.correction
}

type audioPCMFramePositionReader interface {
	ReadPCMFramePosition(dst []byte) (audioPCMFramePosition, error)
}

// rtpL16PCMFrameReader consumes RFC4571-framed RTP/L16 produced by GStreamer.
// RTP packet boundaries do not need to match AirPlay codec-frame boundaries.
type rtpL16PCMFrameReader struct {
	reader io.Reader
	now    func() time.Time
	packet []byte
	pcm    []byte

	haveTimeline    bool
	timelinePTS     time.Time
	timelineRTP     uint32
	receivedSamples uint64
	consumedSamples uint64
	havePacket      bool
	sequence        uint16
	ssrc            uint32

	drift           audioClockDriftTracker
	discontinuities atomic.Uint64
}

func newRTPL16PCMFrameReader(reader io.Reader) audioPCMFrameReader {
	return newRTPL16PCMFrameReaderWithNow(reader, time.Now)
}

func newRTPL16PCMFrameReaderWithNow(reader io.Reader, now func() time.Time) *rtpL16PCMFrameReader {
	if now == nil {
		now = time.Now
	}
	return &rtpL16PCMFrameReader{reader: reader, now: now}
}

func (r *rtpL16PCMFrameReader) ReadPCMFrame(dst []byte) (time.Time, error) {
	position, err := r.ReadPCMFramePosition(dst)
	return position.PTS, err
}

func (r *rtpL16PCMFrameReader) ReadPCMFramePosition(dst []byte) (audioPCMFramePosition, error) {
	if r.reader == nil {
		return audioPCMFramePosition{}, fmt.Errorf("RTP audio: nil capture reader")
	}
	if len(dst) == 0 || len(dst)%audioBytesPerSampleFrame != 0 {
		return audioPCMFramePosition{}, fmt.Errorf("RTP audio: PCM frame size %d is not whole stereo S16 samples", len(dst))
	}

	for len(r.pcm) < len(dst) {
		if err := r.readPacket(); err != nil {
			if err == io.EOF && len(r.pcm) != 0 {
				return audioPCMFramePosition{}, io.ErrUnexpectedEOF
			}
			return audioPCMFramePosition{}, err
		}
	}

	sourceRTP := r.timelineRTP + uint32(r.consumedSamples)
	// Use the newest packet's RTP/PTS correlation. This retains slow source-clock
	// rate changes instead of extrapolating forever from the first packet.
	pts := r.timelinePTS.Add(audioSamplesDuration(uint64(uint32(sourceRTP - r.timelineRTP))))
	copy(dst, r.pcm[:len(dst)])
	r.pcm = r.pcm[len(dst):]
	r.consumedSamples += uint64(len(dst) / audioBytesPerSampleFrame)
	return audioPCMFramePosition{PTS: pts, SourceRTP: sourceRTP, HasSourceRTP: true, ClockCorrection: r.drift.correction}, nil
}

func (r *rtpL16PCMFrameReader) readPacket() error {
	packet, err := r.readRFC4571Packet()
	if err != nil {
		return err
	}
	header, payload, err := parseTimestampedL16RTPPacket(packet)
	if err != nil {
		return err
	}
	if len(payload)%audioBytesPerSampleFrame != 0 {
		return fmt.Errorf("RTP audio: L16 payload size %d is not whole stereo samples", len(payload))
	}
	packetSamples := uint64(len(payload) / audioBytesPerSampleFrame)
	if packetSamples == 0 {
		return fmt.Errorf("RTP audio: empty L16 payload")
	}

	wallPTS := timeFromNTP(header.onvifTimestamp)
	now := r.now()
	// The ONVIF wall offset was fixed at pipeline start; follow the device
	// clock's drift against the host so PTS keeps meaning capture time.
	// Express PTS on the monotonic clock (now carries a reading, wallPTS does not):
	// mediaClock anchors on it, and mixing in wall-clock arithmetic made the
	// announced mapping step whenever the host wall clock was being disciplined.
	arrivalAge := now.Sub(wallPTS)
	packetPTS := now.Add(r.drift.observe(now, arrivalAge) - arrivalAge)
	discontinuous := !r.haveTimeline
	if r.haveTimeline {
		expectedRTP := r.timelineRTP + uint32(r.receivedSamples)
		expectedPTS := r.timelinePTS.Add(audioSamplesDuration(r.receivedSamples))
		ptsDeltaSamples := durationToAudioSamples(packetPTS.Sub(expectedPTS))
		discontinuous = !r.havePacket || header.sequence != r.sequence+1 || header.ssrc != r.ssrc ||
			header.timestamp != expectedRTP
		if discontinuous {
			r.discontinuities.Add(1)
			diagEmit("audio.capture_discontinuity", map[string]any{
				"seq_from": r.sequence, "seq_to": header.sequence,
				"rtp_expected": expectedRTP, "rtp_got": header.timestamp,
				"pts_delta_samples": ptsDeltaSamples, "partial_pcm_bytes_dropped": len(r.pcm),
				"ssrc_changed": header.ssrc != r.ssrc,
			})
		}
	}
	if !discontinuous {
		// Rebase the correlation without disturbing buffered sample positions.
		// The source RTP clock is authoritative for sample progression, while PTS
		// tells us where that sample boundary lies on the system/network timeline.
		consumedBeforePacket := r.receivedSamples
		r.timelinePTS = packetPTS.Add(-audioSamplesDuration(consumedBeforePacket))
	}
	if discontinuous {
		r.pcm = r.pcm[:0]
		r.timelinePTS = packetPTS
		r.timelineRTP = header.timestamp
		r.receivedSamples = 0
		r.consumedSamples = 0
		r.haveTimeline = true
	}

	// RTP L16 is network byte order. Both built-in ALAC and optional FDK-AAC
	// encoders consume the original interleaved S16LE representation.
	for i := 0; i < len(payload); i += audioBytesPerSample {
		r.pcm = append(r.pcm, payload[i+1], payload[i])
	}
	r.receivedSamples += packetSamples
	r.havePacket = true
	r.sequence = header.sequence
	r.ssrc = header.ssrc
	return nil
}

func (r *rtpL16PCMFrameReader) readRFC4571Packet() ([]byte, error) {
	var lengthBytes [2]byte
	if _, err := io.ReadFull(r.reader, lengthBytes[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("RTP audio: truncated RFC4571 length: %w", err)
		}
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(lengthBytes[:]))
	if length == 0 {
		return nil, fmt.Errorf("RTP audio: zero-length RFC4571 packet")
	}
	if length > maxAudioCapturePacketBytes {
		return nil, fmt.Errorf("RTP audio: packet length %d exceeds %d", length, maxAudioCapturePacketBytes)
	}
	if cap(r.packet) < length {
		r.packet = make([]byte, length)
	} else {
		r.packet = r.packet[:length]
	}
	if _, err := io.ReadFull(r.reader, r.packet); err != nil {
		return nil, fmt.Errorf("RTP audio: truncated RFC4571 packet: %w", err)
	}
	return r.packet, nil
}

type timestampedL16RTPHeader struct {
	sequence       uint16
	timestamp      uint32
	ssrc           uint32
	onvifTimestamp uint64
}

func parseTimestampedL16RTPPacket(packet []byte) (timestampedL16RTPHeader, []byte, error) {
	var header timestampedL16RTPHeader
	if len(packet) < 12 {
		return header, nil, fmt.Errorf("RTP audio: packet is shorter than the RTP header")
	}
	if packet[0]>>6 != 2 {
		return header, nil, fmt.Errorf("RTP audio: unsupported RTP version %d", packet[0]>>6)
	}
	if packet[1]&0x7f != audioCapturePayloadType {
		return header, nil, fmt.Errorf("RTP audio: unexpected payload type %d", packet[1]&0x7f)
	}

	padding := packet[0]&0x20 != 0
	hasExtension := packet[0]&0x10 != 0
	csrcCount := int(packet[0] & 0x0f)
	offset := 12 + 4*csrcCount
	if offset > len(packet) {
		return header, nil, fmt.Errorf("RTP audio: truncated CSRC list")
	}
	if !hasExtension {
		return header, nil, fmt.Errorf("RTP audio: ONVIF timestamp extension is absent")
	}
	if len(packet)-offset < 4 {
		return header, nil, fmt.Errorf("RTP audio: truncated header extension")
	}
	profile := binary.BigEndian.Uint16(packet[offset : offset+2])
	if profile != audioONVIFRTPHeaderProfile {
		return header, nil, fmt.Errorf("RTP audio: unexpected header extension profile 0x%04x", profile)
	}
	extensionBytes := int(binary.BigEndian.Uint16(packet[offset+2:offset+4])) * 4
	offset += 4
	if extensionBytes < 12 || extensionBytes > len(packet)-offset {
		return header, nil, fmt.Errorf("RTP audio: invalid ONVIF extension size %d", extensionBytes)
	}
	onvifTimestamp := binary.BigEndian.Uint64(packet[offset : offset+8])
	if onvifTimestamp == 0 {
		return header, nil, fmt.Errorf("RTP audio: zero ONVIF timestamp")
	}
	offset += extensionBytes

	payloadEnd := len(packet)
	if padding {
		paddingBytes := int(packet[len(packet)-1])
		if paddingBytes == 0 || paddingBytes > payloadEnd-offset {
			return header, nil, fmt.Errorf("RTP audio: invalid padding length %d", paddingBytes)
		}
		payloadEnd -= paddingBytes
	}
	if payloadEnd <= offset {
		return header, nil, fmt.Errorf("RTP audio: packet has no L16 payload")
	}

	header = timestampedL16RTPHeader{
		sequence:       binary.BigEndian.Uint16(packet[2:4]),
		timestamp:      binary.BigEndian.Uint32(packet[4:8]),
		ssrc:           binary.BigEndian.Uint32(packet[8:12]),
		onvifTimestamp: onvifTimestamp,
	}
	return header, packet[offset:payloadEnd], nil
}

func audioSamplesDuration(samples uint64) time.Duration {
	seconds := samples / audioSampleRate
	remainder := samples % audioSampleRate
	return time.Duration(seconds)*time.Second +
		time.Duration((remainder*uint64(time.Second)+audioSampleRate/2)/audioSampleRate)
}

func durationToAudioSamples(delta time.Duration) int64 {
	seconds := int64(delta / time.Second)
	remainder := int64(delta % time.Second)
	whole := seconds * audioSampleRate
	if remainder >= 0 {
		return whole + (remainder*audioSampleRate+int64(time.Second)/2)/int64(time.Second)
	}
	return whole - ((-remainder)*audioSampleRate+int64(time.Second)/2)/int64(time.Second)
}

// audioRTPClock maps the source sample timebase onto the sender's random RTP
// epoch. It is safe for the media loop and periodic TimeAnnounce loop to share.
type audioRTPClock struct {
	mu sync.RWMutex

	valid           bool
	anchorPTS       time.Time
	anchorRTP       uint32
	lastFramePTS    time.Time
	lastFrameRTP    uint32
	lastFrameCount  uint32
	anchorSourceRTP uint32
	lastSourceRTP   uint32
	hasSourceRTP    bool
}

func newAudioRTPClock(epoch uint32) *audioRTPClock {
	return &audioRTPClock{anchorRTP: epoch}
}

// mapFrame returns the outgoing timestamp for a source frame. Forward source
// gaps remain RTP gaps. A backward source-clock reset starts a new linear map
// after the previous frame and asks the caller to publish a reset announce.
func (c *audioRTPClock) mapFrame(pts time.Time, samples uint32) (rtp uint32, reset bool) {
	return c.mapFramePosition(audioPCMFramePosition{PTS: pts}, samples)
}

func (c *audioRTPClock) mapFramePosition(position audioPCMFramePosition, samples uint32) (rtp uint32, reset bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pts := position.PTS

	if !c.valid {
		c.valid = true
		c.anchorPTS = pts
		c.lastFramePTS = pts
		c.lastFrameRTP = c.anchorRTP
		c.lastFrameCount = samples
		c.hasSourceRTP = position.HasSourceRTP
		c.anchorSourceRTP = position.SourceRTP
		c.lastSourceRTP = position.SourceRTP
		return c.anchorRTP, false
	}

	sourceReset := position.HasSourceRTP != c.hasSourceRTP
	if position.HasSourceRTP && c.hasSourceRTP {
		expectedSource := c.lastSourceRTP + c.lastFrameCount
		sourceReset = int32(position.SourceRTP-expectedSource) < 0
	}
	if sourceReset || (!position.HasSourceRTP && pts.Before(c.lastFramePTS)) {
		c.anchorRTP = c.lastFrameRTP + c.lastFrameCount
		c.anchorPTS = pts
		c.anchorSourceRTP = position.SourceRTP
		rtp = c.anchorRTP
		reset = true
	} else if position.HasSourceRTP {
		// Media RTP advances from captured sample positions. This preserves the
		// source clock's rate and any real capture gaps without deriving either
		// from wall-clock duration.
		rtp = c.anchorRTP + (position.SourceRTP - c.anchorSourceRTP)
		// A large source-PTS phase reset needs an immediate TimeAnnounce, but it
		// must not break otherwise continuous media RTP sample progression.
		reset = pts.Before(c.lastFramePTS.Add(-100 * time.Millisecond))
	} else {
		deltaSamples := durationToAudioSamples(pts.Sub(c.anchorPTS))
		rtp = c.anchorRTP + uint32(deltaSamples)
		minimum := c.lastFrameRTP + c.lastFrameCount
		if int32(rtp-minimum) < 0 {
			// Sub-sample PTS jitter must not overlap already-sent media.
			rtp = minimum
			c.anchorRTP = rtp
			c.anchorPTS = pts
		}
	}
	c.lastFramePTS = pts
	c.lastFrameRTP = rtp
	c.lastFrameCount = samples
	c.hasSourceRTP = position.HasSourceRTP
	c.lastSourceRTP = position.SourceRTP
	return rtp, reset
}

func (c *audioRTPClock) rtpAt(localTime time.Time) (uint32, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.valid || localTime.IsZero() {
		return 0, false
	}
	deltaSamples := durationToAudioSamples(localTime.Sub(c.anchorPTS))
	return c.anchorRTP + uint32(deltaSamples), true
}

// latestBoundary returns the sample boundary immediately after the most recent
// frame. TimeAnnounce must map RTP and network time for the same instant; this
// future/apply boundary avoids assuming that the audio device clock runs at
// exactly the host clock's rate.
func (c *audioRTPClock) latestBoundary() (uint32, time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.valid || c.lastFramePTS.IsZero() {
		return 0, time.Time{}, false
	}
	return c.lastFrameRTP + c.lastFrameCount,
		c.lastFramePTS.Add(audioSamplesDuration(uint64(c.lastFrameCount))), true
}
