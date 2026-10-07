package airplay

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func configureCaptureCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// Start suspended so neither GStreamer nor a plugin-created child can escape
// the Job Object before assignment. The non-inherited job handle is retained
// until Wait completes; Windows closes it on parent death and kills every job
// member. Failure to establish supervision is a startup error, not a fallback.
func startGStreamerCommandPlatform(cmd *exec.Cmd) (<-chan error, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create capture job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure capture job: %w", err)
	}
	configureCaptureCommand(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	fail := func(err error) (<-chan error, error) {
		_ = cmd.Process.Kill()
		_ = windows.CloseHandle(job)
		_ = cmd.Wait()
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(fmt.Errorf("open capture process: %w", err))
	}
	err = windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if err != nil {
		return fail(fmt.Errorf("assign capture process to job: %w", err))
	}
	if err := resumeCaptureProcess(uint32(cmd.Process.Pid)); err != nil {
		return fail(fmt.Errorf("resume capture process: %w", err))
	}
	waitResult := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = windows.CloseHandle(job)
		waitResult <- err
		close(waitResult)
	}()
	return waitResult, nil
}

// os/exec closes CreateProcess's initial thread handle. Find that thread while
// it is still suspended (no user code has run) and use the documented Win32
// ResumeThread API rather than relying on undocumented NT process APIs.
func resumeCaptureProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		return err
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return err
	}
	return fmt.Errorf("initial thread for process %d was not found", pid)
}

func interruptCaptureCommand(cmd *exec.Cmd) {
	// os.Interrupt is not supported for hidden Windows subprocesses.
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
