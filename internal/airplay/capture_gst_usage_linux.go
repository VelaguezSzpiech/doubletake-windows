package airplay

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// gstProcessUsage reads utime/stime from /proc/<pid>/stat (USER_HZ is 100 on
// every supported Linux ABI).
func gstProcessUsage(pid int) (gstUsage, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return gstUsage{}, false
	}
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return gstUsage{}, false
	}
	fields := strings.Fields(text[end+1:])
	// fields[0] is state (field 3); utime and stime are fields 14 and 15.
	if len(fields) < 13 {
		return gstUsage{}, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return gstUsage{}, false
	}
	const tick = 10 * time.Millisecond
	return gstUsage{user: time.Duration(utime) * tick, kernel: time.Duration(stime) * tick}, true
}
