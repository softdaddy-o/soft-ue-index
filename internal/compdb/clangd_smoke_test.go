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
