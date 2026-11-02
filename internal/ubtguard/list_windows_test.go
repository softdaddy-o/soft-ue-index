//go:build windows

package ubtguard

import (
	"context"
	"os"
	"testing"
)

func TestListProcessesSeesSelf(t *testing.T) {
	procs, err := ListProcesses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == os.Getpid() {
			return
		}
	}
	t.Fatalf("own pid %d not in %d processes", os.Getpid(), len(procs))
}
