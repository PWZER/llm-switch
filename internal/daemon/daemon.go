// Package daemon implements background (daemon) startup and the
// single-instance lock for llm-switch.
//
// With -daemon, the foreground process re-executes itself detached (setsid,
// stdio redirected to <data-dir>/llm-switch.log) and waits on a readiness
// pipe so startup failures — "already running", bind errors — are reported
// to the terminal with a non-zero exit code instead of vanishing into the
// log file.
//
// The single-instance lock is an flock on <data-dir>/llm-switch.lock held
// for the process lifetime. It is acquired in every mode (foreground and
// daemon) to protect the single-writer SQLite database, and is released
// automatically by the kernel when the process dies — no stale pidfiles.
package daemon

import "os"

// ChildEnv marks the re-executed daemon child so it serves instead of
// spawning again.
const ChildEnv = "LLM_SWITCH_DAEMON_CHILD"

// File names inside the data directory.
const (
	LockFile = "llm-switch.lock"
	PidFile  = "llm-switch.pid"
	LogFile  = "llm-switch.log"
)

// IsChild reports whether this process is the re-executed daemon child.
func IsChild() bool {
	return os.Getenv(ChildEnv) == "1"
}
