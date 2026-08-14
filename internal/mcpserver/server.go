// Package mcpserver exposes bounded, read-only Unreal code intelligence over MCP.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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
	CallHierarchy(context.Context, registry.Project, string, TextPosition, int) ([]lsp.CallHierarchyItem, error)
}

type Dependencies struct {
	Projects ProjectLoader
	Queries  Queries
	OpenFile func(string) (io.ReadCloser, error)
	Limits   Limits
}

type Server struct {
	projects ProjectLoader
	queries  Queries
	openFile func(string) (io.ReadCloser, error)
	limits   Limits
}

func New(d Dependencies) *Server {
	if d.OpenFile == nil {
		d.OpenFile = func(path string) (io.ReadCloser, error) { return os.Open(path) }
	}
	return &Server{projects: d.Projects, queries: d.Queries, openFile: d.OpenFile, limits: d.Limits.normalized()}
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
	result.Items, result.Truncated = s.filterSymbols(p, result.Items, result.Truncated)
	result.Items, result.Truncated = trimSymbols(result.Items, s.limits.MaxResponseBytes, result.Truncated)
	return bounded(s.limits.MaxResponseBytes, result)
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
	MaxDepth  int          `json:"max_depth,omitempty"`
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
type DocumentSymbol struct {
	Name           string    `json:"name"`
	Kind           int       `json:"kind"`
	Range          lsp.Range `json:"range"`
	SelectionRange lsp.Range `json:"selection_range"`
}
type DocumentSymbolsResult struct {
	Items     []DocumentSymbol `json:"items"`
	Truncated bool             `json:"truncated"`
}
type HoverResult struct {
	Item      *lsp.HoverResult `json:"item,omitempty"`
	Truncated bool             `json:"truncated"`
}
type CallHierarchyResult struct {
	Items     []lsp.CallHierarchyItem `json:"items"`
	Truncated bool                    `json:"truncated"`
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
	r.Items, r.Truncated = s.filterLocations(p, r.Items, r.Truncated)
	r.Items, r.Truncated = trimLocations(r.Items, s.limits.MaxResponseBytes, r.Truncated)
	return bounded(s.limits.MaxResponseBytes, r)
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
	r := DocumentSymbolsResult{Items: flattenSymbols(items)}
	if len(r.Items) > max {
		r.Items = r.Items[:max]
		r.Truncated = true
	}
	r.Items, r.Truncated = trimDocumentSymbols(r.Items, s.limits.MaxResponseBytes, r.Truncated)
	return bounded(s.limits.MaxResponseBytes, r)
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
	if err != nil {
		return HoverResult{}, mapError(err)
	}
	r := HoverResult{Item: v}
	if v != nil && len(v.Contents.Value) > s.limits.MaxResponseBytes {
		v.Contents.Value = v.Contents.Value[:s.limits.MaxResponseBytes]
		r.Truncated = true
	}
	return bounded(s.limits.MaxResponseBytes, r)
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
	if in.MaxDepth < 0 || in.MaxDepth > s.limits.MaxCallDepth {
		return CallHierarchyResult{}, ErrLimitExceeded
	}
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	v, err := s.queries.CallHierarchy(ctx, p, kind, in.Position, s.itemLimit(in.MaxItems)+1)
	if err != nil {
		return CallHierarchyResult{}, mapError(err)
	}
	r := CallHierarchyResult{Items: v}
	if len(r.Items) > s.itemLimit(in.MaxItems) {
		r.Items = r.Items[:s.itemLimit(in.MaxItems)]
		r.Truncated = true
	}
	r.Items, r.Truncated = s.filterCalls(p, r.Items, r.Truncated)
	return bounded(s.limits.MaxResponseBytes, r)
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
	return bounded(s.limits.MaxResponseBytes, out)
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
	return bounded(s.limits.MaxResponseBytes, ProjectStatusResult{ID: p.ID, Name: p.Name, EngineVersion: p.Engine.Version, Ready: p.Generation.CompilationDatabase != "", LastFingerprint: p.Generation.LastFingerprint})
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
	file, err := s.openFile(path)
	if err != nil {
		return ReadSymbolSourceResult{}, mapError(err)
	}
	limit := s.limits.MaxSourceBytes
	if in.MaxBytes > 0 && in.MaxBytes < limit {
		limit = in.MaxBytes
	}
	// Close unblocks a potentially stalled Read when the request context ends.
	stopWatch := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = file.Close()
		case <-stopWatch:
		}
	}()
	defer func() { close(stopWatch); <-watchDone; _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	// A fixed scanner ceiling bounds a single hostile line independently of the
	// caller's output budget. Long lines are reported as a bounded read error.
	scanner.Buffer(make([]byte, 4096), 64*1024)
	line := 0
	end := in.StartLine - 1
	var b strings.Builder
	truncated := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return ReadSymbolSourceResult{}, mapError(err)
		}
		line++
		if line < in.StartLine {
			continue
		}
		if line > in.EndLine {
			break
		}
		part := scanner.Text()
		need := len(part)
		if end >= in.StartLine {
			need++
		}
		if b.Len()+need > limit {
			remaining := limit - b.Len()
			if end >= in.StartLine && remaining > 0 {
				b.WriteByte('\n')
				remaining--
			}
			if remaining > 0 {
				b.WriteString(part[:min(remaining, len(part))])
			}
			truncated = true
			break
		}
		if end >= in.StartLine {
			b.WriteByte('\n')
		}
		b.WriteString(part)
		end = line
		if line == in.EndLine {
			return bounded(s.limits.MaxResponseBytes, ReadSymbolSourceResult{Path: path, StartLine: in.StartLine, EndLine: end, Text: b.String(), Truncated: truncated})
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ReadSymbolSourceResult{}, mapError(ctx.Err())
		}
		return ReadSymbolSourceResult{}, mapError(err)
	}
	if line < in.StartLine {
		return ReadSymbolSourceResult{}, errors.New("start_line is outside the file")
	}
	return bounded(s.limits.MaxResponseBytes, ReadSymbolSourceResult{Path: path, StartLine: in.StartLine, EndLine: end, Text: b.String(), Truncated: truncated})
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
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("request timed out: %w", err)
	}
	if errors.Is(err, ErrProjectRequired) || errors.Is(err, ErrProjectNotFound) || errors.Is(err, ErrPathForbidden) || errors.Is(err, ErrLimitExceeded) {
		return err
	}
	return errors.New("code intelligence request failed")
}

func bounded[T any](limit int, value T) (T, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return value, err
	}
	if len(b) > limit {
		return value, ErrLimitExceeded
	}
	return value, nil
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

// fileURIPath decodes only local file URIs. Hostnames other than localhost are
// rejected to avoid treating remote UNC shares as trusted project files.
func fileURIPath(raw, goos string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return "", ErrPathForbidden
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", ErrPathForbidden
	}
	p, err := url.PathUnescape(u.EscapedPath())
	if err != nil || p == "" {
		return "", ErrPathForbidden
	}
	if goos == "windows" {
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
		p = strings.ReplaceAll(p, "/", "\\")
	}
	return p, nil
}
func runtimeGOOS() string { return runtime.GOOS }

func (s *Server) allowed(p registry.Project, uri string) bool {
	path, err := fileURIPath(uri, runtimeGOOS())
	if err != nil {
		return false
	}
	_, err = safePath(path, filepath.Dir(p.UProject), p.Engine.Root)
	return err == nil
}
func (s *Server) filterSymbols(p registry.Project, in []lsp.Symbol, truncated bool) ([]lsp.Symbol, bool) {
	out := in[:0]
	for _, v := range in {
		if v.Location.URI == "" || s.allowed(p, v.Location.URI) {
			out = append(out, v)
		} else {
			truncated = true
		}
	}
	return out, truncated
}
func (s *Server) filterLocations(p registry.Project, in []lsp.Location, truncated bool) ([]lsp.Location, bool) {
	out := in[:0]
	for _, v := range in {
		if s.allowed(p, v.URI) {
			out = append(out, v)
		} else {
			truncated = true
		}
	}
	return out, truncated
}
func (s *Server) filterCalls(p registry.Project, in []lsp.CallHierarchyItem, truncated bool) ([]lsp.CallHierarchyItem, bool) {
	out := in[:0]
	for _, v := range in {
		if s.allowed(p, v.URI) {
			out = append(out, v)
		} else {
			truncated = true
		}
	}
	return out, truncated
}
func flattenSymbols(in []lsp.DocumentSymbol) []DocumentSymbol {
	out := make([]DocumentSymbol, 0)
	var walk func([]lsp.DocumentSymbol)
	walk = func(xs []lsp.DocumentSymbol) {
		for _, v := range xs {
			out = append(out, DocumentSymbol{Name: v.Name, Kind: v.Kind, Range: v.Range, SelectionRange: v.SelectionRange})
			walk(v.Children)
		}
	}
	walk(in)
	return out
}
func trimSymbols(in []lsp.Symbol, max int, tr bool) ([]lsp.Symbol, bool) {
	out := make([]lsp.Symbol, 0, len(in))
	used := 0
	for _, v := range in {
		n := len(v.Name) + len(v.ContainerName) + 128
		if used+n > max {
			tr = true
			break
		}
		out = append(out, v)
		used += n
	}
	return out, tr
}
func trimLocations(in []lsp.Location, max int, tr bool) ([]lsp.Location, bool) {
	out := make([]lsp.Location, 0, len(in))
	used := 0
	for _, v := range in {
		n := len(v.URI) + 96
		if used+n > max {
			tr = true
			break
		}
		out = append(out, v)
		used += n
	}
	return out, tr
}
func trimDocumentSymbols(in []DocumentSymbol, max int, tr bool) ([]DocumentSymbol, bool) {
	out := make([]DocumentSymbol, 0, len(in))
	used := 0
	for _, v := range in {
		n := len(v.Name) + 96
		if used+n > max {
			tr = true
			break
		}
		out = append(out, v)
		used += n
	}
	return out, tr
}
