package proc

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestCommandHidesWindowOnWindows(t *testing.T) {
	for name, cmd := range map[string]interface{ flags() uint32 }{
		"Command":        flagsOf{Command("x")},
		"CommandContext": flagsOf{CommandContext(context.Background(), "x")},
	} {
		got := cmd.flags()
		want := uint32(0)
		if runtime.GOOS == "windows" {
			want = createNoWindow
		}
		if got&want != want {
			t.Errorf("%s: flags %#x missing %#x", name, got, want)
		}
	}
}

// TestNoDirectExec fails when any non-test file creates a child process
// without going through this package, so a new call site cannot silently
// reintroduce console windows.
func TestNoDirectExec(t *testing.T) {
	root := filepath.Join("..", "..")
	re := regexp.MustCompile(`\bexec\.Command(Context)?\(`)
	// daemon_windows.go builds DETACHED_PROCESS|CREATE_NO_WINDOW itself.
	allowed := map[string]bool{filepath.ToSlash(filepath.Join("internal", "daemon", "daemon_windows.go")): true}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "proc" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[filepath.ToSlash(rel)] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if re.Match(b) {
			t.Errorf("%s calls os/exec directly; use internal/proc", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
