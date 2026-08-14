//go:build !windows

package unreal

// WindowsAssociationSource is unavailable on non-Windows hosts.
type WindowsAssociationSource struct{}

func (WindowsAssociationSource) Lookup(string) (string, bool, error) { return "", false, nil }
