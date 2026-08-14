package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/softdaddy-o/soft-ue-index/internal/cli"
	"github.com/softdaddy-o/soft-ue-index/internal/registry"
	"github.com/softdaddy-o/soft-ue-index/internal/testutil"
	"github.com/softdaddy-o/soft-ue-index/internal/unreal"
)

func TestListRendersEmptyRegistryAsJSON(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := New(Dependencies{Store: store, Output: &out})
	if err := a.Run(context.Background(), cli.Command{Name: "list", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("got %q", got)
	}
}

func TestAddGenerateListAndStatusUseOneRegisteredProject(t *testing.T) {
	env := testutil.NewFakeUE58(t)
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := New(Dependencies{Store: store, Output: &out, Discover: func(r unreal.ProjectRequest) (unreal.Project, error) {
		r.AssociationSource = unreal.MapAssociationSource{"UE_5.8": env.EngineRoot}
		return unreal.Discover(r)
	}, Generate: func(_ context.Context, p registry.Project) (registry.Project, error) {
		p.Generation.CompilationDatabase = "ready/compile_commands.json"
		return p, nil
	}})
	if err := a.Run(context.Background(), cli.Command{Name: "add", ProjectPath: env.UProject}); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background(), cli.Command{Name: "generate", ProjectName: "game"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.Run(context.Background(), cli.Command{Name: "status", ProjectName: "game", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"compilationDatabase":`) || !strings.Contains(out.String(), "compile_commands.json") {
		t.Fatalf("status %q", out.String())
	}
}

func TestRemoveUnknownProjectFails(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(Dependencies{Store: store})
	if err := a.Run(context.Background(), cli.Command{Name: "remove", ProjectName: "missing"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDoctorDefaultReturnsStructuredChecks(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := New(Dependencies{Store: store, Output: &out})
	if err := a.Run(context.Background(), cli.Command{Name: "doctor", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"checks"`) || !strings.Contains(out.String(), `"engine"`) {
		t.Fatalf("doctor %q", out.String())
	}
}
