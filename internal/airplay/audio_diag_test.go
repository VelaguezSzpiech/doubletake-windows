package airplay

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"
)

func captureDiagRecords(t *testing.T, run func()) []map[string]any {
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

func healthFor(t *testing.T, feed func(d *audioDiag, start time.Time)) map[string]any {
	t.Helper()
	start := time.Unix(1787370000, 0)
	d := newAudioDiag(start)
	stream := &AudioStream{latencySamples: 3748} // ~85ms
	feed(d, start)
	records := captureDiagRecords(t, func() {
		d.emitHealth(start.Add(5*time.Second), stream, &AudioCapture{}, &mediaClock{})
	})
	if len(records) != 1 || records[0]["kind"] != "audio.health" {
		t.Fatalf("records = %v, want one audio.health", records)
	}
	return records[0]
}

func hasFlag(record map[string]any, flag string) bool {
	flags, _ := record["flags"].([]any)
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

func feedHealthy(d *audioDiag, start time.Time, age time.Duration, frames int) {
	for i := 0; i < frames; i++ {
		now := start.Add(time.Duration(i) * 8 * time.Millisecond)
		d.onRead(8 * time.Millisecond)
		d.onFrame(now, audioPCMFramePosition{PTS: now.Add(-age), SourceRTP: uint32(i * 352), HasSourceRTP: true})
		d.onSendTiming(100 * time.Microsecond)
		d.onSent(uint32(i*352), uint16(i))
	}
}

func TestAudioHealthReportsOKForTimelyStream(t *testing.T) {
	record := healthFor(t, func(d *audioDiag, start time.Time) { feedHealthy(d, start, 12*time.Millisecond, 600) })
	if record["status"] != "ok" || len(record["flags"].([]any)) != 0 {
		t.Fatalf("healthy stream reported %v flags=%v", record["status"], record["flags"])
	}
	if record["frames_sent_iv"].(float64) != 600 || record["source_age_us"].(map[string]any)["last"].(float64) != 12000 {
		t.Fatalf("unexpected counters: %v", record)
	}
}

func TestAudioHealthFlagsStaleDroppingAsFailing(t *testing.T) {
	record := healthFor(t, func(d *audioDiag, start time.Time) {
		feedHealthy(d, start, 90*time.Millisecond, 100)
		for i := 0; i < 100; i++ {
			d.onStale()
		}
	})
	if record["status"] != "failing" || !hasFlag(record, "stale_dropping") {
		t.Fatalf("stale stream reported %v flags=%v", record["status"], record["flags"])
	}
	if record["age_headroom_us"].(float64) >= 0 {
		t.Fatalf("age headroom = %v, want negative once past the stale threshold", record["age_headroom_us"])
	}
}

func TestAudioHealthFlagsSilenceAndSlowSend(t *testing.T) {
	silent := healthFor(t, func(d *audioDiag, start time.Time) {})
	if silent["status"] != "failing" || !hasFlag(silent, "no_frames_sent") {
		t.Fatalf("silent stream reported %v flags=%v", silent["status"], silent["flags"])
	}
	slow := healthFor(t, func(d *audioDiag, start time.Time) {
		feedHealthy(d, start, 12*time.Millisecond, 100)
		d.onSendTiming(40 * time.Millisecond)
	})
	if slow["status"] != "degraded" || !hasFlag(slow, "send_slow") {
		t.Fatalf("slow-send stream reported %v flags=%v", slow["status"], slow["flags"])
	}
}

func TestAudioHealthFlagsAnnounceMappingSteps(t *testing.T) {
	smooth := healthFor(t, func(d *audioDiag, start time.Time) {
		feedHealthy(d, start, 12*time.Millisecond, 600)
		rtp, net := uint32(1000), compactTimestamp(time.Hour)
		for i := 0; i < 5; i++ {
			d.onAnnounce(rtp, net)
			rtp += audioSampleRate
			net += compactTimestamp(time.Second)
		}
	})
	if hasFlag(smooth, "announce_mapping_jump") {
		t.Fatalf("constant-rate announces flagged: %v", smooth["announce_mapping_err_us"])
	}
	stepped := healthFor(t, func(d *audioDiag, start time.Time) {
		feedHealthy(d, start, 12*time.Millisecond, 600)
		rtp, net := uint32(1000), compactTimestamp(time.Hour)
		for i := 0; i < 5; i++ {
			d.onAnnounce(rtp, net)
			rtp += audioSampleRate
			net += compactTimestamp(time.Second)
			if i == 2 {
				net += compactTimestamp(2 * time.Millisecond) // forward step at one feedback
			}
		}
	})
	if !hasFlag(stepped, "announce_mapping_jump") {
		t.Fatalf("2ms announce step not flagged: %v", stepped["announce_mapping_err_us"])
	}
}
