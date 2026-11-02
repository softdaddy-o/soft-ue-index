//go:build windows

package ubtguard

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/softdaddy-o/soft-ue-index/internal/proc"
)

// ListProcesses enumerates processes (with command lines) through CIM.
func ListProcesses(ctx context.Context) ([]Process, error) {
	script := "Get-CimInstance Win32_Process | Select-Object ProcessId,Name,CommandLine | ConvertTo-Json -Compress"
	cmd := proc.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var raw []struct {
		ProcessId   int
		Name        string
		CommandLine *string
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &raw); err != nil {
		return nil, err
	}
	procs := make([]Process, 0, len(raw))
	for _, r := range raw {
		p := Process{PID: r.ProcessId, Name: r.Name}
		if r.CommandLine != nil {
			p.Cmdline = *r.CommandLine
		}
		procs = append(procs, p)
	}
	return procs, nil
}
