package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"doubletake/internal/airplay"
)

type uiEvent struct {
	Type         string `json:"type"`
	Message      string `json:"message,omitempty"`
	RequestID    uint64 `json:"request_id,omitempty"`
	SessionReady bool   `json:"session_ready,omitempty"`
}

type uiCommand struct {
	Type      string `json:"type"`
	RequestID uint64 `json:"request_id,omitempty"`
	Value     string `json:"value,omitempty"`
}

// uiControl owns the only stdin reader for the lifetime of a UI session.
// Replies must match the outstanding prompt; stop remains active before,
// during, and after pairing. EOF means the owning UI has gone away.
type uiControl struct {
	ctx         context.Context
	cancel      context.CancelFunc
	stop        context.CancelFunc
	output      *json.Encoder
	outputMu    sync.Mutex
	userStopped bool
	mu          sync.Mutex
	nextID      uint64
	pendingID   uint64
	pending     chan string
	started     chan struct{}
	startOnce   sync.Once
}

func newUIControl(ctx context.Context, cancel context.CancelFunc, input io.Reader, output io.Writer) *uiControl {
	u := &uiControl{ctx: ctx, output: json.NewEncoder(output), started: make(chan struct{})}
	// Serialize user stop with the final readiness check and event emission.
	u.cancel = func() {
		u.outputMu.Lock()
		cancel()
		u.outputMu.Unlock()
	}
	// Only an explicit stop or clean EOF owns cancellation of startup I/O.
	// Internal cleanup uses cancel and must not turn a real failure into a stop.
	u.stop = func() {
		u.outputMu.Lock()
		if ctx.Err() == nil {
			u.userStopped = true
		}
		cancel()
		u.outputMu.Unlock()
	}
	go u.readInput(input)
	return u
}

func (u *uiControl) emit(event uiEvent) {
	if u == nil {
		return
	}
	u.outputMu.Lock()
	err := u.output.Encode(event)
	u.outputMu.Unlock()
	if err != nil {
		u.cancel()
	}
}

// waitForMedia observes sends, not setup. A nil audio channel means audio was
// explicitly disabled or unavailable in the non-UI CLI. The caller starts both
// streams first and cancels and joins this waiter when streaming ends.
func (u *uiControl) waitForMedia(ctx context.Context, video, audio <-chan struct{}) {
	if u == nil {
		return
	}
	for video != nil || audio != nil {
		select {
		case <-ctx.Done():
			return
		case <-video:
			video = nil
		case <-audio:
			audio = nil
		}
	}
	u.outputMu.Lock()
	if ctx.Err() != nil {
		u.outputMu.Unlock()
		return
	}
	err := u.output.Encode(uiEvent{Type: "connected", SessionReady: true})
	u.outputMu.Unlock()
	if err != nil {
		u.cancel()
	}
}

// startupError recognizes only the socket closure caused by an owned user stop.
// Media errors never pass through this helper; finish preserves their causes.
func (u *uiControl) startupError(err error) error {
	if u == nil || !errors.Is(err, net.ErrClosed) {
		return err
	}
	u.outputMu.Lock()
	stopped := u.userStopped && errors.Is(context.Cause(u.ctx), context.Canceled)
	u.outputMu.Unlock()
	if stopped {
		return context.Canceled
	}
	return err
}

// finish resolves the session's owned failure after cleanup. Cancellation by
// the user is normal; cancellation caused by media failure is not.
func (u *uiControl) finish(ctx context.Context, resultErr error) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		resultErr = cause
	} else if errors.Is(resultErr, context.Canceled) {
		resultErr = nil
	}
	if resultErr != nil {
		u.emit(uiEvent{Type: "error", Message: resultErr.Error()})
	}
	u.emit(uiEvent{Type: "disconnected"})
	return resultErr
}

func (u *uiControl) readInput(input io.Reader) {
	defer u.cancel()
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		var command uiCommand
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
			u.emit(uiEvent{Type: "error", Message: "Invalid UI control message"})
			return
		}
		switch command.Type {
		case "stop":
			u.stop()
			return
		case "start":
			u.startOnce.Do(func() { close(u.started) })
		case "credential":
			u.mu.Lock()
			if u.pending != nil && command.RequestID == u.pendingID {
				select {
				case u.pending <- command.Value:
				default:
				}
			}
			u.mu.Unlock()
		default:
			u.emit(uiEvent{Type: "error", Message: "Unknown UI control command"})
			return
		}
	}
	if scanner.Err() != nil {
		u.emit(uiEvent{Type: "error", Message: "UI control input failed"})
	} else {
		u.stop()
	}
}

func (u *uiControl) waitForStart() error {
	select {
	case <-u.ctx.Done():
		return u.ctx.Err()
	case <-u.started:
		return u.ctx.Err()
	}
}

func (u *uiControl) credential(prompt string) (string, error) {
	if err := u.ctx.Err(); err != nil {
		return "", err
	}
	u.mu.Lock()
	u.nextID++
	id := u.nextID
	response := make(chan string, 1)
	u.pendingID, u.pending = id, response
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.pending = nil; u.pendingID = 0; u.mu.Unlock() }()
	u.emit(uiEvent{Type: "credential_required", Message: prompt, RequestID: id})
	select {
	case <-u.ctx.Done():
		return "", u.ctx.Err()
	case value := <-response:
		if err := u.ctx.Err(); err != nil {
			return "", err
		}
		if value == "" {
			return "", fmt.Errorf("receiver credential cannot be empty")
		}
		return value, nil
	}
}

func discoverJSON(ctx context.Context, output io.Writer) error {
	devices, err := airplay.DiscoverAirPlayDevices(ctx)
	if err != nil {
		return err
	}
	if devices == nil {
		devices = []airplay.AirPlayDevice{}
	}
	return json.NewEncoder(output).Encode(devices)
}
