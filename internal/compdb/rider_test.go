package compdb

import (
	"encoding/json"
	"fmt"
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
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%d", len(entries))
	}
	if !within(entries[0].File, project) || !within(entries[1].File, engine) {
		t.Fatalf("project translation units must precede lexically earlier engine paths: %#v", entries)
	}
	repeated, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "GameEditor", TargetFile: filepath.Join(project, "Source", "GameEditor.Target.cs"), StagingDir: staging, ClangCL: "C:/llvm/bin/clang-cl.exe"})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Fingerprint != result.Fingerprint {
		t.Fatalf("fingerprint changed across identical runs: %q != %q", repeated.Fingerprint, result.Fingerprint)
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

func TestRiderMetadataStaleForTracksSelectedEnginePluginDescriptor(t *testing.T) {
	root := t.TempDir()
	project, engine := filepath.Join(root, "Project"), filepath.Join(root, "Engine")
	gameDir := filepath.Join(project, "Source", "Game")
	pluginRoot := filepath.Join(engine, "Plugins", "Runtime", "EnginePlugin")
	pluginModule := filepath.Join(pluginRoot, "Source", "EnginePlugin")
	metadataDir := RiderMetadataDir(project)
	for _, dir := range []string{gameDir, pluginModule, metadataDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	targetFile := filepath.Join(project, "Source", "GameEditor.Target.cs")
	uproject := filepath.Join(project, "Game.uproject")
	descriptor := filepath.Join(pluginRoot, "EnginePlugin.uplugin")
	for _, path := range []string{targetFile, uproject, descriptor} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	metadata := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{` +
		`"Game":{"Directory":"` + filepath.ToSlash(gameDir) + `"},` +
		`"EnginePlugin":{"Directory":"` + filepath.ToSlash(pluginModule) + `"}}}`
	metadataPath := filepath.Join(metadataDir, "GameEditor.json")
	if err := os.WriteFile(metadataPath, []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	metadataTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(metadataPath, metadataTime, metadataTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(descriptor, metadataTime.Add(time.Hour), metadataTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale, err := RiderMetadataStaleFor(project, engine, "GameEditor", targetFile, false)
	if err != nil || !stale {
		t.Fatalf("selected engine plugin descriptor should stale metadata: stale=%v err=%v", stale, err)
	}
	if err := os.Chtimes(descriptor, metadataTime.Add(-time.Hour), metadataTime.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale, err = RiderMetadataStaleFor(project, engine, "GameEditor", targetFile, false)
	if err != nil || stale {
		t.Fatalf("older selected engine plugin descriptor should be fresh: stale=%v err=%v", stale, err)
	}
	if err := os.Remove(descriptor); err != nil {
		t.Fatal(err)
	}
	stale, err = RiderMetadataStaleFor(project, engine, "GameEditor", targetFile, false)
	if err != nil || !stale {
		t.Fatalf("removed selected engine plugin descriptor should stale metadata: stale=%v err=%v", stale, err)
	}
}

func TestRiderMetadataStaleForDetectsMissingAndNewProjectInputs(t *testing.T) {
	setup := func(t *testing.T) (project, engine, targetFile, rules, metadata string, metadataTime time.Time) {
		t.Helper()
		root := t.TempDir()
		project, engine = filepath.Join(root, "Project"), filepath.Join(root, "Engine")
		module := filepath.Join(project, "Source", "Game")
		metadataDir := RiderMetadataDir(project)
		for _, dir := range []string{module, engine, metadataDir} {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
		}
		targetFile, rules = filepath.Join(project, "Source", "GameEditor.Target.cs"), filepath.Join(module, "Game.Build.cs")
		for _, path := range []string{targetFile, rules, filepath.Join(project, "Game.uproject")} {
			if err := os.WriteFile(path, nil, 0644); err != nil {
				t.Fatal(err)
			}
		}
		body := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{"Game":{"Directory":"` + filepath.ToSlash(module) + `","Rules":"Game.Build.cs"}}}`
		metadata = filepath.Join(metadataDir, "GameEditor.json")
		if err := os.WriteFile(metadata, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		metadataTime = time.Now().Add(time.Hour)
		if err := os.Chtimes(metadata, metadataTime, metadataTime); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Run("missing selected target", func(t *testing.T) {
		project, engine, target, _, _, _ := setup(t)
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		stale, err := RiderMetadataStaleFor(project, engine, "GameEditor", target, false)
		if err != nil || !stale {
			t.Fatalf("stale=%v err=%v", stale, err)
		}
	})
	t.Run("missing selected rules after rename", func(t *testing.T) {
		project, engine, target, rules, _, metadataTime := setup(t)
		newRules := filepath.Join(filepath.Dir(rules), "Renamed.Build.cs")
		if err := os.Rename(rules, newRules); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newRules, metadataTime.Add(time.Hour), metadataTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		stale, err := RiderMetadataStaleFor(project, engine, "GameEditor", target, false)
		if err != nil || !stale {
			t.Fatalf("stale=%v err=%v", stale, err)
		}
	})
	t.Run("new module rules", func(t *testing.T) {
		project, engine, target, _, _, metadataTime := setup(t)
		newRules := filepath.Join(project, "Source", "NewModule", "NewModule.Build.cs")
		if err := os.MkdirAll(filepath.Dir(newRules), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(newRules, nil, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newRules, metadataTime.Add(time.Hour), metadataTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		stale, err := RiderMetadataStaleFor(project, engine, "GameEditor", target, false)
		if err != nil || !stale {
			t.Fatalf("stale=%v err=%v", stale, err)
		}
	})
	t.Run("new target rules", func(t *testing.T) {
		project, engine, target, _, _, metadataTime := setup(t)
		newTarget := filepath.Join(project, "Source", "Server.Target.cs")
		if err := os.WriteFile(newTarget, nil, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newTarget, metadataTime.Add(time.Hour), metadataTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		stale, err := RiderMetadataStaleFor(project, engine, "GameEditor", target, false)
		if err != nil || !stale {
			t.Fatalf("stale=%v err=%v", stale, err)
		}
	})
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

func TestSynthesizeRiderIncludesEveryModuleAPIDefinitionInEveryResponse(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "ProjectZ")
	engine := filepath.Join(root, "EngineA")
	metadataDir := RiderMetadataDir(project)
	projectModule := filepath.Join(project, "Source", "Game")
	engineModule := filepath.Join(engine, "Source", "Runtime", "Core")
	for _, dir := range []string{projectModule, engineModule, metadataDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	projectSource := filepath.Join(projectModule, "Game.cpp")
	engineSource := filepath.Join(engineModule, "Core.cpp")
	targetFile := filepath.Join(project, "Source", "GameEditor.Target.cs")
	for _, path := range []string{projectSource, engineSource, targetFile} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	metadata := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{` +
		`"Game":{"Directory":"` + filepath.ToSlash(projectModule) + `","Definitions":["GAME_LOCAL=1"],"ApiDefinitions":["GAME_API=__declspec(dllexport)"]},` +
		`"Core":{"Directory":"` + filepath.ToSlash(engineModule) + `","Definitions":["CORE_LOCAL=1"],"ApiDefinitions":["CORE_API=__declspec(dllimport)"]}}}`
	if err := os.WriteFile(filepath.Join(metadataDir, "GameEditor.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	responseDir := filepath.Join(root, "responses")
	if _, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "GameEditor", TargetFile: targetFile, StagingDir: filepath.Join(root, "stage"), ResponseDir: responseDir, ClangCL: "clang-cl.exe"}); err != nil {
		t.Fatal(err)
	}
	responses, err := os.ReadDir(responseDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 {
		t.Fatalf("responses=%d, want 2", len(responses))
	}
	for _, response := range responses {
		data, err := os.ReadFile(filepath.Join(responseDir, response.Name()))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "GAME_API=__declspec(dllexport)") || !strings.Contains(text, "CORE_API=__declspec(dllimport)") {
			t.Fatalf("response %s is missing dependency API definitions:\n%s", response.Name(), text)
		}
	}
}

func TestSynthesizeRiderIncludesSafeGeneratedCodeDirectory(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "Project")
	engine := filepath.Join(root, "Engine")
	projectModule := filepath.Join(project, "Source", "Game")
	engineModule := filepath.Join(engine, "Source", "Core")
	generated := filepath.Join(project, "Intermediate", "Build", "Inc", "Game", "UHT")
	metadataDir := RiderMetadataDir(project)
	for _, dir := range []string{projectModule, engineModule, generated, metadataDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	targetFile := filepath.Join(project, "Source", "GameEditor.Target.cs")
	for _, path := range []string{filepath.Join(projectModule, "Game.cpp"), filepath.Join(engineModule, "Core.cpp"), filepath.Join(generated, "Game.generated.h"), targetFile} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	metadata := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{` +
		`"Game":{"Directory":"` + filepath.ToSlash(projectModule) + `","GeneratedCodeDirectory":"` + filepath.ToSlash(generated) + `","Definitions":["GAME_LOCAL=1"]},` +
		`"Core":{"Directory":"` + filepath.ToSlash(engineModule) + `"}}}`
	if err := os.WriteFile(filepath.Join(metadataDir, "GameEditor.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	responses := filepath.Join(root, "responses")
	result, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "GameEditor", TargetFile: targetFile, StagingDir: filepath.Join(root, "stage"), ResponseDir: responses, ClangCL: "clang-cl.exe"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectTranslationUnits != 1 || result.EngineTranslationUnits != 1 {
		t.Fatalf("generated directory affected source coverage: %+v", result)
	}
	files, err := os.ReadDir(responses)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(responses, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "GAME_LOCAL=1") {
			found = strings.Contains(string(data), filepath.Clean(generated))
		}
	}
	if !found {
		t.Fatal("project module response omitted GeneratedCodeDirectory")
	}
}

func TestSynthesizeRiderRejectsEscapingGeneratedCodeDirectory(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "Project")
	engine := filepath.Join(root, "Engine")
	projectModule := filepath.Join(project, "Source", "Game")
	engineModule := filepath.Join(engine, "Source", "Core")
	outside := filepath.Join(root, "OutsideGenerated")
	metadataDir := RiderMetadataDir(project)
	for _, dir := range []string{projectModule, engineModule, outside, metadataDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	targetFile := filepath.Join(project, "Source", "GameEditor.Target.cs")
	for _, path := range []string{filepath.Join(projectModule, "Game.cpp"), filepath.Join(engineModule, "Core.cpp"), targetFile} {
		if err := os.WriteFile(path, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	metadata := `{"Name":"GameEditor","TargetFile":"` + filepath.ToSlash(targetFile) + `","Modules":{` +
		`"Game":{"Directory":"` + filepath.ToSlash(projectModule) + `","GeneratedCodeDirectory":"` + filepath.ToSlash(outside) + `"},` +
		`"Core":{"Directory":"` + filepath.ToSlash(engineModule) + `"}}}`
	if err := os.WriteFile(filepath.Join(metadataDir, "GameEditor.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "GameEditor", TargetFile: targetFile, StagingDir: filepath.Join(root, "stage"), ClangCL: "clang-cl.exe"}); err == nil {
		t.Fatal("escaping GeneratedCodeDirectory was accepted")
	}
	link := filepath.Join(project, "Intermediate", "GeneratedLink")
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Logf("symlink escape check skipped: %v", err)
		return
	}
	metadata = strings.Replace(metadata, filepath.ToSlash(outside), filepath.ToSlash(link), 1)
	if err := os.WriteFile(filepath.Join(metadataDir, "GameEditor.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := SynthesizeRider(RiderInput{ProjectRoot: project, EngineRoot: engine, Target: "GameEditor", TargetFile: targetFile, StagingDir: filepath.Join(root, "stage-link"), ClangCL: "clang-cl.exe"}); err == nil {
		t.Fatal("symlinked GeneratedCodeDirectory escape was accepted")
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

func TestRiderModuleIndexLookupScalesWithPathDepth(t *testing.T) {
	root := t.TempDir()
	modules := make([]riderResolvedModule, 10_000)
	for i := range modules {
		modules[i] = riderResolvedModule{name: fmt.Sprintf("Module%05d", i), dir: filepath.Join(root, fmt.Sprintf("Module%05d", i))}
	}
	child := riderResolvedModule{name: "Nested", dir: filepath.Join(modules[len(modules)-1].dir, "Private")}
	modules = append(modules, child)
	index := newRiderModuleIndex(modules)
	got, probes := index.lookupWithProbes(filepath.Join(child.dir, "Detail", "Thing.cpp"))
	if got == nil || got.name != "Nested" {
		t.Fatalf("deepest module = %#v", got)
	}
	if probes > 4 {
		t.Fatalf("lookup probed %d ancestors for %d modules", probes, len(modules))
	}
	if roots := riderModuleWalkRoots(modules); len(roots) != len(modules)-1 {
		t.Fatalf("nested module should share its parent walk: roots=%d modules=%d", len(roots), len(modules))
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
