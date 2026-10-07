package airplay

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// Linux delivers Pdeathsig when the creating OS thread exits, not strictly
// when the whole process exits. Keep that thread locked until Wait completes.
func startGStreamerCommandPlatform(cmd *exec.Cmd) (<-chan error, error) {
	started := make(chan error, 1)
	waitResult := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
		err := cmd.Start()
		started <- err
		if err != nil {
			close(waitResult)
			return
		}
		waitResult <- cmd.Wait()
		close(waitResult)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return waitResult, nil
}

func interruptCaptureCommand(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
}
