// Package diagnostics reports prerequisite health in human-readable and machine-readable forms.
package diagnostics

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
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

// Environment and FileSystem make Windows prerequisite checks independent of the real host.
type Environment interface{ LookupEnv(string) (string, bool) }
type FileSystem interface {
	Exists(path string) bool
	Glob(pattern string) []string
}

// WindowsHostProbe fills the Windows compiler and SDK fields of an existing Probe.
type WindowsHostProbe struct {
	Environment Environment
	FileSystem  FileSystem
}

func (p WindowsHostProbe) Apply(probe Probe) Probe {
	if p.Environment == nil || p.FileSystem == nil {
		return probe
	}
	for _, root := range p.programFilesRoots() {
		if anyExisting(p.FileSystem, windowsPath(root, "Microsoft Visual Studio", "*", "*", "VC", "Tools", "MSVC", "*", "bin", "Hostx64", "x64", "cl.exe")) {
			probe.MSVC = true
		}
		if anyExisting(p.FileSystem, windowsPath(root, "Windows Kits", "10", "Include", "*", "um", "windows.h")) {
			probe.WindowsSDK = true
		}
	}
	return probe
}

func (p WindowsHostProbe) programFilesRoots() []string {
	roots := []string{}
	for _, name := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if value, ok := p.Environment.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			roots = append(roots, value)
		}
	}
	return roots
}

func anyExisting(filesystem FileSystem, pattern string) bool {
	for _, path := range filesystem.Glob(pattern) {
		if filesystem.Exists(path) {
			return true
		}
	}
	return false
}
func windowsPath(elements ...string) string { return filepath.ToSlash(filepath.Join(elements...)) }

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

// WithProjectFailures annotates failed checks with stable project labels. It
// deliberately accepts labels rather than paths so reports remain share-safe.
func WithProjectFailures(report Report, failures map[string][]string) Report {
	for i := range report.Checks {
		labels := failures[report.Checks[i].Code]
		if len(labels) == 0 {
			continue
		}
		sort.Strings(labels)
		report.Checks[i].Detail += " Failing projects: " + strings.Join(labels, ", ") + "."
	}
	return report
}
