package airplay

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

func videoDiagRecords(t *testing.T, run func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prevOutput, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOutput); log.SetFlags(prevFlags) }()
	run()
	var records []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "[DIAG] ")
		if !ok {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			t.Fatalf("diagnostic line is not valid JSON: %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestDiagHistogramBucketing(t *testing.T) {
	h := newDiagHistogram("ms", 20, 40, 60, 100)
	for _, v := range []int64{0, 19, 20, 39, 40, 59, 60, 99, 100, 101, 5000} {
		h.observe(v)
	}
	got := h.snapshot()
	want := map[string]uint64{"lt20ms": 2, "20_40ms": 2, "40_60ms": 2, "60_100ms": 2, "ge100ms": 3}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for label, n := range want {
		if got[label] != n {
			t.Errorf("bucket %s = %d, want %d (all: %v)", label, got[label], n, got)
		}
	}
	h.reset()
	for label, n := range h.snapshot() {
		if n != 0 {
			t.Errorf("bucket %s = %d after reset", label, n)
		}
	}
}

func TestDiagAnomalyLimiterCountsSuppressed(t *testing.T) {
	l := newDiagAnomalyLimiter(3)
	now := time.Unix(100, 0)
	allowed := 0
	for i := 0; i < 10; i++ {
		if ok, _ := l.allow("video.gap", now); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("allowed %d in one window, want 3", allowed)
	}
	if ok, other := l.allow("video.send_slow", now); !ok || other != 0 {
		t.Fatalf("another kind must have its own budget, got ok=%v suppressed=%d", ok, other)
	}
	ok, suppressed := l.allow("video.gap", now.Add(1100*time.Millisecond))
	if !ok || suppressed != 7 {
		t.Fatalf("next window: ok=%v suppressed=%d, want true/7", ok, suppressed)
	}
	if got := l.takeSuppressed(); got != 7 {
		t.Fatalf("takeSuppressed = %d, want 7", got)
	}
}

func TestSessionNegotiatedRedactsKeyMaterial(t *testing.T) {
	request := map[string]interface{}{
		"ekey":        []byte{1, 2, 3, 4},
		"eiv":         []byte{9, 9},
		"fpsap":       "FPLY-secret",
		"shk":         []byte{7},
		"pk":          []byte("publickeybytes"),
		"streamKey":   "streamsecret",
		"sessionUUID": "ok-to-log",
		"streams": []interface{}{
			map[string]interface{}{
				"type": uint64(96), "spf": uint64(352), "ct": uint64(2),
				"shk": []byte{1, 2}, "controlPort": uint64(5000),
			},
		},
		"timingPeerInfo": map[string]interface{}{"ClockID": uint64(42), "Addresses": []interface{}{"10.0.0.2"}},
		"pairingData":    []byte{5, 5, 5},
		"blob":           []byte{1, 2, 3, 4, 5},
	}
	records := videoDiagRecords(t, func() {
		diagEmit("session.negotiated", map[string]any{
			"step": "setup", "request": diagSanitize(request),
			"response_headers": diagSafeHeaders(map[string]string{
				"X-Apple-ProcessingTime": "3", "X-Apple-Session-Key": "abc", "Authorization": "Digest x", "Audio-Latency": "88200",
			}),
		})
	})
	if len(records) != 1 || records[0]["kind"] != "session.negotiated" {
		t.Fatalf("records = %v", records)
	}
	encoded, _ := json.Marshal(records[0])
	text := string(encoded)
	for _, secret := range []string{"FPLY-secret", "streamsecret", "publickeybytes", "AQIDBA", "Digest x", "abc\""} {
		if strings.Contains(text, secret) {
			t.Errorf("record leaks %q: %s", secret, text)
		}
	}
	for _, keep := range []string{"ok-to-log", "controlPort", "5000", "ClockID", "10.0.0.2", "5 bytes", "redacted 4 bytes", "88200"} {
		if !strings.Contains(text, keep) {
			t.Errorf("record lost %q: %s", keep, text)
		}
	}
	req := records[0]["request"].(map[string]any)
	for _, key := range []string{"ekey", "eiv", "fpsap", "shk", "pk", "streamKey", "pairingData"} {
		if v, _ := req[key].(string); !strings.HasPrefix(v, "<redacted") {
			t.Errorf("%s = %v, want redacted", key, req[key])
		}
	}
	stream := req["streams"].([]any)[0].(map[string]any)
	if v, _ := stream["shk"].(string); !strings.HasPrefix(v, "<redacted") {
		t.Errorf("nested shk = %v, want redacted", stream["shk"])
	}
}

func TestEmitSetParameterDiag(t *testing.T) {
	records := videoDiagRecords(t, func() {
		emitSetParameterDiag("rtsp://x/1", []byte("volume: 0.000000\r\n"), 12*time.Millisecond, nil)
		emitSetParameterDiag("rtsp://x/1", []byte("volume: 0\r\n"), time.Millisecond, errors.New("boom"))
	})
	if len(records) != 2 {
		t.Fatalf("records = %v", records)
	}
	first := records[0]
	if first["kind"] != "session.negotiated" || first["step"] != "set_parameter" || first["rtt_us"].(float64) != 12000 {
		t.Fatalf("first = %v", first)
	}
	if body, _ := first["body"].(string); !strings.Contains(body, "volume") {
		t.Fatalf("body = %v, want the volume parameter", first["body"])
	}
	if records[1]["error"] != "boom" {
		t.Fatalf("second = %v, want the error", records[1])
	}
}

func TestFeedbackExchangeRecord(t *testing.T) {
	start := time.Unix(1000, 0)
	clock := &mediaClock{anchorLocal: start, anchorTimestamp: compactTimestamp(10 * time.Second), timelineID: 7}
	headers := map[string]string{"x-apple-requestreceivedtimestamp": "12040", "x-apple-processingtime": "3", "x-apple-other": "v"}
	responded := start.Add(2*time.Second + 8*time.Millisecond)
	var result mediaClockReanchor
	records := videoDiagRecords(t, func() {
		var err error
		result, err = clock.reanchorDetailed(headers, responded)
		if err != nil {
			t.Fatal(err)
		}
		diagEmit("clock.feedback", diagFeedbackExchangeRecord(feedbackExchange{
			seq: 4, requestedAt: responded.Add(-8 * time.Millisecond), respondedAt: responded,
			prevRequestedAt: start, headers: headers,
			reanchor: result, reanchored: true,
		}))
	})
	var exchange map[string]any
	for _, r := range records {
		if r["record"] == "exchange" {
			exchange = r
		}
	}
	if exchange == nil {
		t.Fatalf("no exchange record in %v", records)
	}
	if exchange["rtt_us"].(float64) != 8000 || exchange["seq"].(float64) != 4 {
		t.Fatalf("exchange = %v", exchange)
	}
	if exchange["receiver_processing_ms"].(float64) != 3 || exchange["receiver_request_received_ms"].(float64) != 12040 {
		t.Fatalf("raw clock fields = %v", exchange)
	}
	if exchange["net_rtt_est_us"].(float64) != 5000 {
		t.Fatalf("net_rtt_est_us = %v, want 5000", exchange["net_rtt_est_us"])
	}
	if exchange["had_anchor"] != true || exchange["clamped"] != false {
		t.Fatalf("anchor fields = %v", exchange)
	}
	// Receiver 12043 ms vs projection 12008 ms: a real +35 ms phase applied at once.
	if phase := exchange["phase_us"].(float64); phase < 34000 || phase > 36000 {
		t.Fatalf("phase_us = %v, want about 35000", phase)
	}
	if exchange["advance_us"].(float64) < 34000 {
		t.Fatalf("advance_us = %v, want the phase applied", exchange["advance_us"])
	}
	hdr := exchange["response_headers"].(map[string]any)
	if hdr["x-apple-other"] != "v" {
		t.Fatalf("response_headers = %v", hdr)
	}

	failure := diagFeedbackExchangeRecord(feedbackExchange{seq: 5, requestedAt: start, respondedAt: start.Add(time.Second), err: errors.New("timeout"), errStreak: 2})
	if failure["error"] != "timeout" || failure["consecutive_errors"].(uint64) != 2 {
		t.Fatalf("failure record = %v", failure)
	}
}

func TestVideoDiagOnFrameMapAndHealth(t *testing.T) {
	start := time.Unix(2000, 0)
	d := newVideoDiag(start)
	captured := start
	ts := compactTimestamp(5 * time.Second)
	d.onFrameMap(captured, ts, ts)
	// Next frame 16 ms later in capture time, mapped 16 ms later: no step.
	captured = captured.Add(16 * time.Millisecond)
	ts += compactTimestamp(16 * time.Millisecond)
	d.onFrameMap(captured, ts, ts)
	// 16 ms later again but the clock mapping moved by 8 ms: a step; also clamped.
	captured = captured.Add(16 * time.Millisecond)
	ts += compactTimestamp(24 * time.Millisecond)
	d.onFrameMap(captured, ts, ts+1)

	d.onAccessUnit(start, start.Add(time.Millisecond), start, 1000)
	d.onAccessUnit(start.Add(10*time.Millisecond), start.Add(130*time.Millisecond), start.Add(30*time.Millisecond), 300*1024)
	d.onSend(300*1024, true, start, start.Add(30*time.Millisecond), nil)
	d.onPTSAge(80*time.Millisecond, 100*time.Millisecond)

	records := videoDiagRecords(t, func() {
		d.emitHealth(start.Add(time.Second), 100*time.Millisecond, 0, false)
	})
	if len(records) != 1 || records[0]["kind"] != "video.health" {
		t.Fatalf("records = %v", records)
	}
	h := records[0]
	if h["ts_clamped_iv"].(float64) != 1 {
		t.Errorf("ts_clamped_iv = %v, want 1", h["ts_clamped_iv"])
	}
	step := h["ts_map_step_us_iv"].(map[string]any)
	if step["max"].(float64) < 7900 || step["max"].(float64) > 8100 || step["min"].(float64) < -100 || step["min"].(float64) > 100 {
		t.Errorf("ts_map_step_us_iv = %v, want min~0 max~8000", step)
	}
	gaps := h["output_gap_hist_iv"].(map[string]any)
	if gaps["ge100ms"].(float64) != 1 {
		t.Errorf("output_gap_hist_iv = %v, want one gap >= 100ms", gaps)
	}
	if h["idr_iv"].(float64) != 1 || h["idr_max_kb_iv"].(float64) != 300 {
		t.Errorf("idr fields = %v / %v", h["idr_iv"], h["idr_max_kb_iv"])
	}
	if sends := h["send_hist_iv"].(map[string]any); sends["25_50ms"].(float64) != 1 {
		t.Errorf("send_hist_iv = %v, want one 25-50ms write", sends)
	}
	if h["process"] == nil || h["latency_budget_us"].(float64) != 100000 {
		t.Errorf("missing process stats or latency budget: %v", h)
	}
	if h["final"] != false {
		t.Errorf("final = %v", h["final"])
	}
	// Interval counters reset after the emission.
	again := videoDiagRecords(t, func() { d.emitHealth(start.Add(2*time.Second), 100*time.Millisecond, 0, true) })
	if again[0]["frames_sent_iv"].(float64) != 0 || again[0]["frames_sent_total"].(float64) != 1 || again[0]["final"] != true {
		t.Errorf("second health = %v", again[0])
	}
	// The 120 ms arrival gap and the 8 ms step were queued as anomalies.
	kinds := map[string]bool{}
	for len(d.anomalies) > 0 {
		kinds[(<-d.anomalies).kind] = true
	}
	for _, kind := range []string{"video.gap", "video.idr_large", "video.send_slow", "video.ts_step"} {
		if !kinds[kind] {
			t.Errorf("missing anomaly %s (got %v)", kind, kinds)
		}
	}
}

func TestGstEncoderSummaryAndPipelineName(t *testing.T) {
	args := []string{"gst-launch-1.0.exe", "-q", "d3d11screencapturesrc", "!", "videoconvert", "!", "nvh264enc", "bitrate=12000", "gop-size=60", "preset=low-latency", "!", "h264parse", "!", "fdsink"}
	if got := gstPipelineName(args); got != "video" {
		t.Fatalf("pipeline name = %q, want video", got)
	}
	enc := gstEncoderSummary(args)
	if enc["element"] != "nvh264enc" || enc["bitrate"] != "12000" || enc["gop-size"] != "60" {
		t.Fatalf("encoder summary = %v", enc)
	}
	audio := []string{"gst-launch-1.0", "wasapi2src", "loopback=true", "!", "audio/x-raw,rate=44100", "!", "fdsink"}
	if got := gstPipelineName(audio); got != "audio" {
		t.Fatalf("audio pipeline name = %q", got)
	}
	if got := gstPipelineName([]string{"gst-inspect-1.0", "nvh264enc"}); got != "" {
		t.Fatalf("probe classified as %q", got)
	}
}

func TestGstStderrLimiter(t *testing.T) {
	var l gstStderrLimiter
	now := time.Unix(10, 0)
	if out := l.observe(now, "WARNING: x"); len(out) != 1 {
		t.Fatalf("first line out = %v", out)
	}
	for i := 0; i < 50; i++ {
		if out := l.observe(now.Add(time.Duration(i)*time.Millisecond), "WARNING: x"); len(out) != 0 {
			t.Fatalf("repeat %d emitted %v", i, out)
		}
	}
	out := l.observe(now.Add(100*time.Millisecond), "different")
	if len(out) != 2 || out[0]["repeat_n"].(int) != 50 || out[0]["repeat_end"] != true || out[1]["line"] != "different" {
		t.Fatalf("after repeats out = %v", out)
	}
	// A flood of distinct lines is capped per second and reported as suppressed_n.
	emitted := 0
	for i := 0; i < 100; i++ {
		emitted += len(l.observe(now.Add(200*time.Millisecond), "line "+strings.Repeat("x", i)))
	}
	if emitted > gstStderrLinesPerSec {
		t.Fatalf("emitted %d lines in one second, limit %d", emitted, gstStderrLinesPerSec)
	}
	out = l.observe(now.Add(2*time.Second), "later")
	if len(out) != 1 || out[0]["suppressed_n"].(int) == 0 {
		t.Fatalf("later out = %v, want suppressed_n", out)
	}
}
