package main

import (
	"errors"
	"fmt"
	"os"

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
	_, err := cli.Parse(args)
	return err
}
