//go:build !windows

package ubtguard

import "context"

// ListProcesses is a no-op off Windows (the UBT guard is a Windows concern here).
func ListProcesses(context.Context) ([]Process, error) { return nil, nil }
