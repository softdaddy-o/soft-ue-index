package unreal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/softdaddy-o/soft-ue-index/internal/testutil"
)

func TestDiscoverUE58Project(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	got, err := Discover(ProjectRequest{UProject: env.UProject, AssociationSource: MapAssociationSource{"UE_5.8": env.EngineRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version.Major != 5 || got.Version.Minor != 8 {
		t.Fatalf("got %#v", got.Version)
	}
	if got.EditorTarget != "GameEditor" {
		t.Fatalf("got %q", got.EditorTarget)
	}
	if got.Engine.Root != env.EngineRoot {
		t.Fatalf("got %q", got.Engine.Root)
	}
}

func TestDiscoverExplicitEngineOverridesAssociation(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	got, err := Discover(ProjectRequest{UProject: env.UProject, EngineRoot: env.EngineRoot, AssociationSource: MapAssociationSource{"UE_5.8": filepath.Join(env.Root, "missing")}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine.Root != env.EngineRoot {
		t.Fatalf("got %q", got.Engine.Root)
	}
}

func TestDiscoverRejectsMissingAssociation(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	_, err := Discover(ProjectRequest{UProject: env.UProject, AssociationSource: MapAssociationSource{}})
	if !errors.Is(err, ErrAssociationNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDiscoverRejectsMissingEngineFiles(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	if err := os.Remove(filepath.Join(env.EngineRoot, "Engine", "Binaries", "DotNET", "UnrealBuildTool", "UnrealBuildTool.dll")); err != nil {
		t.Fatal(err)
	}
	_, err := Discover(ProjectRequest{UProject: env.UProject, EngineRoot: env.EngineRoot})
	if !errors.Is(err, ErrUnrealBuildToolNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDiscoverRejectsUnsupportedVersion(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	testutil.WriteFile(t, filepath.Join(env.EngineRoot, "Engine", "Build", "Build.version"), `{"MajorVersion":5,"MinorVersion":7}`)
	_, err := Discover(ProjectRequest{UProject: env.UProject, EngineRoot: env.EngineRoot})
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestDiscoverRejectsMalformedJSON(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	testutil.WriteFile(t, env.UProject, "{")
	_, err := Discover(ProjectRequest{UProject: env.UProject, EngineRoot: env.EngineRoot})
	if !errors.Is(err, ErrMalformedProject) {
		t.Fatalf("got %v", err)
	}
}

func TestDiscoverRejectsMissingEditorTarget(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	if err := os.Remove(filepath.Join(env.Root, "Game", "Source", "GameEditor.Target.cs")); err != nil {
		t.Fatal(err)
	}
	_, err := Discover(ProjectRequest{UProject: env.UProject, EngineRoot: env.EngineRoot})
	if !errors.Is(err, ErrEditorTargetNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDiscoverRejectsAmbiguousEditorTarget(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	if err := os.Remove(filepath.Join(env.Root, "Game", "Source", "GameEditor.Target.cs")); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(env.Root, "Game", "Source", "AlphaEditor.Target.cs"), "")
	testutil.WriteFile(t, filepath.Join(env.Root, "Game", "Source", "BetaEditor.Target.cs"), "")
	_, err := Discover(ProjectRequest{UProject: env.UProject, EngineRoot: env.EngineRoot})
	if !errors.Is(err, ErrAmbiguousEditorTarget) {
		t.Fatalf("got %v", err)
	}
}

func TestDiscoverAcceptsForwardSlashInput(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	project := strings.ReplaceAll(env.UProject, `\`, "/")
	engine := strings.ReplaceAll(env.EngineRoot, `\`, "/")
	if _, err := Discover(ProjectRequest{UProject: project, EngineRoot: engine}); err != nil {
		t.Fatal(err)
	}
}
