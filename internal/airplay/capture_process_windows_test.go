//go:build windows

package airplay

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestCaptureProcessHelper(t *testing.T) {
	switch os.Getenv("DOUBLETAKE_CAPTURE_PROCESS_HELPER") {
	case "exit":
		os.Exit(23)
	case "sleep":
		_, _ = os.Stdout.WriteString("ready\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	case "parent":
		cmd := captureProcessTestCommand(t, "sleep")
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			os.Exit(40)
		}
		if _, err := startGStreamerCommand(cmd); err != nil {
			os.Exit(41)
		}
		if _, err := bufio.NewReader(pipe).ReadString('\n'); err != nil {
			os.Exit(42)
		}
		_, _ = os.Stdout.WriteString(strconv.Itoa(cmd.Process.Pid) + "\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func captureProcessTestCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCaptureProcessHelper$")
	cmd.Env = append(os.Environ(), "DOUBLETAKE_CAPTURE_PROCESS_HELPER="+mode)
	return cmd
}

func TestWindowsCaptureProcessReportsExit(t *testing.T) {
	cmd := captureProcessTestCommand(t, "exit")
	wait, err := startGStreamerCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-wait:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 23 {
			t.Fatalf("wait error = %v, want exit code 23", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("capture child did not exit")
	}
}

func TestWindowsCaptureProcessCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCaptureProcessHelper$")
	cmd.Env = append(os.Environ(), "DOUBLETAKE_CAPTURE_PROCESS_HELPER=sleep")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	wait, err := startGStreamerCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if _, err := bufio.NewReader(pipe).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-wait:
		if err == nil {
			t.Fatal("canceled child reported successful exit")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("context cancellation left capture running")
	}
}

func TestWindowsCaptureProcessDiesWithParent(t *testing.T) {
	parent := captureProcessTestCommand(t, "parent")
	pipe, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	pidLine, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(pidLine[:len(pidLine)-1])
	if err != nil {
		t.Fatal(err)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	defer windows.TerminateProcess(child, 1)
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	result, err := windows.WaitForSingleObject(child, 10000)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("child after parent death: wait = %d, error = %v", result, err)
	}
}

func TestWindowsCaptureProcessStartFailure(t *testing.T) {
	cmd := exec.Command("doubletake-missing-capture-program-for-test.exe")
	wait, err := startGStreamerCommand(cmd)
	if err == nil || wait != nil {
		t.Fatalf("missing capture child start = (%v, %v), want nil channel and error", wait, err)
	}
}
