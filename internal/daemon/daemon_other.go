//go:build !unix

package daemon

import (
	"fmt"
	"os"
)

// Lock is a no-op on platforms without flock: the single-instance guard is
// only enforced on unix.
func Lock(string) (func(), error) { return func() {}, nil }

// Spawn reports that daemon mode is unsupported on this platform.
func Spawn(string) int {
	fmt.Fprintln(os.Stderr, "llm-switch: daemon mode is not supported on this platform")
	return 1
}

// Notify is a no-op: daemon mode is unsupported on this platform.
func Notify(error) {}

// WritePid is a no-op on platforms without daemon support.
func WritePid(string) string { return "" }
