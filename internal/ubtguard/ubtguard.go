// Package ubtguard defers GenerateClangDatabase while another Unreal build is
// running. UBT takes a global mutex per engine, so a background compdb run that
// overlaps a user's build or Build.bat can make the user's build fail.
package ubtguard

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Process is one running process: image name and full command line.
type Process struct {
	PID     int
	Name    string
	Cmdline string
}

// Lister returns the running processes.
type Lister func(context.Context) ([]Process, error)

// Busy returns a description of the first process that looks like an Unreal
// build, or "" when none is running. Process ownPID is ignored.
func Busy(procs []Process, ownPID int) string {
	for _, p := range procs {
		if p.PID == ownPID {
			continue
		}
		name := strings.ToLower(p.Name)
		cmd := strings.ToLower(p.Cmdline)
		switch {
		case name == "unrealbuildtool.exe":
		case (name == "dotnet.exe" || name == "cmd.exe" || name == "powershell.exe" || name == "pwsh.exe") &&
			(strings.Contains(cmd, "unrealbuildtool") || strings.Contains(cmd, "runubt.bat") || strings.Contains(cmd, "\\build\\batchfiles\\build.bat")):
		case name == "unrealeditor-cmd.exe" && strings.Contains(cmd, "-compile"):
		default:
			continue
		}
		return fmt.Sprintf("%s (pid %d)", p.Name, p.PID)
	}
	return ""
}

// Options tunes Wait. Zero values give 15s initial backoff doubling to 2m, 30m cap.
type Options struct {
	Initial, Max, Total time.Duration
	Sleep               func(context.Context, time.Duration) error
	OwnPID              int
}

// ErrBusy is returned when a build was still running after the total wait.
type ErrBusy struct{ Who string }

func (e ErrBusy) Error() string {
	return "Unreal build still running (" + e.Who + "); compilation database generation deferred"
}

// Wait blocks until list reports no competing build, backing off between
// checks. A lister error is treated as "not busy" (best effort: never block
// generation just because process enumeration failed).
func Wait(ctx context.Context, list Lister, o Options, onWait func(who string, next time.Duration)) error {
	if o.Initial <= 0 {
		o.Initial = 15 * time.Second
	}
	if o.Max <= 0 {
		o.Max = 2 * time.Minute
	}
	if o.Total <= 0 {
		o.Total = 30 * time.Minute
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	delay, waited := o.Initial, time.Duration(0)
	for {
		procs, err := list(ctx)
		if err != nil {
			return nil
		}
		who := Busy(procs, o.OwnPID)
		if who == "" {
			return nil
		}
		if waited >= o.Total {
			return ErrBusy{Who: who}
		}
		if onWait != nil {
			onWait(who, delay)
		}
		if err := o.Sleep(ctx, delay); err != nil {
			return err
		}
		waited += delay
		if delay *= 2; delay > o.Max {
			delay = o.Max
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
