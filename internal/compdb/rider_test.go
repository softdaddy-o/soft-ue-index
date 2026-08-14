package compdb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSynthesizeRiderSelectsExactTargetNotUE5Decoy(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "Project")
	engine := filepath.Join(root, "Engine")
	for _, p := range []string{filepath.Join(project, "Source", "Game"), filepath.Join(engine, "Source", "Runtime", "Core"), filepath.Join(project, "Intermediate", "ProjectFiles", ".Rider", "Win64", "Development", "Editor")} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(project, "Source", "Game", "Game.cpp"), "")
	write(filepath.Join(engine, "Source", "Runtime", "Core", "Core.cpp"), "")
	write(filepath.Join(project, "Source", "Game", "Game.Build.cs"), "")
	write(filepath.Join(project, "Source", "GameEditor.Target.cs"), "")
	meta := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(filepath.Join(project, "Source", "GameEditor.Target.cs")) + `","ToolchainInfo":{"CompilerPath":"C:/llvm/bin/clang-cl.exe"},"Modules":{"Game":{"Directory":"` + filepath.ToSlash(filepath.Join(project, "Source", "Game")) + `","Rules":{"PublicIncludePaths":["Public"],"Definitions":["GAME=1"]}},"Core":{"Directory":"` + filepath.ToSlash(filepath.Join(engine, "Source", "Runtime", "Core")) + `","Rules":{"Definitions":["CORE=1"]}}}}`
	write(filepath.Join(project, "Intermediate", "ProjectFiles", ".Rider", "Win64", "Development", "Editor", "GameEditor.json"), meta)
	write(filepath.Join(project, "Intermediate", "ProjectFiles", ".Rider", "Win64", "Development", "Editor", "UE5.json"), `{"Name":"UE5","Modules":{}}`)
	staging := filepath.Join(root, "stage")
	if err := os.MkdirAll(staging, 0755); err != nil {
		t.Fatal(err)
	}
	result, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "GameEditor", TargetFile: filepath.Join(project, "Source", "GameEditor.Target.cs"), StagingDir: staging, ClangCL: "C:/llvm/bin/clang-cl.exe"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectTranslationUnits != 1 || result.EngineTranslationUnits != 1 {
		t.Fatalf("coverage=%+v", result)
	}
	data, err := os.ReadFile(filepath.Join(staging, DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty db")
	}
}

func TestRiderMetadataStaleWhenRulesNewer(t *testing.T) {
	root := t.TempDir()
	dir := RiderMetadataDir(root)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(dir, "GameEditor.json")
	if err := os.WriteFile(metadata, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(metadata, old, old); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(root, "Source", "Game.Build.cs")
	if err := os.MkdirAll(filepath.Dir(rules), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rules, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if !RiderMetadataStale(root) {
		t.Fatal("expected newer build rules to mark metadata stale")
	}
}

func TestRiderMetadataStaleForUsesOnlySelectedTargetMetadata(t *testing.T) {
	root := t.TempDir()
	project, engine := filepath.Join(root, "P"), filepath.Join(root, "E")
	dir := RiderMetadataDir(project)
	for _, path := range []string{filepath.Join(project, "Source", "Game"), filepath.Join(project, "Source", "Other"), engine, dir} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	targetFile := filepath.Join(project, "Source", "GameEditor.Target.cs")
	rules := filepath.Join(project, "Source", "Game", "Game.Build.cs")
	uproject := filepath.Join(project, "P.uproject")
	for _, path := range []string{targetFile, rules, uproject, filepath.Join(project, "Source", "Other", "Other.Build.cs")} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	meta := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{"Game":{"Directory":"` + filepath.ToSlash(filepath.Dir(rules)) + `","Rules":"Game.Build.cs"}}}`
	selected := filepath.Join(dir, "GameEditor.json")
	if err := os.WriteFile(selected, []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Unrelated.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	newerThanSources := time.Now().Add(time.Hour)
	if err := os.Chtimes(selected, newerThanSources, newerThanSources); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "Unrelated.json"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale, err := RiderMetadataStaleFor(project, engine, "GameEditor", targetFile, false)
	if err != nil || stale {
		t.Fatalf("selected metadata should be fresh: stale=%v err=%v", stale, err)
	}
	if err := os.Chtimes(rules, time.Now().Add(2*time.Hour), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale, err = RiderMetadataStaleFor(project, engine, "GameEditor", targetFile, false)
	if err != nil || !stale {
		t.Fatalf("selected rules should stale metadata: stale=%v err=%v", stale, err)
	}
}

func TestRiderMetadataStaleForFullScopeIncludesUnrealEditorOnlyWhenRequested(t *testing.T) {
	root := t.TempDir()
	project, engine := filepath.Join(root, "P"), filepath.Join(root, "E")
	dir := RiderMetadataDir(project)
	for _, path := range []string{filepath.Join(project, "Source", "Game"), filepath.Join(engine, "Source", "Core"), dir} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	projectTarget := filepath.Join(project, "Source", "GameEditor.Target.cs")
	engineTarget := filepath.Join(engine, "Source", "UnrealEditor.Target.cs")
	engineRules := filepath.Join(engine, "Source", "Core", "Core.Build.cs")
	for _, path := range []string{projectTarget, engineTarget, engineRules, filepath.Join(project, "P.uproject")} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	projectMeta := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(projectTarget) + `","Modules":{"Game":{"Directory":"` + filepath.ToSlash(filepath.Join(project, "Source", "Game")) + `"}}}`
	engineMeta := `{"Name":"UnrealEditor","TargetFile":"` + filepath.ToSlash(engineTarget) + `","Modules":{"Core":{"Directory":"` + filepath.ToSlash(filepath.Join(engine, "Source", "Core")) + `","Rules":"Core.Build.cs"}}}`
	for name, meta := range map[string]string{"project.json": projectMeta, "engine.json": engineMeta} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(meta), 0644); err != nil {
			t.Fatal(err)
		}
	}
	fresh := time.Now().Add(time.Hour)
	for _, path := range []string{filepath.Join(dir, "project.json"), filepath.Join(dir, "engine.json")} {
		if err := os.Chtimes(path, fresh, fresh); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(engineRules, time.Now().Add(2*time.Hour), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale, err := RiderMetadataStaleFor(project, engine, "GameEditor", projectTarget, false)
	if err != nil || stale {
		t.Fatalf("project-only should ignore engine metadata: stale=%v err=%v", stale, err)
	}
	stale, err = RiderMetadataStaleFor(project, engine, "GameEditor", projectTarget, true)
	if err != nil || !stale {
		t.Fatalf("full scope should use UnrealEditor rules: stale=%v err=%v", stale, err)
	}
}

func TestRiderMetadataStaleForRejectsMalformedAndAmbiguousTargets(t *testing.T) {
	root := t.TempDir()
	project, engine := filepath.Join(root, "P"), filepath.Join(root, "E")
	dir := RiderMetadataDir(project)
	if err := os.MkdirAll(filepath.Join(project, "Source"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(engine, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(project, "Source", "GameEditor.Target.cs")
	if err := os.WriteFile(targetFile, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := RiderMetadataStaleFor(project, engine, "GameEditor", targetFile, false); err == nil || !strings.Contains(err.Error(), "rider_metadata_missing") {
		t.Fatalf("malformed metadata should fail safely: %v", err)
	}
	meta := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{}}`
	for _, name := range []string{"one.json", "two.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(meta), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RiderMetadataStaleFor(project, engine, "GameEditor", targetFile, false); err == nil || !strings.Contains(err.Error(), "rider_metadata_ambiguous") {
		t.Fatalf("ambiguous metadata should fail safely: %v", err)
	}
}

func TestSynthesizeRiderPersistsContentAddressedResponsesAndRootEnvironment(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "P")
	engine := filepath.Join(root, "E")
	dir := RiderMetadataDir(project)
	for _, p := range []string{filepath.Join(project, "Source", "M"), filepath.Join(engine, "Source", "Core"), dir} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(project, "Source", "M", "M.cpp"), filepath.Join(engine, "Source", "Core", "E.cpp"), filepath.Join(project, "Source", "MEditor.Target.cs")} {
		if err := os.WriteFile(p, []byte{}, 0644); err != nil {
			t.Fatal(err)
		}
	}
	meta := `{"Name":"MEditor","TargetFile":"` + filepath.ToSlash(filepath.Join(project, "Source", "MEditor.Target.cs")) + `","EnvironmentIncludePaths":{"System":["C:/SDK Includes"]},"EnvironmentDefinitions":["GLOBAL=1"],"Modules":{"M":{"Directory":"` + filepath.ToSlash(filepath.Join(project, "Source", "M")) + `","Rules":"M.Build.cs","PrivateIncludePaths":["Private"],"PublicSystemIncludePaths":["Public System Only"],"ApiDefinitions":["UE_MODULE_NAME=M"],"ForceIncludeFiles":["PCH.h"]},"E":{"Directory":"` + filepath.ToSlash(filepath.Join(engine, "Source", "Core")) + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "target.json"), []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	responses := filepath.Join(root, "responses")
	if _, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "MEditor", StagingDir: stage, ResponseDir: responses, ClangCL: "clang-cl.exe"}); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(responses)
	if err != nil || len(files) == 0 {
		t.Fatal("response not persisted")
	}
	var data string
	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(responses, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		data += string(b)
	}
	if !strings.Contains(data, "GLOBAL=1") || !strings.Contains(data, "SDK Includes") || !strings.Contains(data, "Public System Only") || !strings.Contains(data, "UE_MODULE_NAME=M") || !strings.Contains(data, "/FI") {
		t.Fatalf("root env absent: %s", data)
	}
}

func TestStringsFromJSONNestedObjectsAreDeterministic(t *testing.T) {
	raw := json.RawMessage(`{"z":{"second":"B","first":"A"},"a":["C","D"]}`)
	want := strings.Join(stringsFromJSON(raw), ",")
	for range 50 {
		if got := strings.Join(stringsFromJSON(raw), ","); got != want {
			t.Fatalf("nondeterministic: %q != %q", got, want)
		}
	}
	if want != "C,D,A,B" {
		t.Fatalf("order=%q", want)
	}
}

func TestSynthesizeRiderFullScopeRequiresSafeUnrealEditorMetadata(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "P")
	engine := filepath.Join(root, "E")
	dir := RiderMetadataDir(project)
	for _, p := range []string{filepath.Join(project, "Source", "M"), filepath.Join(engine, "Source", "Extra"), dir} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(project, "Source", "M", "M.cpp"), filepath.Join(engine, "Source", "Extra", "X.cpp"), filepath.Join(project, "Source", "MEditor.Target.cs")} {
		if err := os.WriteFile(p, []byte{}, 0644); err != nil {
			t.Fatal(err)
		}
	}
	projectMeta := `{"Name":"MEditor","TargetFile":"` + filepath.ToSlash(filepath.Join(project, "Source", "MEditor.Target.cs")) + `","Modules":{"M":{"Directory":"` + filepath.ToSlash(filepath.Join(project, "Source", "M")) + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte(projectMeta), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "MEditor", StagingDir: filepath.Join(root, "stage"), ClangCL: "clang-cl", EngineScopeFull: true}); err == nil {
		t.Fatal("expected missing full metadata error")
	}
	engineTarget := filepath.Join(engine, "Source", "UnrealEditor.Target.cs")
	if err := os.WriteFile(engineTarget, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	full := `{"Name":"UnrealEditor","TargetFile":"` + filepath.ToSlash(engineTarget) + `","Modules":{"Extra":{"Directory":"` + filepath.ToSlash(filepath.Join(engine, "Source", "Extra")) + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "engine.json"), []byte(full), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "MEditor", StagingDir: filepath.Join(root, "stage2"), ClangCL: "clang-cl", EngineScopeFull: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.EngineTranslationUnits != 1 {
		t.Fatalf("engine=%d", r.EngineTranslationUnits)
	}
}
