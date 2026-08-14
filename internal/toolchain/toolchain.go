// Package toolchain discovers the compiler tools required by an Unreal Engine installation.
package toolchain

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// ErrMalformedSDKConfig indicates that Windows_SDK.json did not contain usable clang versions.
	ErrMalformedSDKConfig = errors.New("malformed Windows SDK configuration")
	// ErrCompatibleClangdNotFound indicates that no inspected clangd satisfied the engine minimum version.
	ErrCompatibleClangdNotFound = errors.New("compatible clangd not found")
)

var versionPattern = regexp.MustCompile(`^\s*(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:[-+][0-9A-Za-z.-]+)?\s*$`)
var clangdVersionPattern = regexp.MustCompile(`(?i)\bclangd\s+version\s+([0-9]+(?:\.[0-9]+){0,2}(?:[-+][0-9A-Za-z.-]+)?)`)

// Version is a three-part compiler version. Pre-release suffixes are intentionally ignored.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v Version) Compare(other Version) int {
	if v.Major != other.Major {
		if v.Major < other.Major {
			return -1
		}
		return 1
	}
	if v.Minor != other.Minor {
		if v.Minor < other.Minor {
			return -1
		}
		return 1
	}
	if v.Patch != other.Patch {
		if v.Patch < other.Patch {
			return -1
		}
		return 1
	}
	return 0
}

// ParseVersion accepts the numeric version forms used by UE configuration and clangd output.
func ParseVersion(text string) (Version, error) {
	matches := versionPattern.FindStringSubmatch(text)
	if matches == nil {
		return Version{}, fmt.Errorf("invalid version %q", text)
	}
	parts := [3]int{}
	for index := 1; index <= 3; index++ {
		if matches[index] != "" {
			value, err := strconv.Atoi(matches[index])
			if err != nil {
				return Version{}, err
			}
			parts[index-1] = value
		}
	}
	return Version{Major: parts[0], Minor: parts[1], Patch: parts[2]}, nil
}

// Config contains the clang versions requested by the engine's Windows_SDK.json.
type Config struct {
	Preferred Version
	Minimum   Version
}

// ParseSDKConfig reads the clang requirements from Engine/Config/Windows/Windows_SDK.json.
func ParseSDKConfig(contents []byte) (Config, error) {
	var raw struct {
		Preferred string `json:"PreferredClangVersion"`
		Minimum   string `json:"MinimumClangVersion"`
	}
	if err := json.Unmarshal(contents, &raw); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrMalformedSDKConfig, err)
	}
	preferred, err := ParseVersion(raw.Preferred)
	if err != nil {
		return Config{}, fmt.Errorf("%w: preferred clang version: %v", ErrMalformedSDKConfig, err)
	}
	minimum, err := ParseVersion(raw.Minimum)
	if err != nil {
		return Config{}, fmt.Errorf("%w: minimum clang version: %v", ErrMalformedSDKConfig, err)
	}
	if preferred.Compare(minimum) < 0 {
		return Config{}, fmt.Errorf("%w: preferred clang version is below minimum", ErrMalformedSDKConfig)
	}
	return Config{Preferred: preferred, Minimum: minimum}, nil
}

// Source describes where a clangd executable was discovered.
type Source string

const (
	SourceExplicit        Source = "explicit"
	SourceLLVMPath        Source = "LLVM_PATH"
	SourcePath            Source = "PATH"
	SourceStandardInstall Source = "standard LLVM"
	SourceVisualStudio    Source = "Visual Studio LLVM"
)

// Candidate is a deterministic clangd discovery result.
type Candidate struct {
	Path   string
	Source Source
}

// Runner executes external commands. It is injectable so discovery works in tests without host tools.
type Runner interface {
	Run(name string, args ...string) (string, error)
}

// Selection is the compatible clangd selected for this engine.
type Selection struct {
	Path    string
	Source  Source
	Version Version
}

// SelectClangd executes each candidate and selects the engine-preferred version when present.
// If it is absent, the first candidate at or above the engine minimum wins.
func SelectClangd(config Config, candidates []Candidate, runner Runner) (Selection, error) {
	compatible := make([]Selection, 0, len(candidates))
	for _, candidate := range candidates {
		output, err := runner.Run(candidate.Path, "--version")
		if err != nil {
			continue
		}
		matches := clangdVersionPattern.FindStringSubmatch(output)
		if matches == nil {
			continue
		}
		version, err := ParseVersion(matches[1])
		if err != nil || version.Compare(config.Minimum) < 0 {
			continue
		}
		selection := Selection{Path: candidate.Path, Source: candidate.Source, Version: version}
		if version.Compare(config.Preferred) == 0 {
			return selection, nil
		}
		compatible = append(compatible, selection)
	}
	if len(compatible) != 0 {
		return compatible[0], nil
	}
	return Selection{}, ErrCompatibleClangdNotFound
}

// Environment provides the process environment without coupling discovery to the real host.
type Environment interface{ LookupEnv(string) (string, bool) }

// CandidateProvider supplies platform-specific conventional candidates after explicit and environment values.
type CandidateProvider interface {
	StandardCandidates() []Candidate
	VisualStudioCandidates() []Candidate
}

// FileSystem provides existence checks for machine-independent discovery.
type FileSystem interface {
	Exists(path string) bool
	Glob(pattern string) []string
}

// FindBundledDotnet locates the runtime distributed with supported UE 5.8 installations.
func FindBundledDotnet(engineRoot string, filesystem FileSystem) (string, bool) {
	pattern := filepath.ToSlash(filepath.Join(engineRoot, "Engine", "Binaries", "ThirdParty", "DotNet", "8.0.*", "win-x64", "dotnet.exe"))
	paths := filesystem.Glob(pattern)
	sort.Strings(paths)
	for _, path := range paths {
		if filesystem.Exists(path) {
			return path, true
		}
	}
	return "", false
}

// DiscoverCandidates returns candidates in stable source precedence order, removing duplicate paths.
func DiscoverCandidates(explicit string, env Environment, provider CandidateProvider) []Candidate {
	candidates := make([]Candidate, 0)
	add := func(path string, source Source) {
		path = strings.TrimSpace(path)
		if path != "" {
			candidates = append(candidates, Candidate{Path: path, Source: source})
		}
	}
	add(explicit, SourceExplicit)
	if value, ok := env.LookupEnv("LLVM_PATH"); ok {
		add(joinClangd(value), SourceLLVMPath)
	}
	if value, ok := env.LookupEnv("PATH"); ok {
		for _, directory := range strings.Split(value, ";") {
			add(joinClangd(directory), SourcePath)
		}
	}
	for _, candidate := range provider.StandardCandidates() {
		add(candidate.Path, SourceStandardInstall)
	}
	for _, candidate := range provider.VisualStudioCandidates() {
		add(candidate.Path, SourceVisualStudio)
	}
	seen := map[string]bool{}
	result := candidates[:0]
	for _, candidate := range candidates {
		key := strings.ToLower(candidate.Path)
		if !seen[key] {
			seen[key] = true
			result = append(result, candidate)
		}
	}
	return result
}

func joinClangd(path string) string {
	path = strings.TrimRight(strings.TrimSpace(path), "/\\")
	if strings.HasSuffix(strings.ToLower(path), "clangd.exe") {
		return path
	}
	return path + "/clangd.exe"
}
