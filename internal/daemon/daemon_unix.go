//go:build unix

package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Lock acquires the single-instance flock on <dataDir>/llm-switch.lock,
// creating the data directory (0700) if needed. The returned func releases
// the lock; the kernel also releases it automatically on process death.
func Lock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, LockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another llm-switch instance is already running (data dir %s)", dataDir)
	}
	return func() { _ = f.Close() }, nil
}

// SpawnArgs re-executes the current binary detached (setsid, stdio
// redirected to <dataDir>/llm-switch.log) with args, and reports the child's
// startup status on stderr. It returns the exit code the foreground parent
// should exit with.
func SpawnArgs(dataDir string, args []string) int {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "llm-switch: %v\n", err)
		return 1
	}
	logPath := filepath.Join(dataDir, LogFile)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llm-switch: %v\n", err)
		return 1
	}
	defer logFile.Close()

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "llm-switch: %v\n", err)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "llm-switch: %v\n", err)
		return 1
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), ChildEnv+"=1")
	cmd.Stdin = nil // /dev/null
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{pipeW} // child fd 3
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "llm-switch: %v\n", err)
		return 1
	}
	_ = pipeW.Close() // parent holds only the read end

	_ = pipeR.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := bufio.NewReader(pipeR).ReadString('\n')
	_ = pipeR.Close()
	status := strings.TrimSpace(line)
	switch {
	case status == "READY":
		fmt.Fprintf(os.Stderr, "llm-switch started (pid %d), logs: %s\n", cmd.Process.Pid, logPath)
		return 0
	case strings.HasPrefix(status, "ERR "):
		fmt.Fprintf(os.Stderr, "llm-switch: %s\n", strings.TrimPrefix(status, "ERR "))
		_ = cmd.Wait()
		return 1
	case err == io.EOF || status == "":
		fmt.Fprintf(os.Stderr, "llm-switch: daemon exited during startup; see %s\n", logPath)
		_ = cmd.Wait()
		return 1
	default: // read deadline exceeded
		fmt.Fprintf(os.Stderr, "llm-switch: timed out waiting for daemon startup; see %s\n", logPath)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 1
	}
}

var notifyOnce sync.Once

// Notify reports daemon-child startup status to the waiting parent over the
// inherited pipe (fd 3): READY on success, ERR <msg> on failure. It is a
// no-op outside daemon mode and idempotent.
func Notify(err error) {
	notifyOnce.Do(func() {
		if !IsChild() {
			return
		}
		pipe := os.NewFile(3, "daemon-ready")
		if pipe == nil {
			return
		}
		defer pipe.Close()
		if err != nil {
			fmt.Fprintf(pipe, "ERR %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		} else {
			fmt.Fprintln(pipe, "READY")
		}
	})
}

// WritePid writes the daemon pid file into the data directory and returns
// its path ("" on failure; best effort).
func WritePid(dataDir string) string {
	path := filepath.Join(dataDir, PidFile)
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return ""
	}
	return path
}

// Alive reports whether the process is runnable: it exists, or it exists but
// is owned by another user (EPERM — out of reach, but alive).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Stop terminates the running instance: SIGTERM, then wait up to grace for
// the single-instance flock to become acquirable — the authoritative exit
// signal, since the kernel drops it even on SIGKILL. With force, SIGKILL
// after the grace period elapses. Returns ErrNotRunning (stale leftovers
// cleaned) when nothing is running.
func Stop(dataDir string, grace time.Duration, force bool) error {
	pid := ReadPid(dataDir)
	if pid <= 0 || !Alive(pid) {
		// Nothing running — or stale leftovers. The lock arbitrates: if it
		// is held anyway, the pid file is lying and we refuse to guess.
		unlock, err := Lock(dataDir)
		if err != nil {
			return fmt.Errorf("an llm-switch instance is using %s but %s is missing or unreadable", dataDir, PidFile)
		}
		removeRunFiles(dataDir)
		unlock()
		return ErrNotRunning
	}
	if !looksLikeUs(pid) {
		return fmt.Errorf("pid %d in %s does not look like llm-switch; not touched", pid, PidFile)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("failed to signal pid %d: %w", pid, err)
	}
	unlock, err := acquireWithin(dataDir, grace)
	if err != nil {
		if !force {
			return fmt.Errorf("instance (pid %d) did not exit within %s; retry with --force to kill it", pid, grace)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
		if unlock, err = acquireWithin(dataDir, 5*time.Second); err != nil {
			return fmt.Errorf("instance (pid %d) did not exit after SIGKILL: %w", pid, err)
		}
	}
	removeRunFiles(dataDir)
	unlock()
	return nil
}

// acquireWithin polls the single-instance lock until it can be acquired —
// meaning the previous holder exited — or the timeout elapses. On success
// the caller owns the lock and must invoke the returned release func.
func acquireWithin(dataDir string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		unlock, err := Lock(dataDir)
		if err == nil {
			return unlock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for the instance to exit", timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// looksLikeUs is a best-effort pid-reuse guard: on Linux, /proc/<pid>/exe
// must resolve to a program with the same name as this binary. Elsewhere the
// check is skipped.
func looksLikeUs(pid int) bool {
	if runtime.GOOS != "linux" {
		return true
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		// Kernel thread, zombie, or permission issue: do not guess.
		return false
	}
	// The binary may have been replaced by a self-update (rename over the
	// running file); the kernel marks the link "(deleted)".
	exe = strings.TrimSuffix(exe, " (deleted)")
	me, err := os.Executable()
	if err != nil {
		me = "llm-switch"
	}
	return filepath.Base(exe) == filepath.Base(me)
}
