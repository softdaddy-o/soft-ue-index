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

func TestParseDaemonCommands(t *testing.T) {
	for _, action := range []string{"run", "status", "stop"} {
		command, err := Parse([]string{"daemon", action})
		if err != nil {
			t.Fatalf("daemon %s: %v", action, err)
		}
		if command.Name != "daemon" || command.DaemonAction != action {
			t.Fatalf("daemon %s parsed as %+v", action, command)
		}
	}
	command, err := Parse([]string{"daemon", "run", "--child"})
	if err != nil || !command.Child {
		t.Fatalf("daemon child parsed as %+v, %v", command, err)
	}
}

func TestParseRejectsInvalidDaemonCommand(t *testing.T) {
	if _, err := Parse([]string{"daemon", "restart"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("invalid daemon error=%v", err)
	}
}
