// Package compdb generates and validates Unreal compilation databases.
package compdb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const DatabaseName = "compile_commands.json"

// Input contains the discovered Unreal tools and the requested output location.
type Input struct {
	DotNet, UBTDLL, UProject, Target, OutputDir string
	Configuration, Platform                     string
}

// Command is deliberately a process invocation, never a shell command string.
type Command struct {
	Executable string
	Args       []string
}

// BuildCommand creates the supported UBT GenerateClangDatabase invocation.
func BuildCommand(input Input) Command {
	configuration := input.Configuration
	if configuration == "" {
		configuration = "Development"
	}
	platform := input.Platform
	if platform == "" {
		platform = "Win64"
	}
	return Command{Executable: input.DotNet, Args: []string{input.UBTDLL, "-Mode=GenerateClangDatabase", input.Target, platform, configuration, "-Compiler=Clang", "-NoExecCodeGenActions", "-Project=" + input.UProject, "-OutputDir=" + input.OutputDir, "-OutputFilename=" + DatabaseName}}
}

// Runner executes a process while retaining a complete private log and bounded returned output.
type Runner interface {
	Run(context.Context, Command, io.Writer) (stdout, stderr string, err error)
}
type ExecRunner struct{ MaxCapture int }

func (r ExecRunner) Run(ctx context.Context, command Command, log io.Writer) (string, string, error) {
	limit := r.MaxCapture
	if limit <= 0 {
		limit = 64 << 10
	}
	cmd := exec.CommandContext(ctx, command.Executable, command.Args...)
	var out, errOut boundedBuffer
	out.limit, errOut.limit = limit, limit
	cmd.Stdout = io.MultiWriter(&out, log)
	cmd.Stderr = io.MultiWriter(&errOut, log)
	err := cmd.Run()
	return out.String(), errOut.String(), err
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.Len(); remaining > 0 {
		if len(p) > remaining {
			b.Buffer.Write(p[:remaining])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// Generate runs UBT into a freshly-created staging directory, keeping the full tool output in LogPath.
func Generate(ctx context.Context, runner Runner, input Input, stagingRoot, logPath string) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("process runner is required")
	}
	staging, err := os.MkdirTemp(stagingRoot, "compdb-")
	if err != nil {
		return "", fmt.Errorf("create staging: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open generation log: %w", err)
	}
	defer logFile.Close()
	input.OutputDir = staging
	_, stderr, err := runner.Run(ctx, BuildCommand(input), logFile)
	if err != nil {
		return "", fmt.Errorf("generate compilation database: %w: %s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(staging, DatabaseName)); err != nil {
		return "", fmt.Errorf("generated database: %w", err)
	}
	return staging, nil
}
