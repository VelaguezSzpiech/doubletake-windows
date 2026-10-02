package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run the production entry point in an isolated process: main owns flag parsing,
// stdin, signals and the exit code. No receiver response means capture cannot start.
func TestCLIStartupProcessHelper(t *testing.T) {
	if os.Getenv("DOUBLETAKE_STARTUP_PROCESS_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"doubletake"}, os.Args[i+1:]...)
			flag.CommandLine = flag.NewFlagSet("doubletake", flag.ExitOnError)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

type startupCLIProcess struct {
	input   io.WriteCloser
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	done    chan struct{}
	waitErr error
}

func startStartupCLI(t *testing.T, port int) *startupCLIProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	args := []string{
		"-test.run=^TestCLIStartupProcessHelper$", "--", "-ui",
		"-target=127.0.0.1", "-port=" + strconv.Itoa(port),
		"-creds=" + t.TempDir() + "/credentials.json",
	}
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), "DOUBLETAKE_STARTUP_PROCESS_HELPER=1", "DOUBLETAKE_CODE=")
	process := &startupCLIProcess{done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &process.stdout, &process.stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	process.input = input
	t.Cleanup(func() { _ = input.Close() })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-process.done
	})
	if err := json.NewEncoder(input).Encode(uiCommand{Type: "start"}); err != nil {
		t.Fatal(err)
	}
	return process
}

func (p *startupCLIProcess) wait(t *testing.T) ([]uiEvent, error) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("CLI did not exit within five seconds")
	}
	var events []uiEvent
	decoder := json.NewDecoder(&p.stdout)
	for {
		var event uiEvent
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode CLI events: %v; stderr: %s", err, p.stderr.String())
		}
		events = append(events, event)
	}
	return events, p.waitErr
}

func TestCLIStartupCancellationWhileWaitingForInfo(t *testing.T) {
	for _, command := range []string{"stop", "EOF"} {
		t.Run(command, func(t *testing.T) {
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			requestSeen := make(chan error, 1)
			connectionClosed := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					requestSeen <- err
					return
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					requestSeen <- err
					return
				}
				reader := bufio.NewReader(conn)
				line, err := reader.ReadString('\n')
				if err != nil {
					requestSeen <- err
					return
				}
				if strings.TrimSpace(line) != "GET /info RTSP/1.0" {
					requestSeen <- fmt.Errorf("unexpected startup request: %q", line)
					return
				}
				for {
					line, err = reader.ReadString('\n')
					if err != nil {
						requestSeen <- err
						return
					}
					if line == "\r\n" {
						break
					}
				}
				// Withhold the response until the CLI closes this real TCP socket.
				requestSeen <- nil
				_, err = reader.ReadByte()
				connectionClosed <- err
			}()
			process := startStartupCLI(t, listener.Addr().(*net.TCPAddr).Port)
			select {
			case err := <-requestSeen:
				if err != nil {
					t.Fatalf("receive startup request: %v", err)
				}
			case <-process.done:
				t.Fatalf("CLI exited before GET /info: %v; stdout: %s; stderr: %s", process.waitErr, process.stdout.String(), process.stderr.String())
			case <-time.After(5 * time.Second):
				t.Fatal("CLI did not send GET /info within five seconds")
			}
			if command == "stop" {
				err = json.NewEncoder(process.input).Encode(uiCommand{Type: "stop"})
			} else {
				err = process.input.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			events, exitErr := process.wait(t)
			if exitErr != nil {
				t.Fatalf("user cancellation exited with failure: %v; events: %+v; stderr: %s", exitErr, events, process.stderr.String())
			}
			if len(events) != 1 || events[0].Type != "disconnected" {
				t.Fatalf("user cancellation events = %+v, want only disconnected; stderr: %s", events, process.stderr.String())
			}
			select {
			case err := <-connectionClosed:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("receiver connection shutdown = %v, want EOF", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled CLI left receiver connection open")
			}
		})
	}
}

func TestCLIStartupReceiverErrorWithoutStopRemainsFailure(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// The receiver closes its accepted socket without replying. Unlike a closed
	// ephemeral port, this cannot race with another process claiming the port.
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			serverDone <- err
			return
		}
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverDone <- err
				return
			}
			if line == "\r\n" {
				break
			}
		}
		serverDone <- conn.Close()
	}()
	process := startStartupCLI(t, listener.Addr().(*net.TCPAddr).Port)
	// Keep stdin open: neither an explicit stop nor EOF owns this failure.
	events, exitErr := process.wait(t)
	var status *exec.ExitError
	if !errors.As(exitErr, &status) || status.ExitCode() != 1 {
		t.Fatalf("receiver failure exit = %v, want exit 1; events: %+v; stderr: %s", exitErr, events, process.stderr.String())
	}
	if len(events) != 2 || events[0].Type != "error" || events[1].Type != "disconnected" {
		t.Fatalf("receiver failure events = %+v, want error then disconnected", events)
	}
	if !strings.Contains(events[0].Message, "get info failed:") {
		t.Fatalf("receiver error lost startup diagnostic: %+v", events[0])
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not finish")
	}
}
