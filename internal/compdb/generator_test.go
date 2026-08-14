package compdb

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildGenerateCommand(t *testing.T) {
	root := t.TempDir()
	ubt := filepath.Join(root, "Engine", "UnrealBuildTool.dll")
	got := BuildCommand(Input{
		DotNet:    filepath.Join(root, "dotnet.exe"),
		UBTDLL:    ubt,
		UProject:  filepath.Join(root, "Game.uproject"),
		Target:    "GameEditor",
		OutputDir: filepath.Join(root, "staging"),
	})
	if got.Executable != filepath.Join(root, "dotnet.exe") {
		t.Fatalf("executable = %q", got.Executable)
	}
	want := []string{ubt, "-Mode=GenerateClangDatabase", "GameEditor", "Win64", "Development", "-Compiler=Clang", "-NoExecCodeGenActions"}
	assertContainsInOrder(t, got.Args, want...)
	assertContainsInOrder(t, got.Args, "-Project="+filepath.Join(root, "Game.uproject"), "-OutputDir="+filepath.Join(root, "staging"), "-OutputFilename=compile_commands.json")
}

func TestGenerateUsesStagingAndPrivateLog(t *testing.T) {
	root := t.TempDir()
	stagingRoot := filepath.Join(root, "staging")
	if err := os.Mkdir(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "logs", "generation.log")
	if err := os.Mkdir(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := fakeRunner{run: func(command Command, log io.Writer) {
		if command.Executable != "dotnet.exe" {
			t.Fatalf("executable = %q", command.Executable)
		}
		var outputDir string
		for _, argument := range command.Args {
			if len(argument) > len("-OutputDir=") && argument[:len("-OutputDir=")] == "-OutputDir=" {
				outputDir = argument[len("-OutputDir="):]
			}
		}
		if outputDir == "" || filepath.Dir(outputDir) != stagingRoot {
			t.Fatalf("staging output = %q", outputDir)
		}
		if _, err := io.WriteString(log, "complete output"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outputDir, DatabaseName), []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	staging, err := Generate(context.Background(), runner, Input{DotNet: "dotnet.exe"}, stagingRoot, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(staging) != stagingRoot {
		t.Fatalf("staging = %q", staging)
	}
	log, err := os.ReadFile(logPath)
	if err != nil || string(log) != "complete output" {
		t.Fatalf("log = %q, %v", log, err)
	}
}

type fakeRunner struct{ run func(Command, io.Writer) }

func (r fakeRunner) Run(_ context.Context, command Command, log io.Writer) (string, string, error) {
	r.run(command, log)
	return "", "", nil
}

func assertContainsInOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	index := 0
	for _, value := range got {
		if index < len(want) && value == want[index] {
			index++
		}
	}
	if index != len(want) {
		t.Fatalf("arguments %q do not contain %q in order", got, want)
	}
}
