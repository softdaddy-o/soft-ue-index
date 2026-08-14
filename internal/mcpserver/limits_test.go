package mcpserver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/softdaddy-o/soft-ue-index/internal/registry"
)

func TestDefaultSemanticRequestTimeoutMatchesLSP(t *testing.T) {
	if got := (Limits{}).normalized().Timeout; got != 30*time.Second {
		t.Fatalf("default MCP timeout=%v, want 30s", got)
	}
}

func TestSearchSymbolsContextSurvivesOldBoundaryUntilConfiguredTimeout(t *testing.T) {
	root := t.TempDir()
	const oldBoundary = 10 * time.Millisecond
	const configuredTimeout = 30 * time.Millisecond
	q := &slowQueries{wait: time.Second}
	s := New(Dependencies{
		Projects: fakeProjects{projects: []registry.Project{{ID: "alpha", UProject: filepath.Join(root, "A.uproject")}}},
		Queries:  q,
		Limits:   Limits{Timeout: configuredTimeout},
	})
	started := time.Now()
	_, err := s.SearchSymbols(context.Background(), SearchSymbolsInput{ProjectID: "alpha", Query: "Actor"})
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted deadline: %v", err)
	}
	if elapsed <= oldBoundary || elapsed > 200*time.Millisecond {
		t.Fatalf("elapsed=%v, want past %v and near %v", elapsed, oldBoundary, configuredTimeout)
	}
}
