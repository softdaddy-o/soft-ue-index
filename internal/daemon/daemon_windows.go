//go:build windows

package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"syscall"

	"github.com/Microsoft/go-winio"
	"github.com/softdaddy-o/soft-ue-index/internal/registry"
	"golang.org/x/sys/windows"
)

func dialPipe(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	return winio.DialPipeContext(ctx, path)
}

func listenPipe(path string) (net.Listener, error) {
	sddl, err := currentUserPipeSDDL()
	if err != nil {
		return nil, err
	}
	config := &winio.PipeConfig{SecurityDescriptor: sddl}
	listener, err := winio.ListenPipe(path, config)
	if err != nil {
		return nil, err
	}
	return listener, nil
}

func currentUserPipeSDDL() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("D:P(A;;GA;;;%s)(A;;GA;;;SY)", user.User.Sid.String()), nil
}

func StartDetached(executable string, args ...string) error {
	cmd := exec.Command(executable, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x08000000}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func tryAcquireFileLock(path string) (func(), bool, error) {
	return registry.TryAcquireFileLock(path)
}
