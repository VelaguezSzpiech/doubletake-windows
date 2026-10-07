package airplay

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDiagRedactsKeyFieldsEverywhere(t *testing.T) {
	dict := map[string]any{
		"ekey": "SECRETVALUE1", "eiv": []byte("SECRETVALUE2"), "shk": "SECRETVALUE3",
		"nested":   map[string]any{"epk": "SECRETVALUE4", "pv": "SECRETVALUE5", "fairPlayData": "SECRETVALUE6", "ok": "visible"},
		"password": "SECRETVALUE7", "pin": uint64(123456), "streamKey": "SECRETVALUE8", "authToken": "SECRETVALUE9",
		"arr": []any{map[string]any{"aesKey": "SECRETVALUE10"}}, "latency": uint64(88200),
		"blob": []byte("SECRETBLOB11"),
	}
	body, err := plist.Marshal(dict, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	records := captureDiagRecords(t, func() {
		ex := &controlExchangeDiag{proto: "rtsp", method: "SETUP", uri: "rtsp://x/1", cseq: 7, request: []byte(
			"SETUP rtsp://x/1 RTSP/1.0\r\nCSeq: 7\r\nAuthorization: Digest username=\"u\", response=\"SECRETDIGEST\"\r\nX-Apple-ProtocolVersion: 1\r\n\r\n"),
			reqBody: body, reqType: "application/x-apple-binary-plist", started: time.Now(), status: 200,
			respBody: body, respHeaders: map[string]string{"content-type": "application/x-apple-binary-plist", "www-authenticate": "Digest nonce=SECRETDIGEST2"}}
		ex.emit()
	})
	if len(records) != 1 {
		t.Fatalf("records = %v", records)
	}
	out := mustJSON(t, records[0])
	for _, bad := range []string{"SECRETVALUE", "SECRETBLOB", "SECRETDIGEST", "123456"} {
		if strings.Contains(out, bad) {
			t.Errorf("record leaks %q: %s", bad, out)
		}
	}
	for _, want := range []string{"redacted:", "visible", "88200", "X-Apple-ProtocolVersion", "rtt_us", "req_body_bytes"} {
		if !strings.Contains(out, want) {
			t.Errorf("record lacks %q: %s", want, out)
		}
	}
}

func TestDiagSetupInfoPrivacyAcrossRecords(t *testing.T) {
	response := map[string]any{
		"timingPort": uint64(7001),
		"info": map[string]any{
			"name":         "Synthetic Private Receiver",
			"deviceID":     "fixture-device-identifier",
			"extension":    "fixture-private-extension",
			"features":     uint64(1234),
			"model":        "FixtureReceiver1,1",
			"audioFormats": []any{map[string]any{"type": uint64(96), "audioInputFormats": uint64(7)}},
		},
	}
	for _, format := range []struct {
		name string
		kind int
	}{{"binary", plist.BinaryFormat}, {"xml", plist.XMLFormat}} {
		t.Run(format.name, func(t *testing.T) {
			body, err := plist.Marshal(response, format.kind)
			if err != nil {
				t.Fatal(err)
			}
			records := captureDiagRecords(t, func() {
				(&controlExchangeDiag{
					proto: "rtsp", method: "SETUP", uri: "rtsp://x/1", cseq: 7,
					request: []byte("SETUP rtsp://x/1 RTSP/1.0\r\nCSeq: 7\r\n\r\n"),
					started: time.Now(), status: 200, respBody: body,
					respHeaders: map[string]string{"content-type": "application/x-apple-binary-plist"},
				}).emit()
				diagEmit("session.negotiated", map[string]any{"step": "setup", "response": diagSanitizeResponse(response)})
			})
			if len(records) != 2 || records[0]["kind"] != "rtsp.exchange" || records[1]["kind"] != "session.negotiated" {
				t.Fatalf("records = %v", records)
			}
			for _, record := range records {
				out := mustJSON(t, record)
				for _, private := range []string{"Synthetic Private Receiver", "fixture-device-identifier", "fixture-private-extension"} {
					if strings.Contains(out, private) {
						t.Errorf("%s leaks %q: %s", record["kind"], private, out)
					}
				}
			}
			wire := records[0]["resp_body"].(map[string]any)
			info := wire["info"].(map[string]any)
			if info["features"] != float64(1234) || info["model"] != "FixtureReceiver1,1" || info["_other_fields_n"] != float64(3) {
				t.Errorf("wire info = %v", info)
			}
			for _, field := range []string{"name", "deviceID", "extension"} {
				if _, ok := info[field]; ok {
					t.Errorf("wire info retains private field %q: %v", field, info)
				}
			}
			formats := info["audioFormats"].([]any)
			if len(formats) != 1 || formats[0].(map[string]any)["audioInputFormats"] != float64(7) {
				t.Errorf("wire audio formats = %v", formats)
			}
			negotiated := records[1]["response"].(map[string]any)
			if negotiated["info"] != "<info: 6 fields>" {
				t.Errorf("negotiated info = %v, want field-count summary", negotiated["info"])
			}
			if wire["timingPort"] != float64(7001) || negotiated["timingPort"] != float64(7001) {
				t.Errorf("negotiation data lost: wire=%v negotiated=%v", wire, negotiated)
			}
		})
	}
}

func TestDiagRedactsTextParametersAndPairingBodies(t *testing.T) {
	body, _ := diagDescribeBody("rtsp://x/1", "text/parameters", []byte("volume: -20.0\r\nrsaaeskey: SECRETKEYTEXT\r\n"))
	text, _ := body.(string)
	if strings.Contains(text, "SECRETKEYTEXT") || !strings.Contains(text, "volume: -20.0") {
		t.Errorf("text body = %q", text)
	}
	pairing, _ := diagDescribeBody("/pair-setup", "application/octet-stream", []byte("SECRETPAIR"))
	if s, _ := pairing.(string); strings.Contains(s, "SECRETPAIR") || s != "<redacted:10 bytes>" {
		t.Errorf("pairing body = %v", pairing)
	}
	bin, _ := diagDescribeBody("/x", "application/octet-stream", []byte{0, 1, 2, 3})
	if s, _ := bin.(string); s != "<redacted:4 bytes>" {
		t.Errorf("binary body = %v", bin)
	}
}

func TestDiagBodyTruncatedAt16KB(t *testing.T) {
	big := strings.Repeat("a b\r\n", 10000)
	body, truncated := diagDescribeBody("/x", "text/parameters", []byte(big))
	if !truncated || len(body.(string)) != diagBodyLimit {
		t.Errorf("truncated=%v len=%d", truncated, len(body.(string)))
	}
	records := captureDiagRecords(t, func() {
		(&controlExchangeDiag{proto: "rtsp", method: "SET_PARAMETER", uri: "/x", request: []byte("X\r\n\r\n"), reqBody: []byte(big),
			reqType: "text/parameters", started: time.Now()}).emit()
	})
	if records[0]["truncated"] != true {
		t.Errorf("record not marked truncated: %v", records[0])
	}
}

func TestAudioHistogramBucketing(t *testing.T) {
	cases := []struct {
		us   int64
		want int
	}{{0, 0}, {999, 0}, {1000, 1}, {1999, 1}, {2000, 2}, {4999, 2}, {5000, 3}, {9999, 3}, {10000, 4}, {19999, 4}, {20000, 5}, {49999, 5}, {50000, 6}, {1 << 40, 6}}
	for _, c := range cases {
		if got := audioHistBucket(audioGapBucketBoundsUS[:], c.us); got != c.want {
			t.Errorf("gap bucket(%d) = %d, want %d (%s)", c.us, got, c.want, audioGapBucketNames[c.want])
		}
	}
	if audioHistBucket(audioSendBucketBoundsUS[:], 49) != 0 || audioHistBucket(audioSendBucketBoundsUS[:], 5000) != 6 {
		t.Error("send bucket bounds wrong")
	}
}

func TestAudioWireStatsAggregatesAndAnomalies(t *testing.T) {
	now := time.Unix(1787370000, 0)
	w := newAudioWireStats(352, now)
	for i := 0; i < 5; i++ {
		at := now.Add(time.Duration(i) * 8 * time.Millisecond)
		w.onPacket(at, 100*time.Microsecond, 1000, false, nil)
		w.onFresh(at, uint16(100+i), uint32(1000+352*i), 900+i)
	}
	// A skipped sequence number after a 68 ms gap.
	at := now.Add(100 * time.Millisecond)
	w.onPacket(at, 6*time.Millisecond, 1000, false, nil)
	w.onFresh(at, 110, uint32(1000+352*5), 905)
	records := captureDiagRecords(t, func() {
		w.emit(now.Add(time.Second), nil)
		w.emitAnomaly(<-w.anomalies)
	})
	wire, anomaly := records[0], records[1]
	gap := wire["gap_hist_iv"].(map[string]any)
	if gap["5_10ms"] != float64(4) || gap["ge50ms"] != float64(1) || wire["gap_max_us_iv"] != float64(68000) {
		t.Errorf("gap hist = %v max=%v", gap, wire["gap_max_us_iv"])
	}
	send := wire["send_hist_iv"].(map[string]any)
	if send["100_250us"] != float64(5) || send["ge5ms"] != float64(1) {
		t.Errorf("send hist = %v", send)
	}
	if wire["packets_iv"] != float64(6) || wire["bytes_iv"] != float64(6000) || wire["seq_anomalies_total"] != float64(1) {
		t.Errorf("wire = %v", wire)
	}
	fb := wire["frame_bytes_iv"].(map[string]any)
	if fb["min"] != float64(900) || fb["max"] != float64(905) {
		t.Errorf("frame bytes = %v", fb)
	}
	if anomaly["kind"] != "audio.wire_anomaly" || anomaly["anomaly"] != "seq_step" || anomaly["seq_step"] != float64(6) {
		t.Errorf("anomaly = %v", anomaly)
	}
	records = captureDiagRecords(t, func() { w.emit(now.Add(2*time.Second), nil) })
	if records[0]["packets_iv"] != float64(0) || records[0]["gap_max_us_iv"] != float64(0) {
		t.Errorf("interval counters not reset: %v", records[0])
	}
}

func TestDiagRateLimiterSuppressedCount(t *testing.T) {
	l := diagRateLimiter{limit: 20}
	now := time.Unix(1787370000, 0)
	allowed := 0
	for i := 0; i < 50; i++ {
		if ok, _ := l.allow(now); ok {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("allowed=%d, want 20", allowed)
	}
	ok, suppressed := l.allow(now.Add(1100 * time.Millisecond))
	if !ok || suppressed != 30 {
		t.Errorf("next window: ok=%v suppressed=%d, want 30", ok, suppressed)
	}
}

func TestAudioCtrlPacketRecordUnknownAndRetransmit(t *testing.T) {
	unknown := audioCtrlPacketRecord([]byte{0x80, 0x7f, 1, 2, 3}, false)
	if unknown["type"] != "unknown" || unknown["hex"] != "807f010203" {
		t.Errorf("unknown = %v", unknown)
	}
	req := []byte{0x80, audioRetransmitRequestPayloadType, 0, 9, 0x01, 0x02, 0, 3}
	rec := audioCtrlPacketRecord(req, false)
	if rec["type"] != "retransmit_request" || rec["first_seq"] != uint16(0x0102) || rec["count"] != uint16(3) {
		t.Errorf("retransmit = %v", rec)
	}
}

func TestNTPTimingDiagDecodesRequestAndResponse(t *testing.T) {
	td := newNTPTimingDiag()
	local := ntpBootTimestamp()
	req := make([]byte, 32)
	req[0], req[1] = 0x80, 0xd2
	binary.BigEndian.PutUint64(req[24:32], local+uint64(5)<<32) // receiver 5 s ahead
	resp := make([]byte, 32)
	resp[0], resp[1] = 0x80, 0xd3
	for _, off := range []int{8, 16, 24} {
		binary.BigEndian.PutUint64(resp[off:off+8], local-uint64(1)<<32) // T1=T2=T3, 1 s ago
	}
	records := captureDiagRecords(t, func() {
		td.onPacket(req, nil, time.Now())
		td.onPacket(req, nil, time.Now())
		td.onPacket(resp, nil, time.Now())
		td.onPacket([]byte{1, 2, 3}, nil, time.Now())
	})
	if len(records) != 4 {
		t.Fatalf("records = %v", records)
	}
	off := records[0]["request_offset_us"].(float64)
	if off < 4.9e6 || off > 5.1e6 || records[0]["type"] != "request" {
		t.Errorf("request record = %v", records[0])
	}
	if _, ok := records[1]["offset_change_us"]; !ok {
		t.Errorf("second request lacks offset_change_us: %v", records[1])
	}
	if records[2]["type"] != "response" || records[2]["delay_us"].(float64) < 0.9e6 {
		t.Errorf("response record = %v", records[2])
	}
	if records[3]["type"] != "unknown" || records[3]["hex"] != "010203" {
		t.Errorf("unknown record = %v", records[3])
	}
}
