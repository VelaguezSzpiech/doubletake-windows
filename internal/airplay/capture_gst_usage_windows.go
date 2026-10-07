package airplay

import (
	"time"

	"golang.org/x/sys/windows"
)

// gstProcessUsage samples a GStreamer child's cumulative CPU time and priority
// class for diagnostics. It only queries the process; it never changes it.
func gstProcessUsage(pid int) (gstUsage, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return gstUsage{}, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return gstUsage{}, false
	}
	usage := gstUsage{user: filetimeSpan(user), kernel: filetimeSpan(kernel)}
	if class, err := windows.GetPriorityClass(h); err == nil {
		usage.priorityClass = class
	}
	return usage, true
}

func filetimeSpan(ft windows.Filetime) time.Duration {
	return time.Duration((uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)) * 100)
}
