package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/softdaddy-o/soft-ue-index/internal/app"
	"github.com/softdaddy-o/soft-ue-index/internal/cli"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, cli.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	command, err := cli.Parse(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.New(app.Dependencies{}).Run(ctx, command)
}
