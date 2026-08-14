package diagnostics

import (
	"encoding/json"
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
