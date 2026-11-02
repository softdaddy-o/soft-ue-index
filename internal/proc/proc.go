// Package proc is the single place child processes are created. On Windows
// every child gets CREATE_NO_WINDOW so a console program (dotnet, clangd,
// clangd-indexer, powershell) never opens its own console window when the
// parent has no console (the daemon runs DETACHED_PROCESS).
package proc

import (
	"context"
	"os/exec"
)

// Command is exec.Command with the platform's no-window policy applied.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	hideWindow(cmd)
	return cmd
}

// CommandContext is exec.CommandContext with the platform's no-window policy applied.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	return cmd
}
