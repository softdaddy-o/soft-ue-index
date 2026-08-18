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

func TestTryAcquireFileLockReturnsImmediatelyWhenOwned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.lock")
	release, acquired, err := TryAcquireFileLock(path)
	if err != nil || !acquired {
		t.Fatalf("first acquisition: acquired=%t err=%v", acquired, err)
	}
	defer release()

	started := time.Now()
	secondRelease, secondAcquired, err := TryAcquireFileLock(path)
	if err != nil || secondAcquired || secondRelease != nil {
		t.Fatalf("second acquisition: release=%v acquired=%t err=%v", secondRelease != nil, secondAcquired, err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("nonblocking acquisition took %v", elapsed)
	}
}
