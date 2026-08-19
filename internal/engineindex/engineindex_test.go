package engineindex

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
)

func TestSplitPartitionsByEngineRoot(t *testing.T) {
	engineRoot := t.TempDir()
	projectRoot := t.TempDir()
	entries := []compdb.Entry{
		{Directory: engineRoot, File: filepath.Join(engineRoot, "Engine", "Source", "Runtime", "Core", "Foo.cpp")},
		{Directory: projectRoot, File: filepath.Join(projectRoot, "Source", "Game", "Bar.cpp")},
		{Directory: engineRoot, File: filepath.Join(engineRoot, "Engine", "Plugins", "FX", "Niagara", "Baz.cpp")},
	}
	engine, rest := Split(entries, engineRoot)
	if len(engine) != 2 || len(rest) != 1 {
		t.Fatalf("engine=%d rest=%d, want 2 and 1", len(engine), len(rest))
	}
	if !strings.Contains(engine[0].File, "Core") && !strings.Contains(engine[1].File, "Core") {
		t.Fatalf("expected a Core entry in engine split: %#v", engine)
	}
	if !strings.Contains(rest[0].File, "Game") {
		t.Fatalf("expected the project entry in rest: %#v", rest)
	}
}

// TestSplitExcludesEngineRootSiblingsOutsideTheEngineSubdirectory guards
// against re-including files WriteFragment's fragment can never actually
// serve: the fragment's PathMatch is always "Engine/.*", scoped to
// <engineRoot>/Engine specifically, not the whole engine installation
// root. A real engine root has siblings of Engine/ (Templates/,
// FeaturePacks/, Samples/) that would never be reached through the mounted
// index, so Split must not stage (and clangd-indexer must not spend time
// on) files outside Engine/ even though they're under engineRoot.
func TestSplitExcludesEngineRootSiblingsOutsideTheEngineSubdirectory(t *testing.T) {
	engineRoot := t.TempDir()
	entries := []compdb.Entry{
		{Directory: engineRoot, File: filepath.Join(engineRoot, "Templates", "TP_Blank", "Direct.cpp")},
		{Directory: engineRoot, File: filepath.Join(engineRoot, "Engine", "Source", "Direct.cpp")},
	}
	engine, rest := Split(entries, engineRoot)
	if len(engine) != 1 || len(rest) != 1 {
		t.Fatalf("engine=%d rest=%d, want exactly the Engine/-nested file in engine and the Templates/ sibling in rest", len(engine), len(rest))
	}
	if !strings.Contains(engine[0].File, filepath.Join("Engine", "Source")) {
		t.Fatalf("wrong entry ended up in engine: %#v", engine)
	}
}

func TestKeyIsStableForIdenticalInputAndDiffersOnEntryChange(t *testing.T) {
	engineRoot := t.TempDir()
	a := []compdb.Entry{{File: filepath.Join(engineRoot, "Engine", "A.cpp")}}
	b := []compdb.Entry{{File: filepath.Join(engineRoot, "Engine", "B.cpp")}}

	k1, err := Key(engineRoot, a)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Key(engineRoot, a)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("Key is not stable for identical input: %q vs %q", k1, k2)
	}
	k3, err := Key(engineRoot, b)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k3 {
		t.Fatalf("Key did not change when the entry set changed")
	}
}

func TestKeyIsOrderIndependent(t *testing.T) {
	engineRoot := t.TempDir()
	a := []compdb.Entry{
		{File: filepath.Join(engineRoot, "Engine", "A.cpp")},
		{File: filepath.Join(engineRoot, "Engine", "B.cpp")},
	}
	b := []compdb.Entry{a[1], a[0]}
	k1, err := Key(engineRoot, a)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Key(engineRoot, b)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("Key depends on entry order: %q vs %q -- two identical entry sets built in different orders (e.g. from a map) must share a cache destination", k1, k2)
	}
}

func TestKeyDiffersOnEngineRoot(t *testing.T) {
	entries := []compdb.Entry{{File: "Engine/A.cpp"}}
	k1, err := Key(t.TempDir(), entries)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Key(t.TempDir(), entries)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatalf("Key did not depend on engineRoot: two different roots produced %q for both", k1)
	}
}

// TestKeyDiffersOnCompileArguments guards against two projects compiling
// the identical file set from the same engine root under different
// preprocessor state (WITH_EDITOR, UE_BUILD_SHIPPING vs DEVELOPMENT, and
// similar defines that genuinely change what's visible in Unreal's
// headers) computing the same key -- which would make them silently share
// (and overwrite) one another's prebuilt index despite indexing
// meaningfully different content. An earlier version of Key hashed only
// file paths and missed this.
func TestKeyDiffersOnCompileArguments(t *testing.T) {
	root := t.TempDir()
	editorBuild := []compdb.Entry{{File: filepath.Join(root, "Engine", "A.cpp"), Arguments: []string{"clang-cl", "-DWITH_EDITOR=1", "-DUE_BUILD_DEVELOPMENT=1"}}}
	shippingBuild := []compdb.Entry{{File: filepath.Join(root, "Engine", "A.cpp"), Arguments: []string{"clang-cl", "-DWITH_EDITOR=0", "-DUE_BUILD_SHIPPING=1"}}}
	k1, err := Key(root, editorBuild)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Key(root, shippingBuild)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatalf("Key did not depend on compile arguments: an editor build and a shipping build of the identical file produced the same key %q", k1)
	}
}

func TestKeyDiffersOnCommandField(t *testing.T) {
	root := t.TempDir()
	a := []compdb.Entry{{File: filepath.Join(root, "Engine", "A.cpp"), Command: "clang-cl -DWITH_EDITOR=1 A.cpp"}}
	b := []compdb.Entry{{File: filepath.Join(root, "Engine", "A.cpp"), Command: "clang-cl -DWITH_EDITOR=0 A.cpp"}}
	k1, err := Key(root, a)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Key(root, b)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatalf("Key did not depend on the Command field (the Arguments-less compdb.Entry shape): %q for both", k1)
	}
}

type fakeIndexerRunner struct {
	stdout  []byte
	err     error
	gotExe  string
	gotArgs []string
}

func (f *fakeIndexerRunner) Run(_ context.Context, exe string, args []string, stdout, log io.Writer) error {
	f.gotExe, f.gotArgs = exe, args
	if f.err != nil {
		_, _ = log.Write([]byte("boom"))
		return f.err
	}
	_, err := stdout.Write(f.stdout)
	return err
}

func TestBuildIndexWritesRunnerOutputToDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "engine.idx")
	logPath := filepath.Join(dir, "indexer.log")
	stagedCDB := filepath.Join(dir, "compile_commands.json")
	runner := &fakeIndexerRunner{stdout: []byte("fake-index-bytes")}

	if err := BuildIndex(context.Background(), runner, "clangd-indexer", stagedCDB, dest, logPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fake-index-bytes" {
		t.Fatalf("destination content = %q, want %q", got, "fake-index-bytes")
	}
	if runner.gotExe != "clangd-indexer" {
		t.Fatalf("exe = %q", runner.gotExe)
	}
	found := false
	for _, a := range runner.gotArgs {
		if a == stagedCDB {
			found = true
		}
	}
	if !found {
		t.Fatalf("args=%v did not include the staged CDB path %q", runner.gotArgs, stagedCDB)
	}
}

func TestBuildIndexLeavesNoPartialFileOnRunnerError(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "engine.idx")
	logPath := filepath.Join(dir, "indexer.log")
	runner := &fakeIndexerRunner{err: errors.New("boom")}

	err := BuildIndex(context.Background(), runner, "clangd-indexer", filepath.Join(dir, "compile_commands.json"), dest, logPath)
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("expected no destination file after a runner error, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(dest + ".new"); !os.IsNotExist(statErr) {
		t.Fatalf("expected the temporary file to be cleaned up, stat err=%v", statErr)
	}
}

func TestBuildIndexRejectsEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "engine.idx")
	runner := &fakeIndexerRunner{stdout: nil}

	err := BuildIndex(context.Background(), runner, "clangd-indexer", filepath.Join(dir, "compile_commands.json"), dest, filepath.Join(dir, "indexer.log"))
	if err == nil {
		t.Fatal("expected an error for empty indexer output")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("expected no destination file for empty output, stat err=%v", statErr)
	}
}

func TestBuildIndexDoesNotClobberAnExistingGoodIndexOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "engine.idx")
	if err := os.WriteFile(dest, []byte("previous-good-index"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeIndexerRunner{err: errors.New("boom")}

	if err := BuildIndex(context.Background(), runner, "clangd-indexer", filepath.Join(dir, "compile_commands.json"), dest, filepath.Join(dir, "indexer.log")); err == nil {
		t.Fatal("expected an error")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous-good-index" {
		t.Fatalf("a failed rebuild clobbered the previous good index: got %q", got)
	}
}

// TestBuildIndexCleansUpTheTempFileWhenRenameFails guards the case that
// matters most in practice: destIdxPath is exactly the file a running
// clangd session has open via Index.External.File once this feature is in
// use, so "re-run index-engine to refresh" (this package's own documented
// workflow) routinely hits a rename failure on Windows -- not a rare edge
// case. An earlier version of BuildIndex cleaned up the temp file on every
// failure EXCEPT this one, silently leaving a full-size orphan (the same
// size as the real index, since the run itself succeeded) behind on every
// refresh attempt made while a session was open.
func TestBuildIndexCleansUpTheTempFileWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "engine.idx")
	// A directory in place of the destination makes the final os.Rename
	// fail deterministically and portably (unlike holding a file open,
	// which behaves differently across OSes) while still exercising the
	// exact same code path a locked destination would.
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeIndexerRunner{stdout: []byte("fake-index-bytes")}

	err := BuildIndex(context.Background(), runner, "clangd-indexer", filepath.Join(dir, "compile_commands.json"), dest, filepath.Join(dir, "indexer.log"))
	if err == nil {
		t.Fatal("expected an error: destIdxPath is a directory, rename must fail")
	}
	if _, statErr := os.Stat(dest + ".new"); !os.IsNotExist(statErr) {
		t.Fatalf("temp file was not cleaned up after a rename failure, stat err=%v", statErr)
	}
}

func TestFindIndexerPrefersPathNextToClangd(t *testing.T) {
	dir := t.TempDir()
	clangd := filepath.Join(dir, "clangd.exe")
	indexer := filepath.Join(dir, "clangd-indexer.exe")
	if err := os.WriteFile(clangd, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexer, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := FindIndexer(clangd)
	if err != nil {
		t.Fatal(err)
	}
	if found != indexer {
		t.Fatalf("found=%q, want %q", found, indexer)
	}
}

func TestFindIndexerErrorsWithActionableMessageWhenMissing(t *testing.T) {
	dir := t.TempDir()
	clangd := filepath.Join(dir, "clangd.exe")
	_, err := FindIndexer(clangd)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "clangd-indexer") {
		t.Fatalf("error %q does not name the missing binary", err)
	}
}

func TestWriteFragmentThenReadBackMatchesExpectedShape(t *testing.T) {
	engineRoot := t.TempDir()
	idx := filepath.Join(t.TempDir(), "engine.idx")
	if err := WriteFragment(engineRoot, idx, "abc123", "game"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(FragmentPath(engineRoot))
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)
	if !strings.HasPrefix(content, fragmentMarker+" abc123 owner=game\n") {
		t.Fatalf("fragment does not start with the marker+key+owner line: %q", content)
	}
	if !strings.Contains(content, "PathMatch: \"Engine/.*\"") {
		t.Fatalf("fragment missing relative PathMatch (must be relative for a project-local fragment, not absolute): %q", content)
	}
	if !strings.Contains(content, "File: '"+idx+"'") {
		t.Fatalf("fragment missing the single-quoted absolute index file path: %q", content)
	}
	if !strings.Contains(content, "MountPoint: Engine") {
		t.Fatalf("fragment missing relative MountPoint: %q", content)
	}
}

func TestWriteFragmentQuotesAHashInTheIndexPathRatherThanTruncatingIt(t *testing.T) {
	// An unquoted YAML plain scalar treats '#' as starting a comment --
	// silently truncating the File: value at that point, so clangd would
	// mount a path that doesn't exist (or worse, a different one).
	engineRoot := t.TempDir()
	idx := filepath.Join(t.TempDir(), "cache#1", "engine.idx")
	if err := WriteFragment(engineRoot, idx, "key", "game"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(FragmentPath(engineRoot))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "File: '"+idx+"'") {
		t.Fatalf("index path containing '#' was not preserved intact: %q", got)
	}
}

func TestWriteFragmentIsIdempotentForTheSameOwnerAndKey(t *testing.T) {
	engineRoot := t.TempDir()
	idx := filepath.Join(t.TempDir(), "engine.idx")
	if err := WriteFragment(engineRoot, idx, "samekey", "game"); err != nil {
		t.Fatal(err)
	}
	if err := WriteFragment(engineRoot, idx, "samekey", "game"); err != nil {
		t.Fatalf("re-running with the same owner and key should succeed (regenerate), got: %v", err)
	}
}

// TestWriteFragmentAllowsTheSameProjectToRefreshWithADifferentKey guards
// the documented refresh workflow (re-run index-engine after an engine
// hotfix or plugin update -- the entry set, and so Key, legitimately
// changes every time). An earlier version of WriteFragment compared only
// the content key and refused this unconditionally, which meant the
// documented refresh instructions never actually worked for their own
// project.
func TestWriteFragmentAllowsTheSameProjectToRefreshWithADifferentKey(t *testing.T) {
	engineRoot := t.TempDir()
	idx := filepath.Join(t.TempDir(), "engine.idx")
	if err := WriteFragment(engineRoot, idx, "key-before-hotfix", "game"); err != nil {
		t.Fatal(err)
	}
	if err := WriteFragment(engineRoot, idx, "key-after-hotfix", "game"); err != nil {
		t.Fatalf("the same project refreshing its own fragment with a new key must be allowed, got: %v", err)
	}
	got, readErr := os.ReadFile(FragmentPath(engineRoot))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.HasPrefix(string(got), fragmentMarker+" key-after-hotfix owner=game\n") {
		t.Fatalf("fragment was not updated to the refreshed key: %q", got)
	}
}

func TestWriteFragmentRefusesToRepointAtADifferentProjectsDifferentKey(t *testing.T) {
	engineRoot := t.TempDir()
	idx := filepath.Join(t.TempDir(), "engine.idx")
	if err := WriteFragment(engineRoot, idx, "project-a-key", "project-a"); err != nil {
		t.Fatal(err)
	}
	err := WriteFragment(engineRoot, idx, "project-b-key", "project-b")
	if err == nil {
		t.Fatal("expected an error when a different project's key would repoint the shared fragment")
	}
	if !strings.Contains(err.Error(), "project-a-key") || !strings.Contains(err.Error(), "project-b-key") || !strings.Contains(err.Error(), "project-a") || !strings.Contains(err.Error(), "project-b") {
		t.Fatalf("error should name both the existing and the new owner/key so the conflict is diagnosable: %v", err)
	}
	// The original fragment must be untouched by the refused write.
	got, readErr := os.ReadFile(FragmentPath(engineRoot))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.HasPrefix(string(got), fragmentMarker+" project-a-key owner=project-a\n") {
		t.Fatalf("a refused write must not modify the existing fragment: %q", got)
	}
}

// TestWriteFragmentAllowsADifferentProjectWithTheSameKey covers the
// harmless case: two different projects that happen to pull in an
// identical Engine/Plugins entry set from the same engine root compute the
// same key, so opportunistic sharing is still allowed even though the
// owner differs.
func TestWriteFragmentAllowsADifferentProjectWithTheSameKey(t *testing.T) {
	engineRoot := t.TempDir()
	idx := filepath.Join(t.TempDir(), "engine.idx")
	if err := WriteFragment(engineRoot, idx, "shared-key", "project-a"); err != nil {
		t.Fatal(err)
	}
	if err := WriteFragment(engineRoot, idx, "shared-key", "project-b"); err != nil {
		t.Fatalf("a different project computing the identical key should be allowed to share, got: %v", err)
	}
}

func TestWriteFragmentRefusesToOverwriteAHandAuthoredFile(t *testing.T) {
	engineRoot := t.TempDir()
	if err := os.WriteFile(FragmentPath(engineRoot), []byte("CompileFlags:\n  Add: [-Wall]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteFragment(engineRoot, filepath.Join(t.TempDir(), "engine.idx"), "anykey", "game")
	if err == nil {
		t.Fatal("expected an error for a hand-authored .clangd with no marker")
	}
	got, readErr := os.ReadFile(FragmentPath(engineRoot))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(got), "CompileFlags") {
		t.Fatalf("a refused write must not modify the hand-authored file: %q", got)
	}
}
