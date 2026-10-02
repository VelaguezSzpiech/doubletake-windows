package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstanceLockContentionAndRelease(t *testing.T) {
	for _, test := range []struct {
		name    string
		release func(*os.File) error
	}{
		{
			name: "explicit release",
			release: func(file *os.File) error {
				releaseInstanceLock(file)
				return nil
			},
		},
		{
			name: "close releases lock",
			release: func(file *os.File) error {
				return file.Close()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			socketPath := filepath.Join(t.TempDir(), "doubletake.sock")
			first, err := acquireInstanceLock(socketPath)
			if err != nil {
				t.Fatalf("acquire first daemon lock: %v", err)
			}
			t.Cleanup(func() {
				if first != nil {
					releaseInstanceLock(first)
				}
			})

			// A distinct open in the same process must be excluded, not just
			// opens made by another process.
			second, err := acquireInstanceLock(socketPath)
			if second != nil {
				releaseInstanceLock(second)
				t.Fatal("second open acquired an already-owned daemon lock")
			}
			want := "another doubletake daemon is already running for " + socketPath
			if err == nil || err.Error() != want {
				t.Fatalf("second open error = %v, want %q", err, want)
			}

			if err := test.release(first); err != nil {
				t.Fatalf("release first daemon lock: %v", err)
			}
			first = nil
			reacquired, err := acquireInstanceLock(socketPath)
			if err != nil {
				t.Fatalf("reacquire released daemon lock: %v", err)
			}
			t.Cleanup(func() { releaseInstanceLock(reacquired) })
		})
	}
}
