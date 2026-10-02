package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestUIStopCancelsWhileWaitingForCredential(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	eventReader, events := io.Pipe()
	defer events.Close()
	defer eventReader.Close()
	ui := newUIControl(ctx, cancel, input, events)
	result := make(chan error, 1)
	go func() { _, err := ui.credential("PIN"); result <- err }()
	var event uiEvent
	if err := json.NewDecoder(eventReader).Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "credential_required" {
		t.Fatalf("event = %q", event.Type)
	}
	if _, err := io.WriteString(writer, "{\"type\":\"stop\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop failed to cancel pairing prompt")
	}
}

func TestUICredentialCorrelationAndStopAfterPairing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	eventReader, events := io.Pipe()
	defer events.Close()
	defer eventReader.Close()
	ui := newUIControl(ctx, cancel, input, events)
	result := make(chan string, 1)
	go func() { value, _ := ui.credential("Password"); result <- value }()
	var event uiEvent
	if err := json.NewDecoder(eventReader).Decode(&event); err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(writer)
	if err := encoder.Encode(uiCommand{Type: "credential", RequestID: event.RequestID + 1, Value: "stale"}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(uiCommand{Type: "credential", RequestID: event.RequestID, Value: " password with spaces "}); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-result:
		if value != " password with spaces " {
			t.Fatalf("value lost whitespace or accepted stale reply")
		}
	case <-time.After(time.Second):
		t.Fatal("credential response was not delivered")
	}
	if err := encoder.Encode(uiCommand{Type: "stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop reader stopped after pairing")
	}
}

func TestUIInputEOFAndMalformedCommandsCancel(t *testing.T) {
	for _, input := range []string{"", "not-json\n", "{\"type\":\"unknown\"}\n"} {
		ctx, cancel := context.WithCancel(context.Background())
		var output bytes.Buffer
		newUIControl(ctx, cancel, bytes.NewBufferString(input), &output)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatalf("input %q did not cancel", input)
		}
		cancel()
	}
}

func TestUIStartupGateStopsBeforeCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ui := newUIControl(ctx, cancel, input, io.Discard)
	result := make(chan error, 1)
	go func() { result <- ui.waitForStart() }()
	if _, err := io.WriteString(writer, "{\"type\":\"stop\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not release startup gate")
	}
}

func TestUIStartupGateAllowsStartWithoutClosingControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ui := newUIControl(ctx, cancel, input, io.Discard)
	result := make(chan error, 1)
	go func() { result <- ui.waitForStart() }()
	if _, err := io.WriteString(writer, "{\"type\":\"start\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("startup failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("start did not release startup gate")
	}
	if _, err := io.WriteString(writer, "{\"type\":\"stop\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("reader stopped after startup")
	}
}

func TestUIMediaReadinessStopsBeforeSendingConnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	var output bytes.Buffer
	ui := newUIControl(ctx, cancel, input, &output)
	video, audio := make(chan struct{}), make(chan struct{})
	if _, err := io.WriteString(writer, "{\"type\":\"stop\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop command did not cancel session")
	}
	close(video)
	close(audio)
	done := make(chan struct{})
	go func() { ui.waitForMedia(ctx, video, audio); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not release readiness waiter")
	}
	if output.Len() != 0 {
		t.Fatalf("stopped session emitted events: %s", output.String())
	}
}

func TestUIMediaReadinessRequiresEveryEnabledStream(t *testing.T) {
	for _, first := range []string{"video", "audio"} {
		t.Run(first, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output bytes.Buffer
			ui := &uiControl{ctx: ctx, cancel: cancel, output: json.NewEncoder(&output)}
			video, audio := make(chan struct{}), make(chan struct{})
			if first == "video" {
				close(video)
			} else {
				close(audio)
			}
			done := make(chan struct{})
			go func() { ui.waitForMedia(ctx, video, audio); close(done) }()
			select {
			case <-done:
				t.Fatal("connected with only one stream ready")
			case <-time.After(20 * time.Millisecond):
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("readiness waiter survived session end")
			}
			if output.Len() != 0 {
				t.Fatalf("partially ready session emitted events: %s", output.String())
			}
		})
	}
}

func TestUIMediaReadinessConnectsOnceAfterRequiredSends(t *testing.T) {
	for _, audioEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("audio=%v", audioEnabled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var output bytes.Buffer
			ui := &uiControl{ctx: ctx, cancel: cancel, output: json.NewEncoder(&output)}
			video := make(chan struct{})
			var audio chan struct{}
			if audioEnabled {
				audio = make(chan struct{})
			}
			done := make(chan struct{})
			go func() { ui.waitForMedia(ctx, video, audio); close(done) }()
			close(video)
			if audioEnabled {
				close(audio)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("ready streams did not connect")
			}
			decoder := json.NewDecoder(&output)
			var event uiEvent
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event.Type != "connected" || !event.SessionReady {
				t.Fatalf("readiness event = %+v", event)
			}
			if err := decoder.Decode(&event); !errors.Is(err, io.EOF) {
				t.Fatalf("extra readiness event: %+v, error: %v", event, err)
			}
		})
	}
}

func TestUIImmediateAudioFailureSurvivesCancellationAndCleanup(t *testing.T) {
	ctx, cancelCause := context.WithCancelCause(context.Background())
	cancel := func() { cancelCause(nil) }
	defer cancel()
	var output bytes.Buffer
	ui := &uiControl{ctx: ctx, cancel: cancel, output: json.NewEncoder(&output)}
	failure := errors.New("audio prewarm capture failed")
	cancelCause(fmt.Errorf("audio streaming failed: %w", failure))
	// Startup observes cancellation; cleanup must not replace the owned cause.
	startupErr := ctx.Err()
	cancel()
	result := ui.finish(ctx, startupErr)
	if !errors.Is(result, failure) {
		t.Fatalf("session result lost audio failure: %v", result)
	}
	decoder := json.NewDecoder(&output)
	var event uiEvent
	if err := decoder.Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "error" || event.Message != result.Error() {
		t.Fatalf("failure event = %+v", event)
	}
	if err := decoder.Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "disconnected" {
		t.Fatalf("terminal event = %+v", event)
	}
	if err := decoder.Decode(&event); !errors.Is(err, io.EOF) {
		t.Fatalf("duplicate failure event: %+v, error: %v", event, err)
	}
}

func TestUIGenericCleanupCancellationPreservesConcreteErrors(t *testing.T) {
	for _, failure := range []error{nil, errors.New("video send failed")} {
		ctx, cancel := context.WithCancel(context.Background())
		var output bytes.Buffer
		ui := &uiControl{ctx: ctx, cancel: cancel, output: json.NewEncoder(&output)}
		// Generic cleanup is not an explicit UI stop and must preserve failures.
		cancel()
		result := failure
		if result == nil {
			result = ctx.Err()
		}
		result = ui.finish(ctx, result)
		if !errors.Is(result, failure) {
			t.Fatalf("session result = %v, want %v", result, failure)
		}
		decoder := json.NewDecoder(&output)
		var event uiEvent
		if failure != nil {
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event.Type != "error" || event.Message != failure.Error() {
				t.Fatalf("cleanup suppressed concrete error: %+v", event)
			}
		}
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "disconnected" {
			t.Fatalf("user stop emitted unexpected event: %+v", event)
		}
		if err := decoder.Decode(&event); !errors.Is(err, io.EOF) {
			t.Fatalf("extra terminal event: %+v, error: %v", event, err)
		}
	}
}
