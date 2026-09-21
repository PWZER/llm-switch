//go:build !unix

package daemon

import (
	"fmt"
	"os"
	"time"
)

// Lock is a no-op on platforms without flock: the single-instance guard is
// only enforced on unix.
func Lock(string) (func(), error) { return func() {}, nil }

// SpawnArgs reports that daemon mode is unsupported on this platform.
func SpawnArgs(_ string, _ []string) int {
	fmt.Fprintln(os.Stderr, "llm-switch: daemon mode is not supported on this platform")
	return 1
}

// Notify is a no-op: daemon mode is unsupported on this platform.
func Notify(error) {}

// WritePid is a no-op on platforms without daemon support.
func WritePid(string) string { return "" }

// Alive is always false on platforms without process-signal support.
func Alive(int) bool { return false }

// Stop always reports ErrNotRunning on platforms without daemon support, so
// lifecycle commands degrade to "nothing to restart/stop".
func Stop(string, time.Duration, bool) error { return ErrNotRunning }
