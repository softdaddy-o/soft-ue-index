package compdb

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteDatabaseLoadsInClangd(t *testing.T) {
	clangd := findClangdForTest()
	if clangd == "" {
		t.Skip("clangd is not available on PATH")
	}
	compiler, err := exec.LookPath("clang++")
	if err != nil {
		compiler = filepath.Join(filepath.Dir(clangd), "clang++")
		if _, statErr := os.Stat(compiler); statErr != nil {
			compiler += ".exe"
			if _, statErr = os.Stat(compiler); statErr != nil {
				t.Skip("clang++ is not available beside clangd")
			}
		}
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.cpp")
	header := filepath.Join(dir, "value.h")
	if err := os.WriteFile(header, []byte("constexpr int value = 7;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("#include \"value.h\"\nint main() { return value; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteDatabase(filepath.Join(dir, DatabaseName), []Entry{{Directory: dir, File: source, Arguments: []string{compiler, "-std=c++20", "-c", source}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, clangd, "--check="+source, "--compile-commands-dir="+dir, "--log=verbose").CombinedOutput()
	if err != nil {
		t.Fatalf("clangd check failed: %v\n%s", err, output)
	}
	log := string(output)
	if !strings.Contains(log, "Compile command from CDB") || strings.Contains(log, "Failed to find compilation database") || strings.Contains(log, "Generic fallback command") {
		t.Fatalf("clangd did not load the generated compilation database:\n%s", log)
	}
}

func TestRiderGeneratedCodeDirectoryResolvesGeneratedHeaderInClangd(t *testing.T) {
	clangd := findClangdForTest()
	if clangd == "" {
		t.Skip("clangd is not available")
	}
	clangCL := filepath.Join(filepath.Dir(clangd), "clang-cl.exe")
	if info, err := os.Stat(clangCL); err != nil || !info.Mode().IsRegular() {
		t.Skip("clang-cl is not available beside clangd")
	}
	root := t.TempDir()
	project, engine := filepath.Join(root, "Project"), filepath.Join(root, "Engine")
	gameDir, coreDir := filepath.Join(project, "Source", "Game"), filepath.Join(engine, "Source", "Core")
	generated := filepath.Join(project, "Intermediate", "Build", "Inc", "Game", "UHT")
	metadataDir := RiderMetadataDir(project)
	for _, dir := range []string{gameDir, coreDir, generated, metadataDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	gameSource := filepath.Join(gameDir, "Game.cpp")
	coreSource := filepath.Join(coreDir, "Core.cpp")
	targetFile := filepath.Join(project, "Source", "GameEditor.Target.cs")
	files := map[string]string{
		gameSource: "#include \"Game.generated.h\"\nint game() { return GENERATED_VALUE; }\n",
		coreSource: "int core() { return 0; }\n",
		filepath.Join(generated, "Game.generated.h"): "#pragma once\n#define GENERATED_VALUE 7\n",
		targetFile: "",
	}
	for path, contents := range files {
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	metadata := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{` +
		`"Game":{"Directory":"` + filepath.ToSlash(gameDir) + `","GeneratedCodeDirectory":"` + filepath.ToSlash(generated) + `"},` +
		`"Core":{"Directory":"` + filepath.ToSlash(coreDir) + `"}}}`
	if err := os.WriteFile(filepath.Join(metadataDir, "GameEditor.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	if _, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "GameEditor", TargetFile: targetFile, StagingDir: stage, ClangCL: clangCL}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, clangd, "--check="+gameSource, "--compile-commands-dir="+stage, "--log=verbose").CombinedOutput()
	if err != nil {
		t.Fatalf("clangd generated-header check failed: %v\n%s", err, output)
	}
	log := string(output)
	if !strings.Contains(log, "Compile command from CDB") || !strings.Contains(log, "All checks completed, 0 errors") {
		t.Fatalf("clangd did not resolve the generated header:\n%s", log)
	}
}

func findClangdForTest() string {
	if path, err := exec.LookPath("clangd"); err == nil {
		return path
	}
	for _, root := range []string{os.Getenv("LLVM_PATH"), filepath.Join(os.Getenv("ProgramFiles"), "LLVM")} {
		if root == "" {
			continue
		}
		for _, path := range []string{filepath.Join(root, "clangd.exe"), filepath.Join(root, "bin", "clangd.exe")} {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				return path
			}
		}
	}
	return ""
}
