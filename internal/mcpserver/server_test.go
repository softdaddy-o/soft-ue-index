package mcpserver

import (
	"bufio"
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
	if r.Text != "line " || !r.Truncated || r.EndLine != 1 {
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
func (f *fakeQueries) PrepareCallHierarchy(context.Context, registry.Project, TextPosition) ([]lsp.CallHierarchyItem, error) {
	return nil, f.err
}
func (f *fakeQueries) IncomingCalls(context.Context, registry.Project, lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	return nil, f.err
}
func (f *fakeQueries) OutgoingCalls(context.Context, registry.Project, lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
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

func TestRejectsResponseLimitsTooSmallForMCPEnvelope(t *testing.T) {
	s := New(Dependencies{Projects: fakeProjects{}, Limits: Limits{MaxResponseBytes: minimumResponseBytes - 1}})
	if _, err := s.ListProjects(context.Background(), 1); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("limit validation: %v", err)
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

type blockingReader struct{ closed chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) { <-r.closed; return 0, errors.New("closed") }
func (r *blockingReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
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

func TestReadSymbolSourceServerTimeoutClosesBlockedReader(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "S.cpp")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	r := &blockingReader{closed: make(chan struct{})}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "p", UProject: filepath.Join(root, "P.uproject")}}}, OpenFile: func(string) (io.ReadCloser, error) { return r, nil }, Limits: Limits{Timeout: 20 * time.Millisecond}})
	_, err := s.ReadSymbolSource(context.Background(), ReadSymbolSourceInput{ProjectID: "p", Path: path, StartLine: 1, EndLine: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error: %v", err)
	}
	select {
	case <-r.closed:
	case <-time.After(time.Second):
		t.Fatal("reader was not closed")
	}
}

func TestResponseFamiliesRespectSerializedByteCap(t *testing.T) {
	limit := 64
	values := []any{SearchSymbolsResult{Items: []lsp.Symbol{{Name: string(make([]byte, 200))}}, Truncated: true}, LocationsResult{Items: []lsp.Location{{URI: string(make([]byte, 200))}}, Truncated: true}, DocumentSymbolsResult{Items: []DocumentSymbol{{Name: string(make([]byte, 200))}}, Truncated: true}, HoverResult{Item: &lsp.HoverResult{Contents: lsp.MarkupContent{Value: string(make([]byte, 200))}}, Truncated: true}, CallHierarchyResult{Nodes: []CallHierarchyNode{{Item: lsp.CallHierarchyItem{Name: string(make([]byte, 200))}}}, Truncated: true}}
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
	q := &fakeQueries{symbols: []lsp.Symbol{{Name: "s"}}}
	if os.Getenv("SOFT_UE_INDEX_MCP_MODE") == "error" {
		q.err = errors.New(strings.Repeat("backend-secret", 1<<18))
	}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "p", UProject: "P.uproject"}}}, Queries: q, Limits: Limits{MaxResponseBytes: 512}})
	if err := s.RunStdio(context.Background(), "test"); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestOfficialSDKStdioCallToolFramesRespectResponseCap(t *testing.T) {
	for _, mode := range []string{"success", "error"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestMCPStdioHelperProcess", "--")
			cmd.Env = append(os.Environ(), "SOFT_UE_INDEX_MCP_HELPER=1", "SOFT_UE_INDEX_MCP_MODE="+mode)
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = in.Close(); _ = cmd.Wait() }()
			write := func(v any) {
				if err := json.NewEncoder(in).Encode(v); err != nil {
					t.Fatal(err)
				}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2026-07-28", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "test"}}})
			scanner := bufio.NewScanner(out)
			scanner.Buffer(make([]byte, 1024), 64*1024)
			for scanner.Scan() {
				var msg struct {
					ID int `json:"id"`
				}
				if json.Unmarshal(scanner.Bytes(), &msg) == nil && msg.ID == 1 {
					break
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
			requestID := strings.Repeat("i", defaultMaxRequestIDBytes)
			write(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": "tools/call", "params": map[string]any{"name": "search_symbols", "arguments": map[string]any{"project_id": "p", "query": "x"}}})
			for scanner.Scan() {
				line := append([]byte(nil), scanner.Bytes()...)
				var msg struct {
					ID     string              `json:"id"`
					Result *mcp.CallToolResult `json:"result"`
				}
				if json.Unmarshal(line, &msg) != nil || msg.ID != requestID {
					continue
				}
				if len(line)+1 > 512 {
					t.Fatalf("stdio frame=%d", len(line)+1)
				}
				if msg.Result == nil || msg.Result.IsError != (mode == "error") {
					t.Fatalf("result=%#v", msg.Result)
				}
				if strings.Contains(string(line), "backend-secret") {
					t.Fatal("backend error leaked")
				}
				return
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			t.Fatal("missing tools/call response")
		})
	}
}

func TestStdioIngressRejectsOversizedFramesAndStringIDs(t *testing.T) {
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":"` + strings.Repeat("i", defaultMaxRequestIDBytes+1) + `","method":"tools/call","params":{}}` + "\n",
		`{"jsonrpc":"2.0","method":"tools/call","params":"` + strings.Repeat("x", defaultMaxRequestBytes) + `"}` + "\n",
	} {
		t.Run("reject", func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestMCPStdioHelperProcess", "--")
			cmd.Env = append(os.Environ(), "SOFT_UE_INDEX_MCP_HELPER=1")
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(in, input); err != nil {
				t.Fatal(err)
			}
			_ = in.Close()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-time.After(3 * time.Second):
				t.Fatal("server did not stop after rejected ingress")
			case <-done:
			}
			data, _ := io.ReadAll(out)
			if len(data) != 0 {
				t.Fatalf("unexpected response: %q", data)
			}
		})
	}
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

type hierarchyQueries struct {
	fakeQueries
	roots    []lsp.CallHierarchyItem
	incoming map[string][]lsp.CallHierarchyCall
	outgoing map[string][]lsp.CallHierarchyCall
}

func (q *hierarchyQueries) PrepareCallHierarchy(context.Context, registry.Project, TextPosition) ([]lsp.CallHierarchyItem, error) {
	return q.roots, q.err
}
func (q *hierarchyQueries) IncomingCalls(_ context.Context, _ registry.Project, item lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	return q.incoming[callKey(item)], q.err
}
func (q *hierarchyQueries) OutgoingCalls(_ context.Context, _ registry.Project, item lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	return q.outgoing[callKey(item)], q.err
}

func TestCallHierarchyTraversesDepthsCyclesAndDirections(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "S.cpp")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	uri := "file:///" + strings.ReplaceAll(path, "\\", "/")
	item := func(name string, line int) lsp.CallHierarchyItem {
		return lsp.CallHierarchyItem{Name: name, URI: uri, Range: lsp.Range{Start: lsp.Position{Line: line}}}
	}
	a, b, c := item("A", 0), item("B", 1), item("C", 2)
	q := &hierarchyQueries{roots: []lsp.CallHierarchyItem{a}, outgoing: map[string][]lsp.CallHierarchyCall{
		callKey(a): {{To: &b}}, callKey(b): {{To: &c}}, callKey(c): {{To: &a}},
	}, incoming: map[string][]lsp.CallHierarchyCall{callKey(a): {{From: &b}}}}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "p", UProject: filepath.Join(root, "P.uproject")}}}, Queries: q, Limits: Limits{MaxCallDepth: 3, MaxItems: 20, MaxResponseBytes: 4096}})
	for depth, edges := range map[int]int{0: 0, 1: 1, 2: 2, 3: 3} {
		got, err := s.CallHierarchy(context.Background(), CallHierarchyInput{ProjectID: "p", Position: TextPosition{Path: path}, Direction: "outgoing", MaxDepth: depth})
		if err != nil || len(got.Edges) != edges {
			t.Fatalf("depth %d: edges=%d err=%v", depth, len(got.Edges), err)
		}
	}
	got, err := s.CallHierarchy(context.Background(), CallHierarchyInput{ProjectID: "p", Position: TextPosition{Path: path}, Direction: "incoming", MaxDepth: 1})
	if err != nil || len(got.Edges) != 1 || got.Edges[0].To != callKey(b) {
		t.Fatalf("incoming=%#v err=%v", got, err)
	}
}

func TestCallHierarchyLimitsItemsAndSerializedBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "S.cpp")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	uri := "file:///" + strings.ReplaceAll(path, "\\", "/")
	a := lsp.CallHierarchyItem{Name: "A", URI: uri}
	b := lsp.CallHierarchyItem{Name: "B", URI: uri, Range: lsp.Range{Start: lsp.Position{Line: 1}}}
	c := lsp.CallHierarchyItem{Name: "C", URI: uri, Range: lsp.Range{Start: lsp.Position{Line: 2}}}
	q := &hierarchyQueries{roots: []lsp.CallHierarchyItem{a}, outgoing: map[string][]lsp.CallHierarchyCall{callKey(a): {{To: &b}, {To: &c}}}}
	project := fakeProjects{projects: []registry.Project{{ID: "p", UProject: filepath.Join(root, "P.uproject")}}}
	s := New(Dependencies{Projects: project, Queries: q, Limits: Limits{MaxCallDepth: 2, MaxItems: 3, MaxResponseBytes: 4096}})
	got, err := s.CallHierarchy(context.Background(), CallHierarchyInput{ProjectID: "p", Position: TextPosition{Path: path}, Direction: "outgoing", MaxDepth: 1})
	if err != nil || !got.Truncated || len(got.Nodes)+len(got.Edges) > 3 {
		t.Fatalf("item cap=%#v err=%v", got, err)
	}
	big := a
	big.Name = strings.Repeat("x", 4096)
	q.roots = []lsp.CallHierarchyItem{big}
	s = New(Dependencies{Projects: project, Queries: q, Limits: Limits{MaxCallDepth: 1, MaxItems: 5, MaxResponseBytes: 512}})
	got, err = s.CallHierarchy(context.Background(), CallHierarchyInput{ProjectID: "p", Position: TextPosition{Path: path}})
	if err != nil || !got.Truncated {
		t.Fatalf("byte cap=%#v err=%v", got, err)
	}
	encoded, _ := json.Marshal(got)
	if len(encoded) > 512-protocolEnvelopeBytes {
		t.Fatalf("serialized result exceeded cap: %d", len(encoded))
	}
}

func TestCallHierarchyPropagatesRequestTimeout(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "S.cpp")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	uri := "file:///" + strings.ReplaceAll(path, "\\", "/")
	// The adapter keeps this test focused on server-owned timeout behavior.
	timeoutQ := &timeoutCalls{root: lsp.CallHierarchyItem{URI: uri}}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "p", UProject: filepath.Join(root, "P.uproject")}}}, Queries: timeoutQ, Limits: Limits{Timeout: time.Millisecond}})
	_, err := s.CallHierarchy(context.Background(), CallHierarchyInput{ProjectID: "p", Position: TextPosition{Path: path}, Direction: "outgoing", MaxDepth: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
}

type timeoutCalls struct {
	fakeQueries
	root lsp.CallHierarchyItem
}

func (q *timeoutCalls) PrepareCallHierarchy(context.Context, registry.Project, TextPosition) ([]lsp.CallHierarchyItem, error) {
	return []lsp.CallHierarchyItem{q.root}, nil
}
func (q *timeoutCalls) OutgoingCalls(ctx context.Context, _ registry.Project, _ lsp.CallHierarchyItem) ([]lsp.CallHierarchyCall, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestOfficialSDKCallToolSanitizesLargeBackendError(t *testing.T) {
	root := t.TempDir()
	q := &fakeQueries{err: errors.New(strings.Repeat("secret", 1<<20))}
	s := New(Dependencies{Projects: fakeProjects{projects: []registry.Project{{ID: "p", UProject: filepath.Join(root, "P.uproject")}}}, Queries: q, Limits: Limits{MaxResponseBytes: 512}}).MCPServer("test")
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
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "search_symbols", Arguments: map[string]any{"project_id": "p", "query": "x"}})
	if err != nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("tool error=%v result=%#v", err, result)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 512 || strings.Contains(string(encoded), "secret") {
		t.Fatalf("unsanitized MCP result bytes=%d err=%v", len(encoded), err)
	}
	wire, err := json.Marshal(struct {
		JSONRPC string              `json:"jsonrpc"`
		ID      int                 `json:"id"`
		Result  *mcp.CallToolResult `json:"result"`
	}{JSONRPC: "2.0", ID: 1, Result: result})
	if err != nil || len(wire) > 512 {
		t.Fatalf("serialized MCP response exceeded cap: bytes=%d err=%v", len(wire), err)
	}
}
