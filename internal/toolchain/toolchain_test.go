package toolchain

import (
	"errors"
	"path/filepath"
	"strings"
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

func TestParseSDKConfigPreservesPreferredClangRangePriority(t *testing.T) {
	config, err := ParseSDKConfig([]byte(`{"PreferredClangVersions":["20.1.8-20.999","20.1.0-20.1.7","19.1.0-19.999"],"MinimumClangVersion":"19.1.0"}`))
	if err != nil {
		t.Fatalf("ParseSDKConfig() error = %v", err)
	}
	want := []string{"20.1.8-20.999.0", "20.1.0-20.1.7", "19.1.0-19.999.0"}
	if len(config.PreferredRanges) != len(want) {
		t.Fatalf("ranges = %#v, want %d ranges", config.PreferredRanges, len(want))
	}
	for index, value := range want {
		if got := config.PreferredRanges[index].String(); got != value {
			t.Errorf("range %d = %q, want %q", index, got, value)
		}
	}
}

func TestParseRangeIncludesBothBoundaries(t *testing.T) {
	rangeValue, err := ParseRange("20.1.0-20.1.7")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"20.1.0", "20.1.7"} {
		if !rangeValue.Contains(mustVersion(t, version)) {
			t.Errorf("range should include %s", version)
		}
	}
	if rangeValue.Contains(mustVersion(t, "20.1.8")) {
		t.Error("range should exclude 20.1.8")
	}
}

func TestParseSDKConfigRejectsMalformedVersion(t *testing.T) {
	_, err := ParseSDKConfig([]byte(`{"PreferredClangVersion":"eighteen","MinimumClangVersion":"16.0.6"}`))
	if !errors.Is(err, ErrMalformedSDKConfig) {
		t.Fatalf("ParseSDKConfig() error = %v, want ErrMalformedSDKConfig", err)
	}
}

func TestParseSDKConfigRejectsPrereleaseVersion(t *testing.T) {
	_, err := ParseSDKConfig([]byte(`{"PreferredClangVersion":"20.1.8-rc1","MinimumClangVersion":"19.1.0"}`))
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

func TestSelectClangdUsesEngineRangePriorityBeforeCandidateDiscoveryOrder(t *testing.T) {
	config := Config{Minimum: mustVersion(t, "19.1.0"), PreferredRanges: []Range{
		mustRange(t, "20.1.8-20.999"), mustRange(t, "20.1.0-20.1.7"), mustRange(t, "19.1.0-19.999"),
	}}
	selection, err := SelectClangd(config, []Candidate{{Path: "nineteen", Source: SourceExplicit}, {Path: "twenty", Source: SourcePath}}, fakeRunner{outputs: map[string]string{"nineteen": "clangd version 19.1.0", "twenty": "clangd version 20.1.2"}})
	if err != nil {
		t.Fatalf("SelectClangd() error = %v", err)
	}
	if got, want := selection.Path, "twenty"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestSelectClangdRejectsVersionAboveConfiguredPreferredRanges(t *testing.T) {
	config := Config{Minimum: mustVersion(t, "19.1.0"), PreferredRanges: []Range{mustRange(t, "19.1.0-19.999")}}
	_, err := SelectClangd(config, []Candidate{{Path: "too-new"}}, fakeRunner{outputs: map[string]string{"too-new": "clangd version 21.0.0"}})
	if !errors.Is(err, ErrCompatibleClangdNotFound) {
		t.Fatalf("SelectClangd() error = %v, want ErrCompatibleClangdNotFound", err)
	}
}

func TestSelectClangdRejectsPrereleaseOutput(t *testing.T) {
	config := Config{Minimum: mustVersion(t, "20.1.0"), PreferredRanges: []Range{mustRange(t, "20.1.0-20.999")}}
	_, err := SelectClangd(config, []Candidate{{Path: "prerelease"}}, fakeRunner{outputs: map[string]string{"prerelease": "clangd version 20.1.8-rc1"}})
	if !errors.Is(err, ErrCompatibleClangdNotFound) {
		t.Fatalf("SelectClangd() error = %v, want ErrCompatibleClangdNotFound", err)
	}
}

func TestSelectClangdUsesFirstCompatibleCandidateWhenPreferredIsUnavailable(t *testing.T) {
	selection, err := SelectClangd(Config{Minimum: mustVersion(t, "16.0.6"), PreferredRanges: []Range{mustRange(t, "16.0.6-19.999")}}, []Candidate{
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

func TestDiscoverCandidatesSkipsEmptyEnvironmentSegments(t *testing.T) {
	candidates := DiscoverCandidates("", fakeEnvironment{"LLVM_PATH": "", "PATH": ";;C:/llvm;;"}, fakeCandidates{})
	for _, candidate := range candidates {
		if strings.EqualFold(candidate.Path, "clangd.exe") || strings.HasPrefix(candidate.Path, "/clangd.exe") {
			t.Errorf("candidate = %q, want no current-directory clangd candidate", candidate.Path)
		}
	}
	if got, want := candidates[0].Path, "C:/llvm/clangd.exe"; got != want {
		t.Errorf("first candidate = %q, want %q", got, want)
	}
}

func TestWindowsCandidateProviderDiscoversStandardAndVisualStudioLLVM(t *testing.T) {
	filesystem := fakeFiles{
		"C:/Program Files/Microsoft Visual Studio/2022/Community/VC/Tools/Llvm/x64/bin/clangd.exe":  true,
		"C:/Program Files/Microsoft Visual Studio/2022/BuildTools/VC/Tools/Llvm/x86/bin/clangd.exe": true,
	}
	provider := WindowsCandidateProvider{Environment: fakeEnvironment{"ProgramFiles": "C:/Program Files"}, FileSystem: filesystem}
	candidates := DiscoverCandidates("", fakeEnvironment{}, provider)
	want := []string{
		"C:/Program Files/LLVM/bin/clangd.exe",
		"C:/Program Files/Microsoft Visual Studio/2022/Community/VC/Tools/Llvm/x64/bin/clangd.exe",
		"C:/Program Files/Microsoft Visual Studio/2022/BuildTools/VC/Tools/Llvm/x86/bin/clangd.exe",
	}
	if len(candidates) != len(want) {
		t.Fatalf("candidates = %#v, want %d", candidates, len(want))
	}
	for index, path := range want {
		if candidates[index].Path != path {
			t.Errorf("candidate %d = %q, want %q", index, candidates[index].Path, path)
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

func TestFindBundledDotnetSupportsTenRuntimeAndChoosesNewestNumericVersion(t *testing.T) {
	path, ok := FindBundledDotnet("C:/UE", fakeFiles{
		"C:/UE/Engine/Binaries/ThirdParty/DotNet/8.0.401/win-x64/dotnet.exe": true,
		"C:/UE/Engine/Binaries/ThirdParty/DotNet/10.0.9/win-x64/dotnet.exe":  true,
		"C:/UE/Engine/Binaries/ThirdParty/DotNet/10.0.10/win-x64/dotnet.exe": true,
	})
	if !ok {
		t.Fatal("FindBundledDotnet() found no runtime")
	}
	if want := "C:/UE/Engine/Binaries/ThirdParty/DotNet/10.0.10/win-x64/dotnet.exe"; path != want {
		t.Errorf("path = %q, want newest numeric runtime %q", path, want)
	}
}

func TestFindBundledDotnetSkipsNonNumericRuntimeDirectory(t *testing.T) {
	path, ok := FindBundledDotnet("C:/UE", fakeFiles{
		"C:/UE/Engine/Binaries/ThirdParty/DotNet/10.0.10-preview/win-x64/dotnet.exe": true,
		"C:/UE/Engine/Binaries/ThirdParty/DotNet/10.0.9/win-x64/dotnet.exe":          true,
	})
	if !ok {
		t.Fatal("FindBundledDotnet() found no runtime")
	}
	if want := "C:/UE/Engine/Binaries/ThirdParty/DotNet/10.0.9/win-x64/dotnet.exe"; path != want {
		t.Errorf("path = %q, want numeric runtime %q", path, want)
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

func mustRange(t *testing.T, text string) Range {
	t.Helper()
	rangeValue, err := ParseRange(text)
	if err != nil {
		t.Fatal(err)
	}
	return rangeValue
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
