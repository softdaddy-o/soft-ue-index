// Package cli parses soft-ue-index command-line arguments.
package cli

import (
	"errors"
	"fmt"
)

// ErrUsage identifies an invalid command-line invocation.
var ErrUsage = errors.New("usage error")

// Command is a parsed soft-ue-index command.
type Command struct {
	Name        string
	ProjectPath string
	ProjectName string
	JSON        bool
	EngineScope string
}

// Parse converts command-line arguments into a Command.
func Parse(args []string) (Command, error) {
	command := Command{}
	positionals := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			command.JSON = true
			continue
		}
		if arg == "--engine-scope" || arg == "--engine-scope=project" {
			command.EngineScope = "project"
			continue
		}
		if arg == "--engine-scope=full" {
			command.EngineScope = "full"
			continue
		}
		positionals = append(positionals, arg)
	}

	if len(positionals) == 0 {
		return Command{}, usageError("missing command")
	}

	command.Name = positionals[0]
	switch command.Name {
	case "doctor", "list", "watch", "mcp":
		if len(positionals) != 1 {
			return Command{}, usageError("%s does not accept arguments", command.Name)
		}
	case "add":
		if len(positionals) != 2 {
			return Command{}, usageError("add requires a project path")
		}
		command.ProjectPath = positionals[1]
	case "generate", "status", "remove":
		if len(positionals) != 2 {
			return Command{}, usageError("%s requires a project name", command.Name)
		}
		command.ProjectName = positionals[1]
	default:
		return Command{}, usageError("unknown command %q", command.Name)
	}
	if command.EngineScope != "" && command.Name != "generate" {
		return Command{}, usageError("--engine-scope only applies to generate")
	}

	return command, nil
}

func usageError(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUsage}, args...)...)
}
