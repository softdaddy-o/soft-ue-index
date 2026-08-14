// Package diagnostics reports prerequisite health in human-readable and machine-readable forms.
package diagnostics

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Status is the outcome of a diagnostic result.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
)

// Result is a stable diagnostic result suitable for command-line and JSON consumers.
type Result struct {
	Status      Status `json:"status"`
	Code        string `json:"code"`
	Summary     string `json:"summary"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation"`
}

// Report contains all doctor results in deterministic order.
type Report struct {
	Checks []Result `json:"checks"`
}

// Probe supplies independently discovered prerequisites. Keeping collection outside this
// package makes checks deterministic and safe to test without the local machine.
type Probe struct {
	Engine, UBT, Dotnet, LLVM, MSVC, WindowsSDK, GeneratedHeaders, ResponseFiles bool
}

// Check converts tool discovery facts into complete, stable doctor results.
func Check(probe Probe) Report {
	return Report{Checks: []Result{
		result(probe.Engine, "engine", "Unreal Engine", "The Unreal Engine installation was found.", "No supported Unreal Engine installation was found.", "Install or select a supported Unreal Engine installation."),
		result(probe.UBT, "ubt", "UnrealBuildTool", "UnrealBuildTool is available.", "UnrealBuildTool.dll is missing.", "Build the engine or repair the Unreal Engine installation."),
		result(probe.Dotnet, "dotnet", "Bundled .NET", "The bundled .NET runtime is available.", "The bundled .NET runtime is missing.", "Repair the Unreal Engine installation so its bundled .NET runtime is present."),
		result(probe.LLVM, "llvm", "LLVM / clangd", "A compatible clangd was found.", "No compatible clangd was found.", "Install a clangd version supported by this engine or configure its location."),
		result(probe.MSVC, "msvc", "MSVC", "The MSVC toolchain is available.", "The MSVC toolchain is missing.", "Install the Visual Studio C++ build tools required by the engine."),
		result(probe.WindowsSDK, "windows-sdk", "Windows SDK", "The Windows SDK is available.", "The Windows SDK is missing.", "Install a Windows SDK version supported by the engine."),
		result(probe.GeneratedHeaders, "generated-headers", "Generated headers", "Generated headers are available.", "Generated headers are missing.", "Generate project files and build the editor target."),
		result(probe.ResponseFiles, "response-files", "Response files", "Compile response files are available.", "Compile response files are missing.", "Generate project files and build the editor target."),
	}}
}

func result(ok bool, code, summary, successDetail, failureDetail, remediation string) Result {
	if ok {
		return Result{Status: Pass, Code: code, Summary: summary, Detail: successDetail}
	}
	return Result{Status: Fail, Code: code, Summary: summary, Detail: failureDetail, Remediation: remediation}
}

// RenderHuman returns one stable line per check. It renders the same Report as RenderJSON.
func RenderHuman(report Report) string {
	lines := make([]string, 0, len(report.Checks))
	for _, check := range report.Checks {
		line := fmt.Sprintf("%s %s: %s — %s", strings.ToUpper(string(check.Status)), check.Code, check.Summary, check.Detail)
		if check.Remediation != "" {
			line += " Remediation: " + check.Remediation
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// RenderJSON encodes the exact Report used by RenderHuman.
func RenderJSON(report Report) ([]byte, error) { return json.Marshal(report) }
