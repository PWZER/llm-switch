//go:build unix

package daemon

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLockSingleInstance(t *testing.T) {
	dir := t.TempDir()

	unlock, err := Lock(dir)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	// A second flock on a different fd of the same file fails with LOCK_NB,
	// even within one process — this is what guards the data dir.
	if _, err := Lock(dir); err == nil {
		t.Fatal("second Lock succeeded, want single-instance error")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Lock error = %v, want 'already running'", err)
	}

	unlock()
	unlock2, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	unlock2()
}

func TestNotifyNoopOutsideDaemonMode(t *testing.T) {
	// Must not panic or block when not re-executed as a daemon child.
	Notify(nil)
}

func TestWriteReadRunMeta(t *testing.T) {
	dir := t.TempDir()

	meta := RunMeta{PID: 1234, Args: []string{"--daemon", "--addr", ":8902"}, Version: "v1.2.3", StartedAt: 1700000000}
	if path := WriteRunMeta(dir, meta); path == "" {
		t.Fatal("WriteRunMeta failed")
	}
	got, ok := ReadRunMeta(dir)
	if !ok {
		t.Fatal("ReadRunMeta after write: ok = false")
	}
	if !reflect.DeepEqual(got, meta) {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", got, meta)
	}

	// A corrupt file reads as "no metadata", not an error.
	if err := os.WriteFile(filepath.Join(dir, MetaFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadRunMeta(dir); ok {
		t.Fatal("ReadRunMeta on corrupt file: ok = true")
	}

	// A missing file likewise.
	if _, ok := ReadRunMeta(t.TempDir()); ok {
		t.Fatal("ReadRunMeta on missing file: ok = true")
	}
}

func TestReadPidPlain(t *testing.T) {
	dir := t.TempDir()
	if pid := ReadPid(dir); pid != 0 {
		t.Fatalf("ReadPid on missing file = %d, want 0", pid)
	}

	if err := os.WriteFile(filepath.Join(dir, PidFile), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid := ReadPid(dir); pid != 4242 {
		t.Fatalf("ReadPid = %d, want 4242", pid)
	}

	if err := os.WriteFile(filepath.Join(dir, PidFile), []byte(`{"pid":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid := ReadPid(dir); pid != 0 {
		t.Fatalf("ReadPid on garbage = %d, want 0", pid)
	}
}

func TestAliveSelf(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("Alive(self) = false")
	}
	if Alive(0) {
		t.Fatal("Alive(0) = true")
	}
	// Beyond the default pid_max on any supported kernel: cannot exist.
	if Alive(99999999) {
		t.Fatal("Alive(nonexistent) = true")
	}
}

func TestStopNotRunning(t *testing.T) {
	dir := t.TempDir()
	if err := Stop(dir, time.Second, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop on empty dir = %v, want ErrNotRunning", err)
	}

	// Stale leftovers of a dead instance are cleaned up.
	if err := os.WriteFile(filepath.Join(dir, PidFile), []byte("99999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, MetaFile), []byte(`{"pid":99999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Stop(dir, time.Second, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop with stale files = %v, want ErrNotRunning", err)
	}
	for _, f := range []string{PidFile, MetaFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s still present after Stop", f)
		}
	}
}

// TestStopTerminatesInstance exercises the real SIGTERM → flock-release
// sequence against a child process holding the lock.
func TestStopTerminatesInstance(t *testing.T) {
	if dir := os.Getenv("LLM_SWITCH_TEST_HOLD_LOCK"); dir != "" {
		holdLockChild(dir, false)
		return // holdLockChild exits
	}
	stopTerminatesInstance(t, false)
}

// TestStopForceKillsIgnorantInstance covers the --force path: the child
// ignores SIGTERM, so only SIGKILL after the grace period ends it.
func TestStopForceKillsIgnorantInstance(t *testing.T) {
	if dir := os.Getenv("LLM_SWITCH_TEST_HOLD_LOCK"); dir != "" {
		holdLockChild(dir, os.Getenv("LLM_SWITCH_TEST_IGNORE_TERM") == "1")
		return
	}
	stopTerminatesInstance(t, true)
}

// stopTerminatesInstance spawns this test binary as a lock-holding child,
// runs Stop against it, and asserts the instance is gone and its files
// cleaned up.
func stopTerminatesInstance(t *testing.T, force bool) {
	t.Helper()
	dir := t.TempDir()

	env := append(os.Environ(), "LLM_SWITCH_TEST_HOLD_LOCK="+dir)
	if force {
		env = append(env, "LLM_SWITCH_TEST_IGNORE_TERM=1")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	// Wait for the child to take the lock and announce its pid.
	deadline := time.Now().Add(10 * time.Second)
	var pid int
	for {
		if pid = ReadPid(dir); pid > 0 && Alive(pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never wrote a live pid file")
		}
		time.Sleep(20 * time.Millisecond)
	}

	grace := 5 * time.Second
	if force {
		// Keep the test fast: 1s of ignored SIGTERM, then SIGKILL.
		grace = time.Second
	}
	if err := Stop(dir, grace, force); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	for _, f := range []string{PidFile, MetaFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s still present after Stop", f)
		}
	}
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after Stop: %v", err)
	}
	unlock()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit after Stop")
	}
	_ = pid
}

// holdLockChild is the env-guarded branch the tests above re-exec: take the
// single-instance lock, publish the pid + meta files, and wait to be
// stopped.
func holdLockChild(dir string, ignoreTerm bool) {
	if ignoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	unlock, err := Lock(dir)
	if err != nil {
		os.Exit(1)
	}
	defer unlock()
	if WritePid(dir) == "" {
		os.Exit(1)
	}
	if WriteRunMeta(dir, RunMeta{PID: os.Getpid(), Version: "test"}) == "" {
		os.Exit(1)
	}
	time.Sleep(30 * time.Second) // terminated by Stop
	os.Exit(0)
}
