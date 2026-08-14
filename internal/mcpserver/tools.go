package mcpserver

import (
	"context"

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
		r, e := s.ListProjects(ctx, in.MaxItems)
		return textResult(e), r, e
	})
	mcp.AddTool(m, &mcp.Tool{Name: "project_status", Description: "Get compilation database status for one explicit project."}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectStatusInput) (*mcp.CallToolResult, ProjectStatusResult, error) {
		r, e := s.ProjectStatus(ctx, in)
		return textResult(e), r, e
	})
	mcp.AddTool(m, &mcp.Tool{Name: "search_symbols", Description: "Search symbols in one explicit project."}, func(ctx context.Context, _ *mcp.CallToolRequest, in SearchSymbolsInput) (*mcp.CallToolResult, SearchSymbolsResult, error) {
		r, e := s.SearchSymbols(ctx, in)
		return textResult(e), r, e
	})
	addLocations(m, "find_definition", "Find a definition.", s.FindDefinition)
	addLocations(m, "find_references", "Find references.", s.FindReferences)
	addLocations(m, "find_implementations", "Find implementations.", s.FindImplementations)
	mcp.AddTool(m, &mcp.Tool{Name: "document_symbols", Description: "List document symbols for a project or engine source file."}, func(ctx context.Context, _ *mcp.CallToolRequest, in PathQueryInput) (*mcp.CallToolResult, DocumentSymbolsResult, error) {
		r, e := s.DocumentSymbols(ctx, in)
		return textResult(e), r, e
	})
	mcp.AddTool(m, &mcp.Tool{Name: "hover", Description: "Get hover information at a source position."}, func(ctx context.Context, _ *mcp.CallToolRequest, in LocationQueryInput) (*mcp.CallToolResult, HoverResult, error) {
		r, e := s.Hover(ctx, in)
		return textResult(e), r, e
	})
	mcp.AddTool(m, &mcp.Tool{Name: "call_hierarchy", Description: "Prepare, list incoming, or list outgoing calls at a source position."}, func(ctx context.Context, _ *mcp.CallToolRequest, in CallHierarchyInput) (*mcp.CallToolResult, CallHierarchyResult, error) {
		r, e := s.CallHierarchy(ctx, in)
		return textResult(e), r, e
	})
	mcp.AddTool(m, &mcp.Tool{Name: "read_symbol_source", Description: "Read a line-bounded source excerpt inside the selected project or engine."}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadSymbolSourceInput) (*mcp.CallToolResult, ReadSymbolSourceResult, error) {
		r, e := s.ReadSymbolSource(ctx, in)
		return textResult(e), r, e
	})
	return m
}

func addLocations(m *mcp.Server, name, description string, f func(context.Context, LocationQueryInput) (LocationsResult, error)) {
	mcp.AddTool(m, &mcp.Tool{Name: name, Description: description}, func(ctx context.Context, _ *mcp.CallToolRequest, in LocationQueryInput) (*mcp.CallToolResult, LocationsResult, error) {
		r, e := f(ctx, in)
		return textResult(e), r, e
	})
}
func textResult(err error) *mcp.CallToolResult {
	if err == nil {
		return &mcp.CallToolResult{}
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}

// RunStdio serves MCP only through stdin/stdout. Callers must send diagnostics
// elsewhere; this package never writes logs or status messages to stdout.
func (s *Server) RunStdio(ctx context.Context, version string) error {
	return s.MCPServer(version).Run(ctx, &mcp.StdioTransport{})
}
