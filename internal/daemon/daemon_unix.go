//go:build unix

package daemon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// Spawn re-executes the current binary detached (setsid, stdio redirected to
// <dataDir>/llm-switch.log) and reports the child's startup status on stderr.
// It returns the exit code the foreground parent should exit with.
func Spawn(dataDir string) int {
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
	cmd := exec.Command(exe, os.Args[1:]...)
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
