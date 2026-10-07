//go:build !linux && !windows

package airplay

import (
	"os"
	"os/exec"
)

func startGStreamerCommandPlatform(cmd *exec.Cmd) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- cmd.Wait()
		close(waitResult)
	}()
	return waitResult, nil
}

func interruptCaptureCommand(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
}
