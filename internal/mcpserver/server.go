// Package mcpserver exposes bounded, read-only Unreal code intelligence over MCP.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/softdaddy-o/soft-ue-index/internal/lsp"
	"github.com/softdaddy-o/soft-ue-index/internal/registry"
)

var (
	ErrProjectRequired = errors.New("project_id is required")
	ErrProjectNotFound = errors.New("project not found")
	ErrPathForbidden   = errors.New("path is outside the selected project or engine")
	ErrLimitExceeded   = errors.New("request exceeds configured limit")
)

type ProjectLoader interface {
	Load(context.Context) (registry.Registry, error)
}

// Queries is deliberately injected: MCP owns validation and response shaping,
// while the app layer owns clangd lifecycle and converts calls to lsp.Client.
type Queries interface {
	Symbols(context.Context, registry.Project, string, int) ([]lsp.Symbol, error)
	Locations(context.Context, registry.Project, string, TextPosition, int) ([]lsp.Location, error)
	DocumentSymbols(context.Context, registry.Project, string, int) ([]lsp.DocumentSymbol, error)
	Hover(context.Context, registry.Project, TextPosition) (*lsp.HoverResult, error)
	CallHierarchy(context.Context, registry.Project, string, TextPosition, int) (any, error)
}

type Dependencies struct {
	Projects ProjectLoader
	Queries  Queries
	ReadFile func(string) ([]byte, error)
	Limits   Limits
}

type Server struct {
	projects ProjectLoader
	queries  Queries
	readFile func(string) ([]byte, error)
	limits   Limits
}

func New(d Dependencies) *Server {
	if d.ReadFile == nil {
		d.ReadFile = os.ReadFile
	}
	return &Server{projects: d.Projects, queries: d.Queries, readFile: d.ReadFile, limits: d.Limits.normalized()}
}

type SearchSymbolsInput struct {
	ProjectID string `json:"project_id"`
	Query     string `json:"query"`
	MaxItems  int    `json:"max_items,omitempty"`
}
type SearchSymbolsResult struct {
	Items     []lsp.Symbol `json:"items"`
	Truncated bool         `json:"truncated"`
}

func (s *Server) SearchSymbols(ctx context.Context, in SearchSymbolsInput) (SearchSymbolsResult, error) {
	if len(in.Query) > s.limits.MaxQueryBytes {
		return SearchSymbolsResult{}, ErrLimitExceeded
	}
	p, err := s.project(ctx, in.ProjectID)
	if err != nil {
		return SearchSymbolsResult{}, err
	}
	if s.queries == nil {
		return SearchSymbolsResult{}, errors.New("code intelligence is unavailable")
	}
	max := s.itemLimit(in.MaxItems)
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	items, err := s.queries.Symbols(ctx, p, in.Query, max+1)
	if err != nil {
		return SearchSymbolsResult{}, mapError(err)
	}
	result := SearchSymbolsResult{Items: items}
	if len(result.Items) > max {
		result.Items = result.Items[:max]
		result.Truncated = true
	}
	return result, nil
}

type TextPosition struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}
type LocationQueryInput struct {
	ProjectID string       `json:"project_id"`
	Position  TextPosition `json:"position"`
	MaxItems  int          `json:"max_items,omitempty"`
}
type CallHierarchyInput struct {
	ProjectID string       `json:"project_id"`
	Position  TextPosition `json:"position"`
	Direction string       `json:"direction,omitempty"`
	MaxItems  int          `json:"max_items,omitempty"`
}
type LocationsResult struct {
	Items     []lsp.Location `json:"items"`
	Truncated bool           `json:"truncated"`
}
type PathQueryInput struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	MaxItems  int    `json:"max_items,omitempty"`
}
type DocumentSymbolsResult struct {
	Items     []lsp.DocumentSymbol `json:"items"`
	Truncated bool                 `json:"truncated"`
}
type HoverResult struct {
	Item *lsp.HoverResult `json:"item,omitempty"`
}
type CallHierarchyResult struct {
	Item any `json:"item"`
}

func (s *Server) locations(ctx context.Context, kind string, in LocationQueryInput) (LocationsResult, error) {
	p, err := s.project(ctx, in.ProjectID)
	if err != nil {
		return LocationsResult{}, err
	}
	if s.queries == nil {
		return LocationsResult{}, errors.New("code intelligence is unavailable")
	}
	if err := s.validatePosition(p, in.Position); err != nil {
		return LocationsResult{}, err
	}
	max := s.itemLimit(in.MaxItems)
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	items, err := s.queries.Locations(ctx, p, kind, in.Position, max+1)
	if err != nil {
		return LocationsResult{}, mapError(err)
	}
	r := LocationsResult{Items: items}
	if len(r.Items) > max {
		r.Items = r.Items[:max]
		r.Truncated = true
	}
	return r, nil
}
func (s *Server) FindDefinition(ctx context.Context, in LocationQueryInput) (LocationsResult, error) {
	return s.locations(ctx, "definition", in)
}
func (s *Server) FindReferences(ctx context.Context, in LocationQueryInput) (LocationsResult, error) {
	return s.locations(ctx, "references", in)
}
func (s *Server) FindImplementations(ctx context.Context, in LocationQueryInput) (LocationsResult, error) {
	return s.locations(ctx, "implementations", in)
}
func (s *Server) DocumentSymbols(ctx context.Context, in PathQueryInput) (DocumentSymbolsResult, error) {
	p, err := s.project(ctx, in.ProjectID)
	if err != nil {
		return DocumentSymbolsResult{}, err
	}
	if s.queries == nil {
		return DocumentSymbolsResult{}, errors.New("code intelligence is unavailable")
	}
	path, err := s.safeProjectPath(p, in.Path)
	if err != nil {
		return DocumentSymbolsResult{}, err
	}
	max := s.itemLimit(in.MaxItems)
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	items, err := s.queries.DocumentSymbols(ctx, p, path, max+1)
	if err != nil {
		return DocumentSymbolsResult{}, mapError(err)
	}
	r := DocumentSymbolsResult{Items: items}
	if len(r.Items) > max {
		r.Items = r.Items[:max]
		r.Truncated = true
	}
	return r, nil
}
func (s *Server) Hover(ctx context.Context, in LocationQueryInput) (HoverResult, error) {
	p, err := s.project(ctx, in.ProjectID)
	if err != nil {
		return HoverResult{}, err
	}
	if s.queries == nil {
		return HoverResult{}, errors.New("code intelligence is unavailable")
	}
	if err = s.validatePosition(p, in.Position); err != nil {
		return HoverResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	v, err := s.queries.Hover(ctx, p, in.Position)
	return HoverResult{Item: v}, mapError(err)
}
func (s *Server) CallHierarchy(ctx context.Context, in CallHierarchyInput) (CallHierarchyResult, error) {
	p, err := s.project(ctx, in.ProjectID)
	if err != nil {
		return CallHierarchyResult{}, err
	}
	if s.queries == nil {
		return CallHierarchyResult{}, errors.New("code intelligence is unavailable")
	}
	if err = s.validatePosition(p, in.Position); err != nil {
		return CallHierarchyResult{}, err
	}
	kind := in.Direction
	if kind == "" {
		kind = "prepare"
	}
	if kind != "prepare" && kind != "incoming" && kind != "outgoing" {
		return CallHierarchyResult{}, errors.New("direction must be prepare, incoming, or outgoing")
	}
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	v, err := s.queries.CallHierarchy(ctx, p, kind, in.Position, s.itemLimit(in.MaxItems))
	return CallHierarchyResult{Item: v}, mapError(err)
}
func (s *Server) validatePosition(p registry.Project, v TextPosition) error {
	if v.Line < 0 || v.Character < 0 {
		return errors.New("line and character must be non-negative")
	}
	_, err := s.safeProjectPath(p, v.Path)
	return err
}
func (s *Server) safeProjectPath(p registry.Project, path string) (string, error) {
	if len(path) == 0 || len(path) > s.limits.MaxPathBytes {
		return "", ErrLimitExceeded
	}
	return safePath(path, filepath.Dir(p.UProject), p.Engine.Root)
}

type ListProjectsResult struct {
	Items     []ProjectSummary `json:"items"`
	Truncated bool             `json:"truncated"`
}
type ProjectSummary struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	EngineVersion string `json:"engine_version,omitempty"`
	Ready         bool   `json:"ready"`
}

func (s *Server) ListProjects(ctx context.Context, maxItems int) (ListProjectsResult, error) {
	if s.projects == nil {
		return ListProjectsResult{}, errors.New("project registry is unavailable")
	}
	r, err := s.projects.Load(ctx)
	if err != nil {
		return ListProjectsResult{}, mapError(err)
	}
	max := s.itemLimit(maxItems)
	out := ListProjectsResult{Items: make([]ProjectSummary, 0, min(max, len(r.Projects)))}
	for _, p := range r.Projects {
		if len(out.Items) == max {
			out.Truncated = true
			break
		}
		out.Items = append(out.Items, ProjectSummary{ID: p.ID, Name: p.Name, EngineVersion: p.Engine.Version, Ready: p.Generation.CompilationDatabase != ""})
	}
	return out, nil
}

type ProjectStatusInput struct {
	ProjectID string `json:"project_id"`
}
type ProjectStatusResult struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	EngineVersion   string `json:"engine_version,omitempty"`
	Ready           bool   `json:"ready"`
	LastFingerprint string `json:"last_fingerprint,omitempty"`
}

func (s *Server) ProjectStatus(ctx context.Context, in ProjectStatusInput) (ProjectStatusResult, error) {
	p, err := s.project(ctx, in.ProjectID)
	if err != nil {
		return ProjectStatusResult{}, err
	}
	return ProjectStatusResult{ID: p.ID, Name: p.Name, EngineVersion: p.Engine.Version, Ready: p.Generation.CompilationDatabase != "", LastFingerprint: p.Generation.LastFingerprint}, nil
}

type ReadSymbolSourceInput struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	MaxBytes  int    `json:"max_bytes,omitempty"`
}
type ReadSymbolSourceResult struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

func (s *Server) ReadSymbolSource(ctx context.Context, in ReadSymbolSourceInput) (ReadSymbolSourceResult, error) {
	p, err := s.project(ctx, in.ProjectID)
	if err != nil {
		return ReadSymbolSourceResult{}, err
	}
	if len(in.Path) == 0 || len(in.Path) > s.limits.MaxPathBytes {
		return ReadSymbolSourceResult{}, ErrLimitExceeded
	}
	if in.StartLine < 1 || in.EndLine < in.StartLine {
		return ReadSymbolSourceResult{}, errors.New("invalid line range")
	}
	path, err := safePath(in.Path, filepath.Dir(p.UProject), p.Engine.Root)
	if err != nil {
		return ReadSymbolSourceResult{}, err
	}
	contents, err := s.readFile(path)
	if err != nil {
		return ReadSymbolSourceResult{}, mapError(err)
	}
	limit := s.limits.MaxSourceBytes
	if in.MaxBytes > 0 && in.MaxBytes < limit {
		limit = in.MaxBytes
	}
	lines := strings.Split(string(contents), "\n")
	if in.StartLine > len(lines) {
		return ReadSymbolSourceResult{}, errors.New("start_line is outside the file")
	}
	end := min(in.EndLine, len(lines))
	text := strings.Join(lines[in.StartLine-1:end], "\n")
	truncated := false
	if len(text) > limit {
		text = text[:limit]
		truncated = true
	}
	return ReadSymbolSourceResult{Path: path, StartLine: in.StartLine, EndLine: end, Text: text, Truncated: truncated}, nil
}

func (s *Server) project(ctx context.Context, id string) (registry.Project, error) {
	if id == "" {
		return registry.Project{}, ErrProjectRequired
	}
	if s.projects == nil {
		return registry.Project{}, errors.New("project registry is unavailable")
	}
	r, err := s.projects.Load(ctx)
	if err != nil {
		return registry.Project{}, mapError(err)
	}
	for _, p := range r.Projects {
		if p.ID == id {
			return p, nil
		}
	}
	return registry.Project{}, fmt.Errorf("%w: %s", ErrProjectNotFound, id)
}
func (s *Server) itemLimit(requested int) int {
	if requested > 0 && requested < s.limits.MaxItems {
		return requested
	}
	return s.limits.MaxItems
}
func mapError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("request timed out: %w", err)
	}
	return err
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func safePath(path string, roots ...string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		rr, e := filepath.EvalSymlinks(root)
		if e != nil {
			continue
		}
		rel, e := filepath.Rel(rr, resolved)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return resolved, nil
		}
	}
	return "", ErrPathForbidden
}
