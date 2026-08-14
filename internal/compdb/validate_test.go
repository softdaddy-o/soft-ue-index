package compdb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateProjectOnlyFails(t *testing.T) {
	env := newValidationEnv(t)
	writeDatabase(t, env.staging, []Entry{env.projectEntry})
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err == nil {
		t.Fatal("expected engine coverage error")
	}
	assertOldDatabase(t, env.destination)
}

func TestValidateEngineOnlyFails(t *testing.T) {
	env := newValidationEnv(t)
	writeDatabase(t, env.staging, []Entry{env.engineEntry})
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err == nil {
		t.Fatal("expected project coverage error")
	}
	assertOldDatabase(t, env.destination)
}

func TestValidateMalformedJSONPreservesOldDatabase(t *testing.T) {
	env := newValidationEnv(t)
	if err := os.WriteFile(filepath.Join(env.staging, DatabaseName), []byte("[not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err == nil {
		t.Fatal("expected malformed JSON error")
	}
	assertOldDatabase(t, env.destination)
}

func TestValidateTrailingJSONPreservesOldDatabase(t *testing.T) {
	env := newValidationEnv(t)
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	path := filepath.Join(env.staging, DatabaseName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(" trailing"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err == nil {
		t.Fatal("expected trailing JSON error")
	}
	assertOldDatabase(t, env.destination)
}

func TestValidateMissingResponseFilePreservesOldDatabase(t *testing.T) {
	env := newValidationEnv(t)
	env.engineEntry.Arguments = []string{env.compiler, "@missing.rsp", "-c", env.engineEntry.File}
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err == nil {
		t.Fatal("expected response file error")
	}
	assertOldDatabase(t, env.destination)
}

func TestValidateFullCoveragePromotesAndFingerprints(t *testing.T) {
	env := newValidationEnv(t)
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	got, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot})
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectTranslationUnits != 1 || got.EngineTranslationUnits != 1 || got.Fingerprint == "" {
		t.Fatalf("result = %#v", got)
	}
	contents, err := os.ReadFile(filepath.Join(env.destination, DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) == "old" {
		t.Fatal("database was not promoted")
	}
}

func TestFingerprintDoesNotDependOnEntryOrder(t *testing.T) {
	env := newValidationEnv(t)
	path := filepath.Join(env.staging, DatabaseName)
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	first, err := Validate(path, env.projectRoot, env.engineRoot)
	if err != nil {
		t.Fatal(err)
	}
	writeDatabase(t, env.staging, []Entry{env.engineEntry, env.projectEntry})
	second, err := Validate(path, env.projectRoot, env.engineRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprints differ: %q != %q", first.Fingerprint, second.Fingerprint)
	}
}

func TestValidateMissingSourcePreservesOldDatabase(t *testing.T) {
	env := newValidationEnv(t)
	env.projectEntry.File = filepath.Join(env.projectRoot, "Source", "Missing.cpp")
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err == nil {
		t.Fatal("expected missing source error")
	}
	assertOldDatabase(t, env.destination)
}

func TestValidateCompilerDirectoryPreservesOldDatabase(t *testing.T) {
	env := newValidationEnv(t)
	env.projectEntry.Arguments[0] = filepath.Dir(env.compiler)
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err == nil {
		t.Fatal("expected compiler regular-file error")
	}
	assertOldDatabase(t, env.destination)
}

func TestValidateAcceptsHeaderEntry(t *testing.T) {
	env := newValidationEnv(t)
	header := filepath.Join(env.projectRoot, "Source", "Game.hpp")
	if err := os.WriteFile(header, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env.projectEntry.File = header
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	if _, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot}); err != nil {
		t.Fatalf("validate header entry: %v", err)
	}
}

func TestPromotionFailurePreservesOldDatabase(t *testing.T) {
	env := newValidationEnv(t)
	writeDatabase(t, env.staging, []Entry{env.projectEntry, env.engineEntry})
	_, err := ValidateAndPromote(ValidationInput{StagingDir: env.staging, DestinationDir: env.destination, ProjectRoot: env.projectRoot, EngineRoot: env.engineRoot, Promote: func(_, _ string) error { return os.ErrPermission }})
	if err == nil {
		t.Fatal("expected promotion failure")
	}
	assertOldDatabase(t, env.destination)
}

type validationEnv struct {
	staging, destination, projectRoot, engineRoot, compiler string
	projectEntry, engineEntry                               Entry
}

func newValidationEnv(t *testing.T) validationEnv {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "Project")
	engine := filepath.Join(root, "Engine")
	staging := filepath.Join(root, "staging")
	destination := filepath.Join(root, "output")
	for _, path := range []string{project, engine, staging, destination} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	compiler := filepath.Join(root, "tool", "clang++.exe")
	projectFile := filepath.Join(project, "Source", "Game.cpp")
	engineFile := filepath.Join(engine, "Source", "Runtime", "Core.cpp")
	for _, path := range []string{compiler, projectFile, engineFile} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(destination, DatabaseName), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engine, "args.rsp"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	return validationEnv{staging, destination, project, engine, compiler, Entry{Directory: project, File: projectFile, Arguments: []string{compiler, "-c", projectFile}}, Entry{Directory: engine, File: engineFile, Command: compiler + " @args.rsp -c " + engineFile}}
}
func writeDatabase(t *testing.T, directory string, entries []Entry) {
	t.Helper()
	if err := WriteDatabase(filepath.Join(directory, DatabaseName), entries); err != nil {
		t.Fatal(err)
	}
}
func assertOldDatabase(t *testing.T, destination string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(destination, DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("old database changed: %q", got)
	}
}
