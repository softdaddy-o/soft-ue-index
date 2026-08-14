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
	meta := `{"Name":"MEditor","TargetFile":"` + filepath.ToSlash(filepath.Join(project, "Source", "MEditor.Target.cs")) + `","EnvironmentIncludePaths":{"System":["C:/SDK Includes"]},"EnvironmentDefinitions":["GLOBAL=1"],"Modules":{"M":{"Directory":"` + filepath.ToSlash(filepath.Join(project, "Source", "M")) + `"},"E":{"Directory":"` + filepath.ToSlash(filepath.Join(engine, "Source", "Core")) + `"}}}`
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
	data, err := os.ReadFile(filepath.Join(responses, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "GLOBAL=1") || !strings.Contains(string(data), "SDK Includes") {
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
