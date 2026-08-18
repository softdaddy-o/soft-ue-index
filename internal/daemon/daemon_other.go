//go:build !windows

package daemon

import (
	"context"
	"io"
	"net"

	"github.com/softdaddy-o/soft-ue-index/internal/registry"
)

func StartDetached(string, ...string) error { return ErrUnsupportedTransport }

func dialPipe(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, ErrUnsupportedTransport
}

type nullListener struct{}

func (n *nullListener) Accept() (net.Conn, error) { return nil, ErrUnsupportedTransport }
func (n *nullListener) Close() error              { return nil }
func (n *nullListener) Addr() net.Addr            { return &net.IPAddr{} }

func listenPipe(string) (net.Listener, error) {
	return &nullListener{}, nil
}

func tryAcquireFileLock(path string) (func(), bool, error) {
	return registry.TryAcquireFileLock(path)
}
