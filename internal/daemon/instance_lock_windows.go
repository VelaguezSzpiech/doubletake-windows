package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockInstanceFile(file *os.File) error {
	// Lock byte zero even for an empty file. All daemon instances use the
	// same range; FAIL_IMMEDIATELY also excludes another open in this process.
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
}

func unlockInstanceFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}

func isInstanceLockContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
