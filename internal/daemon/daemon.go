// Package daemon implements background (daemon) startup and the
// single-instance lock for llm-switch.
//
// With --daemon, the foreground process re-executes itself detached (setsid,
// stdio redirected to <data-dir>/llm-switch.log) and waits on a readiness
// pipe so startup failures — "already running", bind errors — are reported
// to the terminal with a non-zero exit code instead of vanishing into the
// log file.
//
// The single-instance lock is an flock on <data-dir>/llm-switch.lock held
// for the process lifetime. It is acquired in every mode (foreground and
// daemon) to protect the single-writer SQLite database, and is released
// automatically by the kernel when the process dies — no stale pidfiles.
//
// The running instance also records a pid file and a run-meta file in the
// data directory; the lifecycle commands (status, stop, upgrade) read them.
package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNotRunning is returned by Stop when no instance holds the data dir.
var ErrNotRunning = errors.New("llm-switch is not running")

// ChildEnv marks the re-executed daemon child so it serves instead of
// spawning again.
const ChildEnv = "LLM_SWITCH_DAEMON_CHILD"

// File names inside the data directory. The pid file stays a plain number:
// `kill $(cat <dataDir>/llm-switch.pid)` is a documented stop recipe, and a
// JSON line there would word-split into extra argv for kill.
const (
	LockFile = "llm-switch.lock"
	PidFile  = "llm-switch.pid"
	MetaFile = "llm-switch.meta"
	LogFile  = "llm-switch.log"
)

// IsChild reports whether this process is the re-executed daemon child.
func IsChild() bool {
	return os.Getenv(ChildEnv) == "1"
}

// RunMeta describes the running llm-switch instance. It lives next to the
// pid file so lifecycle commands can report the instance's version and
// restart it with its original flags after a self-update.
type RunMeta struct {
	PID       int      `json:"pid"`
	Args      []string `json:"args,omitempty"`
	Version   string   `json:"version,omitempty"`
	StartedAt int64    `json:"started_at"`
}

// WriteRunMeta writes the run metadata file into the data directory and
// returns its path ("" on failure; best effort).
func WriteRunMeta(dataDir string, meta RunMeta) string {
	b, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	path := filepath.Join(dataDir, MetaFile)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return ""
	}
	return path
}

// ReadRunMeta parses the run metadata file; ok is false when it is missing
// or corrupt.
func ReadRunMeta(dataDir string) (RunMeta, bool) {
	var meta RunMeta
	b, err := os.ReadFile(filepath.Join(dataDir, MetaFile))
	if err != nil {
		return meta, false
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, false
	}
	return meta, true
}

// ReadPid parses the plain-number pid file; 0 when missing or garbage.
func ReadPid(dataDir string) int {
	b, err := os.ReadFile(filepath.Join(dataDir, PidFile))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// RunningInstance probes the data dir for a live instance: the run-meta pid
// wins, the plain pid file is the fallback (metaOK false in that case).
// pid <= 0 means nothing is running.
func RunningInstance(dataDir string) (pid int, meta RunMeta, metaOK bool) {
	meta, metaOK = ReadRunMeta(dataDir)
	if metaOK && Alive(meta.PID) {
		return meta.PID, meta, true
	}
	if candidate := ReadPid(dataDir); Alive(candidate) {
		return candidate, meta, false
	}
	return 0, meta, metaOK
}

// removeRunFiles clears the pid file (best effort); the run-meta file is
// kept on purpose so `restart` can start a cleanly stopped instance with its
// saved args (stale meta is harmless: probes verify the pid is alive).
// Callers must hold the single-instance lock so a concurrently starting
// instance's file is not deleted.
func removeRunFiles(dataDir string) {
	_ = os.Remove(filepath.Join(dataDir, PidFile))
}
