//go:build unix

package daemon

import (
	"strings"
	"testing"
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
