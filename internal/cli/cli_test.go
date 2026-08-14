package cli

import (
	"errors"
	"testing"
)

func TestParseReturnsUsageErrorWithoutCommand(t *testing.T) {
	_, err := Parse(nil)

	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Parse(nil) error = %v, want ErrUsage", err)
	}
}

func TestParseAddCommand(t *testing.T) {
	command, err := Parse([]string{"add", "C:/projects/example/example.uproject"})

	if err != nil {
		t.Fatalf("Parse(add) error = %v", err)
	}
	if command.Name != "add" {
		t.Errorf("Command.Name = %q, want %q", command.Name, "add")
	}
	if command.ProjectPath != "C:/projects/example/example.uproject" {
		t.Errorf("Command.ProjectPath = %q, want project path", command.ProjectPath)
	}
}

func TestParseGenerateEngineScope(t *testing.T) {
	command, err := Parse([]string{"generate", "game", "--engine-scope=full"})
	if err != nil {
		t.Fatal(err)
	}
	if command.EngineScope != "full" {
		t.Fatalf("scope=%q", command.EngineScope)
	}
}
