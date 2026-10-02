package airplay

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLinuxCaptureProcessHelper(t *testing.T) {
	switch os.Getenv("DOUBLETAKE_CAPTURE_PROCESS_HELPER") {
	case "sleep":
		_, _ = os.Stdout.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxCaptureProcessHelper$")
		cmd.Env = append(os.Environ(), "DOUBLETAKE_CAPTURE_PROCESS_HELPER=sleep")
		cmd.Stdout = os.Stdout
		if _, err := startGStreamerCommand(cmd); err != nil {
			os.Exit(41)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func TestLinuxCaptureProcessDiesWithParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	parent := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLinuxCaptureProcessHelper$")
	parent.Env = append(os.Environ(), "DOUBLETAKE_CAPTURE_PROCESS_HELPER=parent")
	pipe, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	reader := bufio.NewReader(pipe)
	pidLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidLine))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Process.Kill(); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal(err)
	}
	// The child inherited the pipe. Parent death alone cannot produce EOF:
	// Pdeathsig must kill the child too and close its inherited writer.
	eof := make(chan error, 1)
	go func() { _, err := reader.ReadByte(); eof <- err }()
	select {
	case err := <-eof:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("child output after parent death = %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("capture child survived parent death")
	}
}
