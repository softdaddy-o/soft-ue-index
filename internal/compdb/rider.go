package compdb

// Rider project metadata is intentionally read-only input.  Rider has already
// asked UBT to calculate the target, so reusing it avoids touching an installed
// engine or the project's Intermediate directory.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type RiderInput struct {
	ProjectRoot, EngineRoot, Target, TargetFile, StagingDir, ResponseDir, ClangCL string
	EngineScopeFull                                                               bool
}

type RiderResult struct {
	ProjectTranslationUnits, EngineTranslationUnits int
	Fingerprint                                     string
}

type riderTarget struct {
	Name, TargetFile string
	ToolchainInfo    struct {
		CompilerPath string `json:"CompilerPath"`
	} `json:"ToolchainInfo"`
	Modules                 map[string]riderModule `json:"Modules"`
	EnvironmentIncludePaths json.RawMessage        `json:"EnvironmentIncludePaths"`
	EnvironmentDefinitions  json.RawMessage        `json:"EnvironmentDefinitions"`
}
type riderModule struct {
	Directory              string          `json:"Directory"`
	Rules                  string          `json:"Rules"`
	GeneratedCodeDirectory string          `json:"GeneratedCodeDirectory"`
	ToolchainInfo          json.RawMessage `json:"ToolchainInfo"`
	riderRules
}
type riderRules struct {
	PublicIncludePaths       []string `json:"PublicIncludePaths"`
	PrivateIncludePaths      []string `json:"PrivateIncludePaths"`
	IncludePaths             []string `json:"IncludePaths"`
	Definitions              []string `json:"Definitions"`
	PublicDefinitions        []string `json:"PublicDefinitions"`
	PrivateDefinitions       []string `json:"PrivateDefinitions"`
	ForceIncludeFiles        []string `json:"ForceIncludeFiles"`
	PublicSystemIncludePaths []string `json:"PublicSystemIncludePaths"`
	SystemIncludePaths       []string `json:"SystemIncludePaths"`
	InternalIncludePaths     []string `json:"InternalIncludePaths"`
	LegacyPublicIncludePaths []string `json:"LegacyPublicIncludePaths"`
	ProjectDefinitions       []string `json:"ProjectDefinitions"`
	ApiDefinitions           []string `json:"ApiDefinitions"`
}

// UnmarshalJSON accepts both Rider forms seen in the wild: older metadata puts
// compile settings under Rules, while current UE metadata keeps Rules as the
// module's .Build.cs path and puts settings on the module itself.
func (m *riderModule) UnmarshalJSON(data []byte) error {
	type wire struct {
		Directory              string          `json:"Directory"`
		Rules                  json.RawMessage `json:"Rules"`
		GeneratedCodeDirectory string          `json:"GeneratedCodeDirectory"`
		ToolchainInfo          json.RawMessage `json:"ToolchainInfo"`
		riderRules
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.Directory, m.GeneratedCodeDirectory, m.ToolchainInfo, m.riderRules = w.Directory, w.GeneratedCodeDirectory, w.ToolchainInfo, w.riderRules
	if len(w.Rules) == 0 || string(w.Rules) == "null" {
		return nil
	}
	if err := json.Unmarshal(w.Rules, &m.Rules); err == nil {
		return nil
	}
	var nested riderRules
	if err := json.Unmarshal(w.Rules, &nested); err != nil {
		return fmt.Errorf("decode Rider module Rules: %w", err)
	}
	m.riderRules = mergeRiderRules(nested, m.riderRules)
	return nil
}

func mergeRiderRules(a, b riderRules) riderRules {
	return riderRules{
		PublicIncludePaths: unique(append(a.PublicIncludePaths, b.PublicIncludePaths...)), PrivateIncludePaths: unique(append(a.PrivateIncludePaths, b.PrivateIncludePaths...)), IncludePaths: unique(append(a.IncludePaths, b.IncludePaths...)),
		Definitions: unique(append(a.Definitions, b.Definitions...)), PublicDefinitions: unique(append(a.PublicDefinitions, b.PublicDefinitions...)), PrivateDefinitions: unique(append(a.PrivateDefinitions, b.PrivateDefinitions...)),
		ForceIncludeFiles: unique(append(a.ForceIncludeFiles, b.ForceIncludeFiles...)), PublicSystemIncludePaths: unique(append(a.PublicSystemIncludePaths, b.PublicSystemIncludePaths...)), SystemIncludePaths: unique(append(a.SystemIncludePaths, b.SystemIncludePaths...)), InternalIncludePaths: unique(append(a.InternalIncludePaths, b.InternalIncludePaths...)),
		LegacyPublicIncludePaths: unique(append(a.LegacyPublicIncludePaths, b.LegacyPublicIncludePaths...)), ProjectDefinitions: unique(append(a.ProjectDefinitions, b.ProjectDefinitions...)), ApiDefinitions: unique(append(a.ApiDefinitions, b.ApiDefinitions...)),
	}
}

func RiderMetadataDir(projectRoot string) string {
	return filepath.Join(projectRoot, "Intermediate", "ProjectFiles", ".Rider", "Win64", "Development", "Editor")
}

func RiderMetadataAvailable(projectRoot, target, targetFile string) bool {
	_, err := selectRiderTarget(RiderMetadataDir(projectRoot), target, targetFile, projectRoot)
	return err == nil
}

// RiderMetadataStale is retained for callers that cannot identify a target.
// New indexing code must use RiderMetadataStaleFor so unrelated Rider JSON does
// not influence whether a selected target can be reused.
func RiderMetadataStale(projectRoot string) bool {
	files, _ := filepath.Glob(filepath.Join(RiderMetadataDir(projectRoot), "*.json"))
	if len(files) == 0 {
		return true
	}
	var oldest fs.FileInfo
	for _, file := range files {
		if info, err := os.Stat(file); err == nil && (oldest == nil || info.ModTime().Before(oldest.ModTime())) {
			oldest = info
		}
	}
	if oldest == nil {
		return true
	}
	stale := false
	_ = filepath.WalkDir(projectRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".Build.cs") || strings.HasSuffix(name, ".Target.cs") || strings.EqualFold(filepath.Ext(name), ".uplugin") || strings.EqualFold(filepath.Ext(name), ".uproject") {
			if info, e := entry.Info(); e == nil && info.ModTime().After(oldest.ModTime()) {
				stale = true
			}
		}
		return nil
	})
	return stale
}

// RiderMetadataStaleFor compares only metadata selected for this indexing
// request against its target, module rule, and applicable project descriptors.
func RiderMetadataStaleFor(projectRoot, engineRoot, target, targetFile string, engineScopeFull bool) (bool, error) {
	project, err := canonical(projectRoot)
	if err != nil {
		return false, err
	}
	engine, err := canonical(engineRoot)
	if err != nil {
		return false, err
	}
	selected, err := selectRiderTarget(RiderMetadataDir(project), target, targetFile, project)
	if err != nil {
		return false, err
	}
	all := []riderSelectedTarget{selected}
	if engineScopeFull {
		editor, err := selectRiderTarget(RiderMetadataDir(project), "UnrealEditor", "", engine)
		if err != nil {
			return false, fmt.Errorf("rider_metadata_missing: full engine scope requires UnrealEditor metadata: %w", err)
		}
		all = append(all, editor)
	}
	oldest := timeForSelections(all)
	if oldest.IsZero() {
		return true, nil
	}
	for _, selected := range all {
		if missingOrNewer(selected.TargetFile, oldest) {
			return true, nil
		}
		for _, module := range selected.Modules {
			if module.Rules == "" {
				continue
			}
			rules := module.Rules
			if !filepath.IsAbs(rules) {
				rules = filepath.Join(module.Directory, rules)
			}
			path, e := canonical(rules)
			if e != nil || (!within(path, project) && !within(path, engine)) || missingOrNewer(path, oldest) {
				return true, nil
			}
		}
	}
	// Project rules and descriptors can introduce modules that are absent from
	// the selected metadata, so compare the bounded project source/plugin trees.
	if projectInputsNewer(project, oldest) {
		return true, nil
	}
	// Engine-wide scans are intentionally avoided; only plugin descriptors on
	// selected module ancestor paths are applicable.
	for _, descriptor := range relevantDescriptors(project, engine, all) {
		if missingOrNewer(descriptor, oldest) {
			return true, nil
		}
	}
	return false, nil
}

func timeForSelections(selected []riderSelectedTarget) (oldest time.Time) {
	for _, s := range selected {
		info, err := os.Stat(s.MetadataPath)
		if err != nil {
			return time.Time{}
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	return oldest
}

func newer(path string, than time.Time) bool {
	info, err := os.Stat(path)
	return err == nil && info.ModTime().After(than)
}

func missingOrNewer(path string, than time.Time) bool {
	info, err := os.Stat(path)
	return err != nil || info.ModTime().After(than)
}

func projectInputsNewer(project string, than time.Time) bool {
	newerInput := false
	for _, root := range []string{filepath.Join(project, "Source"), filepath.Join(project, "Plugins")} {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				switch entry.Name() {
				case "Binaries", "Intermediate", "Saved", ".git":
					return filepath.SkipDir
				}
				return nil
			}
			name := entry.Name()
			if (strings.HasSuffix(name, ".Build.cs") || strings.HasSuffix(name, ".Target.cs") || strings.EqualFold(filepath.Ext(name), ".uplugin")) && newer(path, than) {
				newerInput = true
				return fs.SkipAll
			}
			return nil
		})
		if newerInput {
			return true
		}
	}
	return false
}

func relevantDescriptors(project, engine string, targets []riderSelectedTarget) []string {
	set := map[string]bool{}
	uprojects, _ := filepath.Glob(filepath.Join(project, "*.uproject"))
	if len(uprojects) == 0 {
		set[filepath.Join(project, filepath.Base(project)+".uproject")] = true
	}
	for _, path := range uprojects {
		set[path] = true
	}
	for _, target := range targets {
		for _, module := range target.Modules {
			dir, err := canonical(module.Directory)
			if err != nil {
				continue
			}
			boundary := ""
			if within(dir, project) {
				boundary = project
			} else if within(dir, engine) {
				boundary = engine
			} else {
				continue
			}
			pluginRoot := ""
			for current := dir; ; current = filepath.Dir(current) {
				plugins, _ := filepath.Glob(filepath.Join(current, "*.uplugin"))
				for _, path := range plugins {
					set[path] = true
				}
				if strings.EqualFold(filepath.Base(current), "Source") {
					candidate := filepath.Dir(current)
					if within(candidate, filepath.Join(project, "Plugins")) || within(candidate, filepath.Join(engine, "Engine", "Plugins")) {
						pluginRoot = candidate
					}
				}
				if strings.EqualFold(current, boundary) {
					break
				}
			}
			if pluginRoot != "" {
				descriptors, _ := filepath.Glob(filepath.Join(pluginRoot, "*.uplugin"))
				if len(descriptors) == 0 {
					set[filepath.Join(pluginRoot, filepath.Base(pluginRoot)+".uplugin")] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func SynthesizeRider(in RiderInput) (RiderResult, error) {
	project, err := canonical(in.ProjectRoot)
	if err != nil {
		return RiderResult{}, err
	}
	engine, err := canonical(in.EngineRoot)
	if err != nil {
		return RiderResult{}, err
	}
	target, err := selectRiderTarget(RiderMetadataDir(project), in.Target, in.TargetFile, project)
	if err != nil {
		return RiderResult{}, err
	}
	compiler := target.ToolchainInfo.CompilerPath
	if in.ClangCL != "" {
		compiler = in.ClangCL
	}
	if compiler == "" {
		return RiderResult{}, fmt.Errorf("rider metadata has no clang-cl compiler")
	}
	modules := make(map[string]riderModule, len(target.Modules))
	for n, m := range target.Modules {
		modules[n] = m
	}
	if in.EngineScopeFull {
		editor, e := selectRiderTarget(RiderMetadataDir(project), "UnrealEditor", "", engine)
		if e != nil {
			return RiderResult{}, fmt.Errorf("rider_metadata_missing: full engine scope requires UnrealEditor metadata: %w", e)
		}
		for n, m := range editor.Modules {
			if _, ok := modules[n]; !ok {
				modules[n] = m
			}
		}
	}
	var all []riderResolvedModule
	for name, module := range modules {
		d, e := canonical(module.Directory)
		if e != nil || (!within(d, project) && !within(d, engine)) {
			return RiderResult{}, fmt.Errorf("rider module %q escapes project and engine roots", name)
		}
		generated := ""
		if module.GeneratedCodeDirectory != "" {
			generated = module.GeneratedCodeDirectory
			if !filepath.IsAbs(generated) {
				generated = filepath.Join(d, generated)
			}
			generated, e = canonicalIncludingMissing(generated)
			info, statErr := os.Stat(generated)
			invalidType := statErr == nil && !info.IsDir()
			if e != nil || invalidType || (statErr != nil && !errors.Is(statErr, os.ErrNotExist)) || (!within(generated, project) && !within(generated, engine)) {
				return RiderResult{}, fmt.Errorf("rider module %q GeneratedCodeDirectory escapes project and engine roots", name)
			}
		}
		all = append(all, riderResolvedModule{name: name, dir: d, generatedDir: generated, rules: module.riderRules})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].dir != all[j].dir {
			return all[i].dir < all[j].dir
		}
		return all[i].name < all[j].name
	})
	moduleIndex := newRiderModuleIndex(all)
	allAPIDefinitions := make([]string, 0)
	for _, module := range all {
		allAPIDefinitions = append(allAPIDefinitions, module.rules.ApiDefinitions...)
	}
	sort.Strings(allAPIDefinitions)
	rootDefinitions := unique(append(stringsFromJSON(target.EnvironmentDefinitions), allAPIDefinitions...))
	if err := os.MkdirAll(in.StagingDir, 0700); err != nil {
		return RiderResult{}, err
	}
	entries := make([]Entry, 0)
	seen := map[string]bool{}
	for _, root := range riderModuleWalkRoots(all) {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return nil
			}
			if d.IsDir() {
				n := d.Name()
				if n == "Intermediate" || n == "Binaries" || n == "Saved" || n == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".c" && ext != ".cc" && ext != ".cpp" && ext != ".cxx" {
				return nil
			}
			source, e := canonical(path)
			if e != nil || (!within(source, project) && !within(source, engine)) {
				return nil
			}
			mod := moduleIndex.lookup(source)
			if mod == nil {
				return nil
			}
			content := response(*mod, compiler, stringsFromJSON(target.EnvironmentIncludePaths), rootDefinitions)
			responseDir := in.ResponseDir
			if responseDir == "" {
				responseDir = in.StagingDir
			}
			if e := os.MkdirAll(responseDir, 0700); e != nil {
				return e
			}
			sum := sha256.Sum256([]byte(content))
			rsp := filepath.Join(responseDir, hex.EncodeToString(sum[:])+".rsp")
			if _, e := os.Stat(rsp); os.IsNotExist(e) {
				if e = writeResponseAtomic(rsp, []byte(content)); e != nil {
					return e
				}
			}
			key := strings.ToLower(source)
			if !seen[key] {
				seen[key] = true
				entries = append(entries, Entry{Directory: mod.dir, File: source, Arguments: []string{compiler, "@" + rsp, source}})
			}
			return nil
		})
		if err != nil {
			return RiderResult{}, err
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		leftClass, rightClass := translationUnitClass(entries[i].File, project, engine), translationUnitClass(entries[j].File, project, engine)
		if leftClass != rightClass {
			return leftClass < rightClass
		}
		left, right := strings.ToLower(entries[i].File), strings.ToLower(entries[j].File)
		if left != right {
			return left < right
		}
		return entries[i].File < entries[j].File
	})
	if err := WriteDatabase(filepath.Join(in.StagingDir, DatabaseName), entries); err != nil {
		return RiderResult{}, err
	}
	result := RiderResult{}
	h := sha256.New()
	for _, e := range entries {
		if within(e.File, project) {
			result.ProjectTranslationUnits++
		} else if within(e.File, engine) {
			result.EngineTranslationUnits++
		}
		h.Write([]byte(e.File + "\x00" + strings.Join(e.Arguments, "\x00") + "\n"))
	}
	result.Fingerprint = hex.EncodeToString(h.Sum(nil))
	if result.ProjectTranslationUnits == 0 || result.EngineTranslationUnits == 0 {
		return RiderResult{}, fmt.Errorf("rider metadata has insufficient coverage: project=%d engine=%d", result.ProjectTranslationUnits, result.EngineTranslationUnits)
	}
	return result, nil
}

// canonicalIncludingMissing resolves the nearest existing ancestor before
// restoring missing suffixes. This permits not-yet-generated UHT directories
// without allowing an existing junction or symlink ancestor to escape a root.
func canonicalIncludingMissing(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	missing := make([]string, 0)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func translationUnitClass(file, project, engine string) int {
	if within(file, project) {
		return 0
	}
	if within(file, engine) {
		return 1
	}
	return 2
}

type riderResolvedModule struct {
	name, dir, generatedDir string
	rules                   riderRules
}

type riderModuleIndex struct {
	modules []riderResolvedModule
	byDir   map[string]int
}

func newRiderModuleIndex(modules []riderResolvedModule) riderModuleIndex {
	index := riderModuleIndex{modules: append([]riderResolvedModule(nil), modules...), byDir: make(map[string]int, len(modules))}
	for i := range index.modules {
		key := riderPathKey(index.modules[i].dir)
		if _, exists := index.byDir[key]; !exists {
			index.byDir[key] = i
		}
	}
	return index
}

func (index riderModuleIndex) lookup(file string) *riderResolvedModule {
	module, _ := index.lookupWithProbes(file)
	return module
}

func (index riderModuleIndex) lookupWithProbes(file string) (*riderResolvedModule, int) {
	dir := filepath.Dir(filepath.Clean(file))
	for probes := 1; ; probes++ {
		if i, exists := index.byDir[riderPathKey(dir)]; exists {
			return &index.modules[i], probes
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, probes
		}
		dir = parent
	}
}

func riderPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func riderModuleWalkRoots(modules []riderResolvedModule) []string {
	dirs := make([]string, 0, len(modules))
	for _, module := range modules {
		dirs = append(dirs, module.dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if len(dirs[i]) != len(dirs[j]) {
			return len(dirs[i]) < len(dirs[j])
		}
		return dirs[i] < dirs[j]
	})
	roots := make([]string, 0, len(dirs))
	rootKeys := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		covered := false
		for current := dir; ; current = filepath.Dir(current) {
			if rootKeys[riderPathKey(current)] {
				covered = true
				break
			}
			if parent := filepath.Dir(current); parent == current {
				break
			}
		}
		if !covered {
			roots = append(roots, dir)
			rootKeys[riderPathKey(dir)] = true
		}
	}
	return roots
}

type riderSelectedTarget struct {
	riderTarget
	MetadataPath string
}

func selectRiderTarget(dir, name, targetFile, projectRoot string) (riderSelectedTarget, error) {
	files, e := filepath.Glob(filepath.Join(dir, "*.json"))
	if e != nil {
		return riderSelectedTarget{}, e
	}
	wanted, _ := canonical(targetFile)
	var matches []riderSelectedTarget
	for _, f := range files {
		b, e := os.ReadFile(f)
		if e != nil {
			continue
		}
		var r riderTarget
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		tf, _ := canonical(r.TargetFile)
		safe := within(tf, projectRoot) && strings.EqualFold(filepath.Base(tf), name+".Target.cs")
		if strings.EqualFold(r.Name, name) && safe && (targetFile == "" || strings.EqualFold(tf, wanted)) {
			metadata, e := canonical(f)
			if e != nil {
				continue
			}
			matches = append(matches, riderSelectedTarget{riderTarget: r, MetadataPath: metadata})
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return riderSelectedTarget{}, fmt.Errorf("rider_metadata_ambiguous: target %q", name)
	}
	return riderSelectedTarget{}, fmt.Errorf("rider_metadata_missing: target %q", name)
}
func response(m riderResolvedModule, compiler string, rootIncludes, rootDefinitions []string) string {
	_ = compiler
	inc := unique(append(append(append(append(append(append(append(append([]string{}, rootIncludes...), m.rules.PublicIncludePaths...), m.rules.PrivateIncludePaths...), m.rules.IncludePaths...), m.rules.PublicSystemIncludePaths...), m.rules.SystemIncludePaths...), m.rules.InternalIncludePaths...), m.rules.LegacyPublicIncludePaths...))
	if m.generatedDir != "" {
		inc = unique(append(inc, m.generatedDir))
	}
	defs := unique(append(append(append(append(append(append([]string{}, rootDefinitions...), m.rules.Definitions...), m.rules.PublicDefinitions...), m.rules.PrivateDefinitions...), m.rules.ProjectDefinitions...), m.rules.ApiDefinitions...))
	var a = []string{"/nologo", "/TP", "/std:c++20", "--target=x86_64-pc-windows-msvc", "/utf-8", "/Zc:__cplusplus", "/permissive-"}
	for _, x := range inc {
		if !filepath.IsAbs(x) {
			x = filepath.Join(m.dir, x)
		}
		a = append(a, "/I"+quoteRsp(x))
	}
	for _, x := range defs {
		a = append(a, "/D"+quoteRsp(x))
	}
	for _, x := range m.rules.ForceIncludeFiles {
		if !filepath.IsAbs(x) {
			x = filepath.Join(m.dir, x)
		}
		a = append(a, "/FI"+quoteRsp(x))
	}
	return strings.Join(a, "\r\n") + "\r\n"
}
func stringsFromJSON(raw json.RawMessage) []string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var out []string
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				visit(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for key := range x {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				visit(x[key])
			}
		}
	}
	visit(value)
	return unique(out)
}
func unique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, x := range in {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
func writeResponseAtomic(path string, data []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		if _, e := os.Stat(path); e == nil {
			return nil
		}
		return err
	}
	return nil
}
func quoteRsp(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
	}
	return s
}
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}
