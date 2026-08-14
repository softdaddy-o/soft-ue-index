package compdb

import (
	"os"
	"path/filepath"
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
