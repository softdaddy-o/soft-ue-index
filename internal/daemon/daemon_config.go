package daemon

import (
	"context"
	"errors"
	"io"
	"time"
)

// RunConfig provides all knobs for EnsureRunning with defaults for normal callers.
type RunConfig struct {
	// Start starts the daemon process.
	Start func(context.Context) error
	// Dialer allows injecting transport for tests.
	Dialer func(ctx context.Context, pipePath string) (io.ReadWriteCloser, error)
	// PipePath overrides transport endpoint.
	PipePath string
	// StartupLock overrides startup lock location.
	StartupLockPath string
	// Timeout bounds waiting for readiness after startup.
	Timeout time.Duration
}

func (c *RunConfig) normalize() error {
	if c.Timeout <= 0 {
		c.Timeout = defaultPollWindow
	}
	return nil
}

func (c *RunConfig) effectivePipePath() (string, error) {
	if c.PipePath != "" {
		return c.PipePath, nil
	}
	return DefaultPipePath()
}

func (c *RunConfig) client() (*Client, error) {
	pipePath, err := c.effectivePipePath()
	if err != nil {
		return nil, err
	}
	client := &Client{
		Timeout:  c.Timeout,
		PipePath: pipePath,
		Dialer:   c.Dialer,
	}
	if client.Timeout <= 0 {
		client.Timeout = 2 * time.Second
	}
	return client, nil
}

func (c *RunConfig) startupLock() (func(), bool, error) {
	lockPath := c.StartupLockPath
	if lockPath == "" {
		var err error
		lockPath, err = DefaultStartupLockPath()
		if err != nil {
			return nil, false, err
		}
	}
	release, acquired, err := tryAcquireFileLock(lockPath)
	if err != nil {
		return nil, false, err
	}
	return release, acquired, nil
}

func (c *RunConfig) timeout() time.Duration {
	if c.Timeout <= 0 {
		return defaultPollWindow
	}
	return c.Timeout
}

// ErrUnsupportedTransport can be used by platform-specific shims.
var ErrUnsupportedTransport = errors.New("daemon transport is not supported on this platform")
