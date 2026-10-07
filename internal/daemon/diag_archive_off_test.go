package daemon

import "os"

// Tests start the real binary as a child; keep its diagnostic archive out of the
// user's state directory (it would also evict real session archives).
func init() { os.Setenv("DOUBLETAKE_DIAG_ARCHIVE", "0") }
