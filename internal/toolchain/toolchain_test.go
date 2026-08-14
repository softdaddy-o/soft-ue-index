package toolchain

import (
	"errors"
	"path/filepath"
	"testing"
)

type fakeRunner struct {
	outputs map[string]string
}

func (r fakeRunner) Run(name string, args ...string) (string, error) {
	if output, ok := r.outputs[name]; ok {
		return output, nil
	}
	return "", errors.New("not executable")
}

func TestParseSDKConfigReadsPreferredAndMinimumClangVersions(t *testing.T) {
	config, err := ParseSDKConfig([]byte(`{"PreferredClangVersion":"18.1.8","MinimumClangVersion":"16.0.6"}`))
	if err != nil {
		t.Fatalf("ParseSDKConfig() error = %v", err)
	}
	if got, want := config.Preferred.String(), "18.1.8"; got != want {
		t.Errorf("Preferred = %q, want %q", got, want)
	}
	if got, want := config.Minimum.String(), "16.0.6"; got != want {
		t.Errorf("Minimum = %q, want %q", got, want)
	}
}

func TestParseSDKConfigRejectsMalformedVersion(t *testing.T) {
	_, err := ParseSDKConfig([]byte(`{"PreferredClangVersion":"eighteen","MinimumClangVersion":"16.0.6"}`))
	if !errors.Is(err, ErrMalformedSDKConfig) {
		t.Fatalf("ParseSDKConfig() error = %v, want ErrMalformedSDKConfig", err)
	}
}

func TestSelectClangdPrefersEnginePreferredVersionOverNewerCompatibleCandidate(t *testing.T) {
	selection, err := SelectClangd(Config{Preferred: mustVersion(t, "18.1.8"), Minimum: mustVersion(t, "16.0.6")}, []Candidate{
		{Path: "C:/LLVM/bin/clangd.exe", Source: SourceLLVMPath},
		{Path: "C:/Program Files/LLVM/bin/clangd.exe", Source: SourceStandardInstall},
	}, fakeRunner{outputs: map[string]string{
		"C:/LLVM/bin/clangd.exe":               "clangd version 19.0.0",
		"C:/Program Files/LLVM/bin/clangd.exe": "clangd version 18.1.8 (https://github.com/llvm/llvm-project.git)",
	}})
	if err != nil {
		t.Fatalf("SelectClangd() error = %v", err)
	}
	if got, want := selection.Path, "C:/Program Files/LLVM/bin/clangd.exe"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestSelectClangdUsesFirstCompatibleCandidateWhenPreferredIsUnavailable(t *testing.T) {
	selection, err := SelectClangd(Config{Preferred: mustVersion(t, "18.1.8"), Minimum: mustVersion(t, "16.0.6")}, []Candidate{
		{Path: "first", Source: SourceExplicit},
		{Path: "second", Source: SourcePath},
	}, fakeRunner{outputs: map[string]string{"first": "clangd version 17.0.0", "second": "clangd version 19.1.0"}})
	if err != nil {
		t.Fatalf("SelectClangd() error = %v", err)
	}
	if got, want := selection.Path, "first"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestSelectClangdReportsMissingCompatibleTool(t *testing.T) {
	_, err := SelectClangd(Config{Preferred: mustVersion(t, "18.1.8"), Minimum: mustVersion(t, "16.0.6")}, []Candidate{{Path: "old"}}, fakeRunner{outputs: map[string]string{"old": "clangd version 15.0.0"}})
	if !errors.Is(err, ErrCompatibleClangdNotFound) {
		t.Fatalf("SelectClangd() error = %v, want ErrCompatibleClangdNotFound", err)
	}
}

func TestDiscoverCandidatesUsesStableSourceOrderAndDeduplicates(t *testing.T) {
	candidates := DiscoverCandidates("C:/custom/clangd.exe", fakeEnvironment{"LLVM_PATH": "C:/llvm", "PATH": "C:/path-one;C:/llvm"}, fakeCandidates{})
	got := make([]string, len(candidates))
	for index, candidate := range candidates {
		got[index] = candidate.Path
	}
	want := []string{"C:/custom/clangd.exe", "C:/llvm/clangd.exe", "C:/path-one/clangd.exe", "C:/standard/clangd.exe", "C:/vs/clangd.exe"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("candidate %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestFindBundledDotnetChecksKnownEngineLocations(t *testing.T) {
	path, ok := FindBundledDotnet("C:/UE", fakeFiles{"C:/UE/Engine/Binaries/ThirdParty/DotNet/8.0.300/win-x64/dotnet.exe": true})
	if !ok {
		t.Fatal("FindBundledDotnet() found no runtime")
	}
	if want := "C:/UE/Engine/Binaries/ThirdParty/DotNet/8.0.300/win-x64/dotnet.exe"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestFindBundledDotnetFindsAnyBundledEightRuntime(t *testing.T) {
	path, ok := FindBundledDotnet("C:/UE", fakeFiles{"C:/UE/Engine/Binaries/ThirdParty/DotNet/8.0.401/win-x64/dotnet.exe": true})
	if !ok {
		t.Fatal("FindBundledDotnet() found no runtime")
	}
	if want := "C:/UE/Engine/Binaries/ThirdParty/DotNet/8.0.401/win-x64/dotnet.exe"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func mustVersion(t *testing.T, text string) Version {
	t.Helper()
	version, err := ParseVersion(text)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

type fakeEnvironment map[string]string

func (e fakeEnvironment) LookupEnv(name string) (string, bool) {
	value, ok := e[name]
	return value, ok
}

type fakeCandidates struct{}

func (fakeCandidates) StandardCandidates() []Candidate {
	return []Candidate{{Path: "C:/standard/clangd.exe"}}
}
func (fakeCandidates) VisualStudioCandidates() []Candidate {
	return []Candidate{{Path: "C:/vs/clangd.exe"}}
}

type fakeFiles map[string]bool

func (f fakeFiles) Exists(path string) bool { return f[path] }
func (f fakeFiles) Glob(pattern string) []string {
	var matches []string
	for path := range f {
		if matched, _ := filepath.Match(pattern, path); matched {
			matches = append(matches, path)
		}
	}
	return matches
}
