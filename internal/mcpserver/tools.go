package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServer constructs the official SDK server. It deliberately configures no
// logging: stdout remains exclusively owned by the stdio JSON-RPC transport.
func (s *Server) MCPServer(version string) *mcp.Server {
	if version == "" {
		version = "dev"
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "soft-ue-index", Version: version}, nil)
	mcp.AddTool(m, &mcp.Tool{Name: "list_projects", Description: "List registered Unreal projects."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		MaxItems int `json:"max_items,omitempty"`
	}) (*mcp.CallToolResult, ListProjectsResult, error) {
		return admitted(s, func() (ListProjectsResult, error) { return s.ListProjects(ctx, in.MaxItems) })
	})
	mcp.AddTool(m, &mcp.Tool{Name: "project_status", Description: "Get compilation database status for one explicit project."}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectStatusInput) (*mcp.CallToolResult, ProjectStatusResult, error) {
		return admitted(s, func() (ProjectStatusResult, error) { return s.ProjectStatus(ctx, in) })
	})
	mcp.AddTool(m, &mcp.Tool{Name: "search_symbols", Description: "Search symbols in one explicit project."}, func(ctx context.Context, _ *mcp.CallToolRequest, in SearchSymbolsInput) (*mcp.CallToolResult, SearchSymbolsResult, error) {
		return admitted(s, func() (SearchSymbolsResult, error) { return s.SearchSymbols(ctx, in) })
	})
	addLocations(m, "find_definition", "Find a definition.", s.FindDefinition, s)
	addLocations(m, "find_references", "Find references.", s.FindReferences, s)
	addLocations(m, "find_implementations", "Find implementations.", s.FindImplementations, s)
	mcp.AddTool(m, &mcp.Tool{Name: "document_symbols", Description: "List document symbols for a project or engine source file."}, func(ctx context.Context, _ *mcp.CallToolRequest, in PathQueryInput) (*mcp.CallToolResult, DocumentSymbolsResult, error) {
		return admitted(s, func() (DocumentSymbolsResult, error) { return s.DocumentSymbols(ctx, in) })
	})
	mcp.AddTool(m, &mcp.Tool{Name: "hover", Description: "Get hover information at a source position."}, func(ctx context.Context, _ *mcp.CallToolRequest, in LocationQueryInput) (*mcp.CallToolResult, HoverResult, error) {
		return admitted(s, func() (HoverResult, error) { return s.Hover(ctx, in) })
	})
	mcp.AddTool(m, &mcp.Tool{Name: "call_hierarchy", Description: "Prepare, list incoming, or list outgoing calls at a source position."}, func(ctx context.Context, _ *mcp.CallToolRequest, in CallHierarchyInput) (*mcp.CallToolResult, CallHierarchyResult, error) {
		return admitted(s, func() (CallHierarchyResult, error) { return s.CallHierarchy(ctx, in) })
	})
	mcp.AddTool(m, &mcp.Tool{Name: "read_symbol_source", Description: "Read a line-bounded source excerpt inside the selected project or engine."}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadSymbolSourceInput) (*mcp.CallToolResult, ReadSymbolSourceResult, error) {
		return admitted(s, func() (ReadSymbolSourceResult, error) { return s.ReadSymbolSource(ctx, in) })
	})
	return m
}

func addLocations(m *mcp.Server, name, description string, f func(context.Context, LocationQueryInput) (LocationsResult, error), s *Server) {
	mcp.AddTool(m, &mcp.Tool{Name: name, Description: description}, func(ctx context.Context, _ *mcp.CallToolRequest, in LocationQueryInput) (*mcp.CallToolResult, LocationsResult, error) {
		return admitted(s, func() (LocationsResult, error) { return f(ctx, in) })
	})
}

func admitted[T any](s *Server, run func() (T, error)) (*mcp.CallToolResult, T, error) {
	release, err := s.admitTool()
	if err != nil {
		var zero T
		result, mapped := s.toolResult(zero, err)
		return result, zero, mapped
	}
	defer release()
	value, err := run()
	result, err := s.toolResult(value, err)
	return result, value, err
}

// toolResult reserves a conservative 256-byte JSON-RPC envelope for a normal
// SDK request ID and the newline delimiter. Successful typed output is checked
// in the same CallToolResult shape that the SDK serializes. Error results use a
// regular, already-sanitized tool error so the SDK does not attach output.
func (s *Server) toolResult(out any, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return nil, s.compactToolError(mapError(err))
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: out}
	if s.fitsToolResult(result) {
		return result, nil
	}
	return nil, s.compactToolError(ErrLimitExceeded)
}

func (s *Server) fitsToolResult(result *mcp.CallToolResult) bool {
	encoded, err := json.Marshal(result)
	return err == nil && len(encoded)+protocolEnvelopeBytes <= s.limits.MaxResponseBytes
}

func (s *Server) compactToolError(err error) error {
	message := mapError(err).Error()
	for len(message) > 0 {
		result := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
		if s.fitsToolResult(result) {
			return errors.New(message)
		}
		message = message[:len(message)-1]
	}
	return errors.New("error")
}

// RunStdio serves MCP only through stdin/stdout. Callers must send diagnostics
// elsewhere; this package never writes logs or status messages to stdout.
func (s *Server) RunStdio(ctx context.Context, version string) error {
	return s.RunIO(ctx, version, os.Stdin, os.Stdout)
}

// RunIO serves MCP over a bounded JSON-RPC frame transport on the provided streams.
// Readers and writers are wrapped before the SDK sees any frame data.
func (s *Server) RunIO(ctx context.Context, version string, reader io.Reader, writer io.Writer) error {
	boundedReader := newBoundedJSONReader(reader, s.limits.MaxRequestBytes, s.limits.MaxRequestIDBytes)
	boundedWriter := newBoundedJSONWriter(writer, s.limits.MaxResponseBytes)
	return s.MCPServer(version).Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(boundedReader), Writer: boundedWriter})
}
