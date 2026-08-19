// Package engineindex builds and manages an offline, prebuilt clangd index
// for a project's Engine/Plugins portion of its compilation database,
// separate from the project's own live --background-index.
//
// See GitHub issue #7: roughly 95% of a typical Unreal project's
// compile_commands.json entries are Engine/Plugins code that changes far
// less often than the project's own source, and every clangd session
// currently re-derives the same semantic state from scratch for all of it,
// independently, with no reuse across sessions or projects sharing an
// engine install beyond the on-disk shard cache clangd itself keeps.
// clangd-indexer (a separate, offline batch indexer bundled with clangd's
// indexing tools, distinct from the interactive --background-index path)
// can build that state once; clangd's own Index.External.File + PathMatch
// config can then mount it read-only for the matched subtree, which
// implicitly disables live background-indexing for those files.
package engineindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/softdaddy-o/soft-ue-index/internal/compdb"
)

// ErrNoEngineEntries is returned when a project's compilation database has
// no entries under its engine root -- nothing to prebuild an index for.
var ErrNoEngineEntries = errors.New("no engine-scoped entries in the compilation database")

// Split partitions entries by whether their source file resolves under
// engineRoot's "Engine" subdirectory -- not engineRoot itself. This
// deliberately matches WriteFragment's fragment, which only ever scopes
// PathMatch to "Engine/.*": an engine installation root can have siblings
// of Engine/ (Templates/, FeaturePacks/, Samples/) that would never be
// reached through the mounted index, so including them here would build
// (and pay clangd-indexer time for) entries the fragment can never serve.
//
// Split and Key both assume engineRoot and each entry's file are already
// canonicalized (symlinks/junctions resolved) -- true for every current
// caller (unreal.normalizePath resolves the registered engine root;
// SynthesizeRider canonicalizes every entry file it writes) but not
// verified here. A junctioned engine root reaching this function
// unresolved would silently split every entry into rest instead of engine,
// or compute a Key that never matches a canonicalized re-run of the same
// engine -- Split returning zero engine entries for a database Validate
// already certified as covering the engine is the visible symptom.
//
// An entry whose file can't be made absolute is dropped from both results
// rather than failing the whole split -- generate's own Validate has
// already vetted the source database, so this is defensive, not the
// primary correctness check.
func Split(entries []compdb.Entry, engineRoot string) (engine, rest []compdb.Entry) {
	root, err := filepath.Abs(filepath.Join(engineRoot, "Engine"))
	if err != nil {
		return nil, entries
	}
	for _, e := range entries {
		abs, absErr := filepath.Abs(e.File)
		if absErr != nil {
			continue
		}
		if underRoot(abs, root) {
			engine = append(engine, e)
		} else {
			rest = append(rest, e)
		}
	}
	return engine, rest
}

func underRoot(abs, root string) bool {
	rel, err := filepath.Rel(root, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Key computes a stable identifier for engineRoot plus the exact set of
// engine-scoped source files being indexed AND the compiler arguments each
// is built with, so two projects that pull in different Engine/Plugins
// module sets -- or the same modules built with different preprocessor
// state (WITH_EDITOR, UE_BUILD_SHIPPING/DEVELOPMENT, and similar defines
// that genuinely change what's visible in Unreal's headers) -- from the
// same engine install get isolated cache entries instead of one silently
// overwriting the other's index. Hashing only file paths would let two
// projects compiling identical files under different configurations
// collide on the same key despite indexing meaningfully different content.
//
// See WriteFragment's conflict check, which depends on this: a
// project-local .clangd fragment mounts exactly one index file for the
// whole engine root, so if a second project's index build were written to
// the same destination, the first project would silently start reading an
// index that may be missing modules -- or built under a configuration --
// only it needs. Isolating by content forces distinct destinations
// instead, at the cost of no automatic sharing when the two projects'
// engine module sets or build configuration merely overlap rather than
// match exactly -- opportunistic sharing (a bonus for identical inputs)
// over guaranteed-but-unsafe sharing.
func Key(engineRoot string, entries []compdb.Entry) (string, error) {
	root, err := filepath.Abs(engineRoot)
	if err != nil {
		return "", err
	}
	type keyed struct{ file, args string }
	files := make([]keyed, 0, len(entries))
	for _, e := range entries {
		abs, absErr := filepath.Abs(e.File)
		if absErr != nil {
			return "", absErr
		}
		args := e.Command
		if len(e.Arguments) > 0 {
			args = strings.Join(e.Arguments, "\x00")
		}
		files = append(files, keyed{file: strings.ToLower(filepath.ToSlash(abs)), args: args})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].file != files[j].file {
			return files[i].file < files[j].file
		}
		return files[i].args < files[j].args
	})
	h := sha256.New()
	_, _ = io.WriteString(h, strings.ToLower(filepath.ToSlash(root)))
	h.Write([]byte{0})
	for _, f := range files {
		_, _ = io.WriteString(h, f.file)
		h.Write([]byte{0})
		_, _ = io.WriteString(h, f.args)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Runner executes clangd-indexer, streaming its stdout (the binary index,
// potentially hundreds of megabytes for a real engine) directly to a
// destination writer. This is deliberately not compdb.Runner: that
// interface's ExecRunner captures both streams into a bounded in-memory
// buffer (64KB default) for diagnostic logs, which would silently truncate
// a real index into a corrupt file if reused here.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdout, log io.Writer) error
}

// ExecRunner runs clangd-indexer as a real subprocess.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, exe string, args []string, stdout, log io.Writer) error {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdout = stdout
	cmd.Stderr = log
	return cmd.Run()
}

// BuildIndex runs clangd-indexer against a staged, engine-only compilation
// database (stagedCDBPath, the compile_commands.json file itself -- matches
// clangd-indexer's own documented invocation, "clangd-indexer
// --executor=all-TUs compile_commands.json") and writes the resulting
// binary index to destIdxPath, atomically (a temporary file, renamed only
// once the run has produced non-empty output). The full stderr log is kept
// at logPath: clangd-indexer logs one "Failed to run action on <file>" line
// per translation unit it can't process (platform-mismatched files in a
// mixed database, for example), which is expected and non-fatal as long as
// enough of the database indexes successfully to produce output.
func BuildIndex(ctx context.Context, runner Runner, indexerPath, stagedCDBPath, destIdxPath, logPath string) (err error) {
	if runner == nil {
		return errors.New("indexer runner is required")
	}
	if err := os.MkdirAll(filepath.Dir(destIdxPath), 0o700); err != nil {
		return fmt.Errorf("create index destination: %w", err)
	}
	tmp := destIdxPath + ".new"
	// Every return path below goes through this: the temp file is cleaned
	// up on any failure, including a failed rename, not just a failed run.
	// An earlier version left tmp behind specifically on rename failure --
	// which is not a rare edge case here: destIdxPath is the file a live
	// clangd session has open via Index.External.File (the entire point of
	// this feature), so the ordinary "re-run to refresh" workflow hits
	// exactly this path on Windows whenever the project's own clangd
	// session is running, leaving a same-sized-as-the-real-index orphan
	// behind every time.
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create index staging file: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		out.Close()
		return fmt.Errorf("open indexer log: %w", err)
	}
	runErr := runner.Run(ctx, indexerPath, []string{"--executor=all-TUs", stagedCDBPath}, out, logFile)
	closeErr := out.Close()
	logFile.Close()
	if runErr != nil {
		return fmt.Errorf("clangd-indexer: %w (see %s)", runErr, logPath)
	}
	if closeErr != nil {
		return fmt.Errorf("write index: %w", closeErr)
	}
	info, statErr := os.Stat(tmp)
	if statErr != nil || info.Size() == 0 {
		return fmt.Errorf("clangd-indexer produced an empty index (check %s)", logPath)
	}
	if err := os.Rename(tmp, destIdxPath); err != nil {
		return fmt.Errorf("move index into place: %w (if a clangd session for this project is currently running, it likely has the previous %s open -- close it and retry)", err, destIdxPath)
	}
	return nil
}

// FindIndexer locates clangd-indexer, which is not part of the standard
// LLVM release clangd itself ships in -- it's distributed separately (the
// "clangd indexing tools" archive). Looked for next to the resolved clangd
// binary first (where a from-source or full LLVM build puts it), then on
// PATH.
func FindIndexer(clangdPath string) (string, error) {
	name := "clangd-indexer"
	if filepath.Ext(clangdPath) != "" {
		name += filepath.Ext(clangdPath)
	}
	if clangdPath != "" {
		candidate := filepath.Join(filepath.Dir(clangdPath), name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("clangd-indexer not found next to %q or on PATH -- it ships separately from clangd itself in LLVM's \"clangd indexing tools\" archive, not the main LLVM release", clangdPath)
}
