package diagnostics

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckReportsEveryRequiredToolchainComponent(t *testing.T) {
	report := Check(Probe{Engine: true, UBT: true, Dotnet: true, LLVM: true, MSVC: true, WindowsSDK: true, GeneratedHeaders: true, ResponseFiles: true})
	if got, want := len(report.Checks), 8; got != want {
		t.Fatalf("len(Checks) = %d, want %d", got, want)
	}
	for _, check := range report.Checks {
		if check.Status != Pass {
			t.Errorf("%s status = %q, want pass", check.Code, check.Status)
		}
		if strings.HasSuffix(check.Code, ".missing") {
			t.Errorf("pass code = %q, want neutral stable identifier", check.Code)
		}
	}
}

func TestCheckProvidesStableMissingToolDetailsAndRemediation(t *testing.T) {
	report := Check(Probe{})
	check := find(t, report, "dotnet")
	if check.Status != Fail || check.Summary == "" || check.Detail == "" || check.Remediation == "" {
		t.Errorf("missing dotnet check = %#v, want failed check with summary/detail/remediation", check)
	}
}

func TestHumanAndJSONRenderSameCheckData(t *testing.T) {
	report := Check(Probe{Engine: true, UBT: false, Dotnet: true, LLVM: false, MSVC: true, WindowsSDK: false, GeneratedHeaders: true, ResponseFiles: false})
	human := RenderHuman(report)
	encoded, err := RenderJSON(report)
	if err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("JSON = %s; unmarshal error = %v", encoded, err)
	}
	if len(decoded.Checks) != len(report.Checks) {
		t.Fatalf("JSON checks = %d, want %d", len(decoded.Checks), len(report.Checks))
	}
	for _, check := range report.Checks {
		jsonCheck := find(t, decoded, check.Code)
		if jsonCheck != check {
			t.Errorf("JSON check %q = %#v, want %#v", check.Code, jsonCheck, check)
		}
		if !strings.Contains(human, check.Code) || !strings.Contains(human, check.Summary) {
			t.Errorf("human output missing %q: %s", check.Code, human)
		}
	}
}

func TestWindowsHostProbeWiresMSVCAndWindowsSDKIntoProbe(t *testing.T) {
	probe := WindowsHostProbe{Environment: fakeEnvironment{"ProgramFiles": "C:/Program Files (x86)"}, FileSystem: fakeFiles{
		"C:/Program Files (x86)/Microsoft Visual Studio/2022/BuildTools/VC/Tools/MSVC/14.38/bin/Hostx64/x64/cl.exe": true,
		"C:/Program Files (x86)/Windows Kits/10/Include/10.0.22621.0/um/windows.h":                                  true,
	}}.Apply(Probe{Engine: true})
	if !probe.Engine || !probe.MSVC || !probe.WindowsSDK {
		t.Errorf("Probe = %#v, want preserved engine plus detected MSVC and Windows SDK", probe)
	}
}

func TestProjectFailuresAppearInSharedJSONAndHumanReport(t *testing.T) {
	report := WithProjectFailures(Check(Probe{}), map[string][]string{"engine": {"broken (Broken)", "alpha (Alpha)"}})
	human := RenderHuman(report)
	jsonData, err := RenderJSON(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha (Alpha)", "broken (Broken)"} {
		if !strings.Contains(human, want) || !strings.Contains(string(jsonData), want) {
			t.Fatalf("missing %q: human=%q json=%s", want, human, jsonData)
		}
	}
	if strings.Index(human, "alpha") > strings.Index(human, "broken") {
		t.Fatal("project labels are not deterministic")
	}
}

func find(t *testing.T, report Report, code string) Result {
	t.Helper()
	for _, check := range report.Checks {
		if check.Code == code {
			return check
		}
	}
	t.Fatalf("check %q not found", code)
	return Result{}
}

type fakeEnvironment map[string]string

func (e fakeEnvironment) LookupEnv(name string) (string, bool) {
	value, ok := e[name]
	return value, ok
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
