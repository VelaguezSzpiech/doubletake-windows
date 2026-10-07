//go:build !linux && !windows

package airplay

func gstProcessUsage(pid int) (gstUsage, bool) { return gstUsage{}, false }
