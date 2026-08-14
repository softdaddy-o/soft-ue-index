package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/softdaddy-o/soft-ue-index/internal/lsp"
	"github.com/softdaddy-o/soft-ue-index/internal/registry"
)

type fakeProjects struct{ projects []registry.Project }

func (f fakeProjects) Load(context.Context) (registry.Registry, error) {
	return registry.Registry{Version: registry.CurrentVersion, Projects: f.projects}, nil
}

func TestReadSymbolSourceRejectsOutsideRootAndBoundsBytes(t *testing.T) {
	root := t.TempDir()
	projectFile := filepath.Join(root, "Alpha.uproject")
	if err := os.WriteFile(projectFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "Source.cpp")
	if err := os.WriteFile(source, []byte("line one\nline two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "alpha", UProject: projectFile}}}, Limits: Limits{MaxSourceBytes: 5}})
	r, err := s.ReadSymbolSource(context.Background(), ReadSymbolSourceInput{ProjectID: "alpha", Path: source, StartLine: 1, EndLine: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "line " || !r.Truncated {
		t.Fatalf("bounded result: %#v", r)
	}
	outside := filepath.Join(t.TempDir(), "secret.cpp")
	if err := os.WriteFile(outside, []byte("no"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSymbolSource(context.Background(), ReadSymbolSourceInput{ProjectID: "alpha", Path: outside, StartLine: 1, EndLine: 1}); !errors.Is(err, ErrPathForbidden) {
		t.Fatalf("outside path: %v", err)
	}
}

func TestSearchSymbolsMapsTimeout(t *testing.T) {
	root := t.TempDir()
	q := &slowQueries{fakeQueries: fakeQueries{}, wait: 50 * time.Millisecond}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "alpha", UProject: filepath.Join(root, "A.uproject")}}}, Queries: q, Limits: Limits{Timeout: time.Millisecond}})
	_, err := s.SearchSymbols(context.Background(), SearchSymbolsInput{ProjectID: "alpha", Query: "Actor"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wanted deadline: %v", err)
	}
}

type slowQueries struct {
	fakeQueries
	wait time.Duration
}

func (f *slowQueries) Symbols(ctx context.Context, p registry.Project, q string, n int) ([]lsp.Symbol, error) {
	select {
	case <-time.After(f.wait):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type fakeQueries struct {
	seenID  string
	symbols []lsp.Symbol
	err     error
}

func (f *fakeQueries) Symbols(_ context.Context, project registry.Project, query string, limit int) ([]lsp.Symbol, error) {
	f.seenID = project.ID
	if f.err != nil {
		return nil, f.err
	}
	return f.symbols, nil
}
func (f *fakeQueries) Locations(context.Context, registry.Project, string, TextPosition, int) ([]lsp.Location, error) {
	return nil, f.err
}
func (f *fakeQueries) DocumentSymbols(context.Context, registry.Project, string, int) ([]lsp.DocumentSymbol, error) {
	return nil, f.err
}
func (f *fakeQueries) Hover(context.Context, registry.Project, TextPosition) (*lsp.HoverResult, error) {
	return nil, f.err
}
func (f *fakeQueries) CallHierarchy(context.Context, registry.Project, string, TextPosition, int) ([]lsp.CallHierarchyItem, error) {
	return nil, f.err
}

func TestSearchSymbolsRoutesExplicitProjectAndTruncates(t *testing.T) {
	root := t.TempDir()
	q := &fakeQueries{symbols: []lsp.Symbol{{Name: "One"}, {Name: "Two"}}}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "alpha", UProject: filepath.Join(root, "Alpha.uproject")}}}, Queries: q, Limits: Limits{MaxItems: 1}})

	result, err := s.SearchSymbols(context.Background(), SearchSymbolsInput{ProjectID: "alpha", Query: "Actor"})
	if err != nil {
		t.Fatal(err)
	}
	if q.seenID != "alpha" {
		t.Fatalf("routed to %q", q.seenID)
	}
	if len(result.Items) != 1 || result.Items[0].Name != "One" || !result.Truncated {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestSearchSymbolsRejectsMissingOrUnknownProject(t *testing.T) {
	s := New(Dependencies{Projects: fakeProjects{}})
	if _, err := s.SearchSymbols(context.Background(), SearchSymbolsInput{Query: "Actor"}); !errors.Is(err, ErrProjectRequired) {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := s.SearchSymbols(context.Background(), SearchSymbolsInput{ProjectID: "missing", Query: "Actor"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

func TestOfficialSDKRegistersAllReadOnlyTools(t *testing.T) {
	s := New(Dependencies{Projects: fakeProjects{}}).MCPServer("test")
	a, b := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	serverSession, err := s.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_projects": true, "project_status": true, "search_symbols": true, "find_definition": true, "find_references": true, "find_implementations": true, "document_symbols": true, "hover": true, "call_hierarchy": true, "read_symbol_source": true}
	for _, tool := range listed.Tools {
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing SDK tools: %v", want)
	}
}

func TestSemanticResultsExcludeOtherProjectPaths(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	project := registry.Project{ID: "alpha", UProject: filepath.Join(root, "A.uproject")}
	inside := filepath.Join(root, "Source.cpp")
	outside := filepath.Join(other, "Other.cpp")
	if err := os.WriteFile(inside, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Dependencies{})
	insideURI := "file:///" + strings.ReplaceAll(inside, "\\", "/")
	outsideURI := "file:///" + strings.ReplaceAll(outside, "\\", "/")
	got, tr := s.filterLocations(project, []lsp.Location{{URI: insideURI}, {URI: outsideURI}}, false)
	if len(got) != 1 || got[0].URI != insideURI || !tr {
		t.Fatalf("unsafe results leaked: %#v %v", got, tr)
	}
}

type countedReader struct {
	data  []byte
	reads int
}

func (r *countedReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
func TestReadSymbolSourceStreamsWithoutReadingWholeFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "S.cpp")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	r := &countedReader{data: append([]byte("first\n"), make([]byte, 1<<20)...)}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "p", UProject: filepath.Join(root, "P.uproject")}}}, OpenFile: func(string) (io.ReadCloser, error) { return io.NopCloser(r), nil }, Limits: Limits{MaxSourceBytes: 32}})
	got, err := s.ReadSymbolSource(context.Background(), ReadSymbolSourceInput{ProjectID: "p", Path: path, StartLine: 1, EndLine: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "first" || r.reads > 2 {
		t.Fatalf("reader was not bounded: %#v reads=%d", got, r.reads)
	}
}

func TestResponseFamiliesRespectSerializedByteCap(t *testing.T) {
	limit := 64
	values := []any{SearchSymbolsResult{Items: []lsp.Symbol{{Name: string(make([]byte, 200))}}, Truncated: true}, LocationsResult{Items: []lsp.Location{{URI: string(make([]byte, 200))}}, Truncated: true}, DocumentSymbolsResult{Items: []DocumentSymbol{{Name: string(make([]byte, 200))}}, Truncated: true}, HoverResult{Item: &lsp.HoverResult{Contents: lsp.MarkupContent{Value: string(make([]byte, 200))}}, Truncated: true}, CallHierarchyResult{Items: []lsp.CallHierarchyItem{{Name: string(make([]byte, 200))}}, Truncated: true}}
	for _, v := range values {
		if _, err := bounded(limit, v); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("oversize %T: %v", v, err)
		}
		b, _ := json.Marshal(struct {
			Truncated bool `json:"truncated"`
		}{true})
		if len(b) > limit {
			t.Fatal("test envelope is not bounded")
		}
	}
}

func TestSafePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.cpp")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.cpp")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := safePath(link, root); !errors.Is(err, ErrPathForbidden) {
		t.Fatalf("symlink escape: %v", err)
	}
}

func TestTwoProjectsRouteToTheirOwnBackendConfiguration(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	q := &fakeQueries{symbols: []lsp.Symbol{{Name: "ok"}}}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "a", UProject: filepath.Join(a, "A.uproject")}, {ID: "b", UProject: filepath.Join(b, "B.uproject")}}}, Queries: q})
	if _, err := s.SearchSymbols(context.Background(), SearchSymbolsInput{ProjectID: "a", Query: "A"}); err != nil {
		t.Fatal(err)
	}
	if q.seenID != "a" {
		t.Fatalf("project A routed to %q", q.seenID)
	}
	if _, err := s.SearchSymbols(context.Background(), SearchSymbolsInput{ProjectID: "b", Query: "B"}); err != nil {
		t.Fatal(err)
	}
	if q.seenID != "b" {
		t.Fatalf("project B routed to %q", q.seenID)
	}
}

func TestMCPStdioHelperProcess(t *testing.T) {
	if os.Getenv("SOFT_UE_INDEX_MCP_HELPER") != "1" {
		return
	}
	if err := New(Dependencies{Projects: fakeProjects{}}).RunStdio(context.Background(), "test"); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestOfficialSDKCommandTransportKeepsStdoutProtocolClean(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.Command(os.Args[0], "-test.run=TestMCPStdioHelperProcess", "--")
	cmd.Env = append(os.Environ(), "SOFT_UE_INDEX_MCP_HELPER=1")
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("stdout was not a clean MCP stream: %v", err)
	}
	if len(listed.Tools) != 10 {
		t.Fatalf("want 10 tools, got %d", len(listed.Tools))
	}
}

func TestFileURIPathDecodesAndRejectsRemoteOrMalformed(t *testing.T) {
	p, err := fileURIPath("file:///work/My%20File.cpp", "linux")
	if err != nil || p != "/work/My File.cpp" {
		t.Fatalf("unix %q %v", p, err)
	}
	p, err = fileURIPath("file:///F:/Work/My%20File.cpp", "windows")
	if err != nil || p != "F:\\Work\\My File.cpp" {
		t.Fatalf("windows %q %v", p, err)
	}
	if _, err := fileURIPath("file://server/share/a.cpp", "windows"); !errors.Is(err, ErrPathForbidden) {
		t.Fatalf("UNC: %v", err)
	}
	if _, err := fileURIPath("http://example/a", "linux"); !errors.Is(err, ErrPathForbidden) {
		t.Fatalf("scheme: %v", err)
	}
}

func TestCallHierarchyDepthIsBounded(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "S.cpp")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "p", UProject: filepath.Join(root, "P.uproject")}}}, Queries: &fakeQueries{}, Limits: Limits{MaxCallDepth: 2}})
	_, err := s.CallHierarchy(context.Background(), CallHierarchyInput{ProjectID: "p", Position: TextPosition{Path: path}, MaxDepth: 3})
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("depth ceiling: %v", err)
	}
	if _, err := s.CallHierarchy(context.Background(), CallHierarchyInput{ProjectID: "p", Position: TextPosition{Path: path}, MaxDepth: 0}); err != nil {
		t.Fatal(err)
	}
}
