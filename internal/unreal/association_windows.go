//go:build windows

package unreal

import "golang.org/x/sys/windows/registry"

// WindowsAssociationSource reads engine associations registered for the current user.
type WindowsAssociationSource struct{}

func (WindowsAssociationSource) Lookup(association string) (string, bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Epic Games\Unreal Engine\Builds`, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer key.Close()
	root, _, err := key.GetStringValue(association)
	if err == registry.ErrNotExist {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return root, true, nil
}
