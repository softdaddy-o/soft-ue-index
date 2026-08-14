package compdb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Entry is one compile_commands.json entry.
type Entry struct {
	Directory, File, Command string
	Arguments                []string
}

// ValidationInput declares the staging output and the source roots it must cover.
type ValidationInput struct {
	StagingDir, DestinationDir, ProjectRoot, EngineRoot string
	Promote                                             func(source, destination string) error
}

// ValidationResult records coverage and a stable content fingerprint.
type ValidationResult struct {
	ProjectTranslationUnits, EngineTranslationUnits int
	Fingerprint                                     string
}

// WriteDatabase exists for synthetic fixtures and writes the standard JSON representation.
func WriteDatabase(path string, entries []Entry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ValidateAndPromote validates staged output completely before replacing the last known-good database.
func ValidateAndPromote(input ValidationInput) (ValidationResult, error) {
	staged := filepath.Join(input.StagingDir, DatabaseName)
	result, err := Validate(staged, input.ProjectRoot, input.EngineRoot)
	if err != nil {
		return ValidationResult{}, err
	}
	if err := os.MkdirAll(input.DestinationDir, 0o700); err != nil {
		return ValidationResult{}, fmt.Errorf("create destination: %w", err)
	}
	temporary := filepath.Join(input.DestinationDir, "."+DatabaseName+".new")
	if err := copyFile(staged, temporary); err != nil {
		return ValidationResult{}, fmt.Errorf("stage promotion: %w", err)
	}
	promote := input.Promote
	if promote == nil {
		promote = replaceFile
	}
	if err := promote(temporary, filepath.Join(input.DestinationDir, DatabaseName)); err != nil {
		_ = os.Remove(temporary)
		return ValidationResult{}, fmt.Errorf("promote compilation database: %w", err)
	}
	return result, nil
}

// Validate stream-decodes a database and confirms it covers both requested source roots.
func Validate(path, projectRoot, engineRoot string) (ValidationResult, error) {
	projectRoot, err := canonical(projectRoot)
	if err != nil {
		return ValidationResult{}, err
	}
	engineRoot, err = canonical(engineRoot)
	if err != nil {
		return ValidationResult{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return ValidationResult{}, fmt.Errorf("open database: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return ValidationResult{}, errors.New("compilation database must be a JSON array")
	}
	hash := sha256.New()
	result := ValidationResult{}
	for decoder.More() {
		var entry Entry
		if err := decoder.Decode(&entry); err != nil {
			return ValidationResult{}, fmt.Errorf("decode entry: %w", err)
		}
		normalized, scope, err := validateEntry(entry, projectRoot, engineRoot)
		if err != nil {
			return ValidationResult{}, err
		}
		if scope == 1 {
			result.ProjectTranslationUnits++
		} else if scope == 2 {
			result.EngineTranslationUnits++
		}
		_, _ = io.WriteString(hash, normalized+"\n")
	}
	if _, err := decoder.Token(); err != nil {
		return ValidationResult{}, fmt.Errorf("finish database: %w", err)
	}
	if result.ProjectTranslationUnits == 0 || result.EngineTranslationUnits == 0 {
		return ValidationResult{}, fmt.Errorf("insufficient coverage: project=%d engine=%d", result.ProjectTranslationUnits, result.EngineTranslationUnits)
	}
	result.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func validateEntry(entry Entry, projectRoot, engineRoot string) (string, int, error) {
	if entry.Directory == "" || entry.File == "" || (entry.Command == "" && len(entry.Arguments) == 0) {
		return "", 0, errors.New("entry requires directory, file, and command or arguments")
	}
	directory, err := canonical(entry.Directory)
	if err != nil {
		return "", 0, err
	}
	source := entry.File
	if !filepath.IsAbs(source) {
		source = filepath.Join(directory, source)
	}
	source, err = canonical(source)
	if err != nil {
		return "", 0, err
	}
	args := entry.Arguments
	if len(args) == 0 {
		args = splitCommand(entry.Command)
	}
	if len(args) == 0 {
		return "", 0, errors.New("entry has no compiler")
	}
	compiler := args[0]
	if !filepath.IsAbs(compiler) {
		compiler = filepath.Join(directory, compiler)
	}
	if _, err := os.Stat(compiler); err != nil {
		return "", 0, fmt.Errorf("compiler %q: %w", compiler, err)
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "@") {
			response := strings.Trim(arg[1:], "\"")
			if !filepath.IsAbs(response) {
				response = filepath.Join(directory, response)
			}
			if _, err := os.Stat(response); err != nil {
				return "", 0, fmt.Errorf("response file %q: %w", response, err)
			}
		}
	}
	scope := 0
	if within(source, projectRoot) {
		scope = 1
	}
	if within(source, engineRoot) {
		scope = 2
	}
	return filepath.ToSlash(source) + "\x00" + strings.Join(args, "\x00"), scope, nil
}
func canonical(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}
func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
func splitCommand(command string) []string {
	var result []string
	var current strings.Builder
	quote := rune(0)
	for _, r := range command {
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
				continue
			}
			if quote == r {
				quote = 0
				continue
			}
		}
		if (r == ' ' || r == '\t') && quote == 0 {
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}
func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
