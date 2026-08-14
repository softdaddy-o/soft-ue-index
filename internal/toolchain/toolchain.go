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

var versionPattern = regexp.MustCompile(`^\s*(\d+)(?:\.(\d+))?(?:\.(\d+))?\s*$`)
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

// Range is an inclusive compiler version range from Windows_SDK.json.
type Range struct {
	Minimum Version
	Maximum Version
}

func (r Range) String() string { return r.Minimum.String() + "-" + r.Maximum.String() }

// Contains reports whether version is within the inclusive range.
func (r Range) Contains(version Version) bool {
	return r.Minimum.Compare(version) <= 0 && version.Compare(r.Maximum) <= 0
}

// ParseRange accepts the inclusive A-B version ranges used by PreferredClangVersions.
func ParseRange(text string) (Range, error) {
	parts := strings.Split(strings.TrimSpace(text), "-")
	if len(parts) != 2 {
		return Range{}, fmt.Errorf("invalid version range %q", text)
	}
	minimum, err := ParseVersion(parts[0])
	if err != nil {
		return Range{}, err
	}
	maximum, err := ParseVersion(parts[1])
	if err != nil {
		return Range{}, err
	}
	if minimum.Compare(maximum) > 0 {
		return Range{}, fmt.Errorf("invalid version range %q: minimum exceeds maximum", text)
	}
	return Range{Minimum: minimum, Maximum: maximum}, nil
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
	// Preferred is retained for callers of the earlier single-version configuration API.
	Preferred       Version
	PreferredRanges []Range
	Minimum         Version
}

// ParseSDKConfig reads the clang requirements from Engine/Config/Windows/Windows_SDK.json.
func ParseSDKConfig(contents []byte) (Config, error) {
	var raw struct {
		Preferred       string   `json:"PreferredClangVersion"`
		PreferredRanges []string `json:"PreferredClangVersions"`
		Minimum         string   `json:"MinimumClangVersion"`
	}
	if err := json.Unmarshal(contents, &raw); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrMalformedSDKConfig, err)
	}
	minimum, err := ParseVersion(raw.Minimum)
	if err != nil {
		return Config{}, fmt.Errorf("%w: minimum clang version: %v", ErrMalformedSDKConfig, err)
	}
	rangeTexts := raw.PreferredRanges
	if len(rangeTexts) == 0 && raw.Preferred != "" {
		rangeTexts = []string{raw.Preferred + "-" + raw.Preferred}
	}
	if len(rangeTexts) == 0 {
		return Config{}, fmt.Errorf("%w: preferred clang versions are required", ErrMalformedSDKConfig)
	}
	ranges := make([]Range, 0, len(rangeTexts))
	for _, text := range rangeTexts {
		versionRange, rangeErr := ParseRange(text)
		if rangeErr != nil {
			return Config{}, fmt.Errorf("%w: preferred clang range: %v", ErrMalformedSDKConfig, rangeErr)
		}
		if versionRange.Maximum.Compare(minimum) < 0 {
			return Config{}, fmt.Errorf("%w: preferred clang range is below minimum", ErrMalformedSDKConfig)
		}
		ranges = append(ranges, versionRange)
	}
	return Config{Preferred: ranges[0].Minimum, PreferredRanges: ranges, Minimum: minimum}, nil
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

// SelectClangd executes candidates and selects the first source candidate in the highest-priority
// engine range. A version at the minimum but outside every configured range is not compatible.
func SelectClangd(config Config, candidates []Candidate, runner Runner) (Selection, error) {
	ranges := config.PreferredRanges
	if len(ranges) == 0 && config.Preferred != (Version{}) {
		ranges = []Range{{Minimum: config.Preferred, Maximum: config.Preferred}}
	}
	selections := make([]Selection, len(ranges))
	found := make([]bool, len(ranges))
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
		for index, versionRange := range ranges {
			if !found[index] && versionRange.Contains(version) {
				selections[index] = Selection{Path: candidate.Path, Source: candidate.Source, Version: version}
				found[index] = true
			}
		}
	}
	for index := range selections {
		if found[index] {
			return selections[index], nil
		}
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

// WindowsCandidateProvider supplies conventional Windows LLVM locations without
// depending on the process environment or real filesystem in tests.
type WindowsCandidateProvider struct {
	Environment Environment
	FileSystem  FileSystem
}

func (p WindowsCandidateProvider) StandardCandidates() []Candidate {
	programFiles, ok := p.Environment.LookupEnv("ProgramFiles")
	if !ok || strings.TrimSpace(programFiles) == "" {
		return nil
	}
	return []Candidate{{Path: windowsPath(programFiles, "LLVM", "bin", "clangd.exe"), Source: SourceStandardInstall}}
}

func (p WindowsCandidateProvider) VisualStudioCandidates() []Candidate {
	if p.FileSystem == nil {
		return nil
	}
	programFiles, ok := p.Environment.LookupEnv("ProgramFiles")
	if !ok || strings.TrimSpace(programFiles) == "" {
		return nil
	}
	var candidates []Candidate
	for _, architecture := range []string{"x64", "x86"} {
		pattern := windowsPath(programFiles, "Microsoft Visual Studio", "*", "*", "VC", "Tools", "Llvm", architecture, "bin", "clangd.exe")
		paths := p.FileSystem.Glob(pattern)
		sort.Strings(paths)
		for _, path := range paths {
			if p.FileSystem.Exists(path) {
				candidates = append(candidates, Candidate{Path: path, Source: SourceVisualStudio})
			}
		}
	}
	return candidates
}

// FileSystem provides existence checks for machine-independent discovery.
type FileSystem interface {
	Exists(path string) bool
	Glob(pattern string) []string
}

// FindBundledDotnet locates the newest numeric runtime distributed with the engine.
func FindBundledDotnet(engineRoot string, filesystem FileSystem) (string, bool) {
	pattern := filepath.ToSlash(filepath.Join(engineRoot, "Engine", "Binaries", "ThirdParty", "DotNet", "*", "win-x64", "dotnet.exe"))
	paths := filesystem.Glob(pattern)
	sort.Strings(paths)
	var selected string
	var selectedVersion Version
	for _, path := range paths {
		if !filesystem.Exists(path) {
			continue
		}
		version, err := ParseVersion(filepath.Base(filepath.Dir(filepath.Dir(path))))
		if err != nil {
			continue
		}
		if selected == "" || version.Compare(selectedVersion) > 0 || (version.Compare(selectedVersion) == 0 && path < selected) {
			selected, selectedVersion = path, version
		}
	}
	return selected, selected != ""
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
	if path == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(path), "clangd.exe") {
		return path
	}
	return path + "/clangd.exe"
}

func windowsPath(elements ...string) string { return filepath.ToSlash(filepath.Join(elements...)) }
