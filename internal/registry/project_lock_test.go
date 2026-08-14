package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireFileLockSerializesIndependentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "generation.lock")
	release, err := AcquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		unlock, e := AcquireFileLock(context.Background(), path)
		if e == nil {
			acquired <- unlock
		}
	}()
	select {
	case <-acquired:
		t.Fatal("second caller bypassed lock")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case unlock := <-acquired:
		unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("second caller never acquired lock")
	}
}
