package airplay

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	diagArchiveMaxFileBytes = 500 << 20
	diagArchiveKeepFiles    = 6
)

// diagArchive keeps every [DIAG] record of this process in its own file. The tray
// log is capped at about 4 MB (roughly ten minutes of this volume), so a symptom
// that starts after half an hour would otherwise have lost its own onset.
var diagArchive struct {
	mu      sync.Mutex
	once    sync.Once
	file    *os.File
	written int64
}

func diagArchiveWrite(line []byte) {
	// Test binaries must not write into the user's state directory.
	if isTestBinary() || os.Getenv("DOUBLETAKE_DIAG_ARCHIVE") == "0" {
		return
	}
	diagArchive.once.Do(openDiagArchive)
	diagArchive.mu.Lock()
	defer diagArchive.mu.Unlock()
	if diagArchive.file == nil || diagArchive.written > diagArchiveMaxFileBytes {
		return
	}
	n, err := diagArchive.file.Write(append(line, '\n'))
	diagArchive.written += int64(n)
	if err != nil {
		diagArchive.file.Close()
		diagArchive.file = nil
	}
}

func openDiagArchive() {
	dir := doubleTakeStateDir()
	if dir == "" {
		return
	}
	dir = filepath.Join(dir, "diag")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	if entries, err := filepath.Glob(filepath.Join(dir, "diag-*.jsonl")); err == nil && len(entries) >= diagArchiveKeepFiles {
		sort.Strings(entries)
		for _, old := range entries[:len(entries)-diagArchiveKeepFiles+1] {
			os.Remove(old)
		}
	}
	name := "diag-" + strings.ReplaceAll(time.Now().Format("20060102-150405"), ":", "") + "-" + strconv.Itoa(os.Getpid()) + ".jsonl"
	if f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		diagArchive.file = f
	}
}

// isTestBinary reports whether this process is a `go test` binary. The test
// flags are not registered yet when the first record is emitted, so the
// executable name is the reliable signal.
func isTestBinary() bool {
	name := strings.ToLower(filepath.Base(os.Args[0]))
	return strings.HasSuffix(name, ".test") || strings.HasSuffix(name, ".test.exe")
}
