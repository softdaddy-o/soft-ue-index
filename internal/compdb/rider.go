package compdb

// Rider project metadata is intentionally read-only input.  Rider has already
// asked UBT to calculate the target, so reusing it avoids touching an installed
// engine or the project's Intermediate directory.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type RiderInput struct {
	ProjectRoot, EngineRoot, Target, TargetFile, StagingDir, ClangCL string
	EngineScopeFull                                                  bool
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
	Modules map[string]riderModule `json:"Modules"`
}
type riderModule struct {
	Directory string     `json:"Directory"`
	Rules     riderRules `json:"Rules"`
}
type riderRules struct {
	PublicIncludePaths  []string `json:"PublicIncludePaths"`
	PrivateIncludePaths []string `json:"PrivateIncludePaths"`
	IncludePaths        []string `json:"IncludePaths"`
	Definitions         []string `json:"Definitions"`
	PublicDefinitions   []string `json:"PublicDefinitions"`
	PrivateDefinitions  []string `json:"PrivateDefinitions"`
	ForceIncludeFiles   []string `json:"ForceIncludeFiles"`
}

func RiderMetadataDir(projectRoot string) string {
	return filepath.Join(projectRoot, "Intermediate", "ProjectFiles", ".Rider", "Win64", "Development", "Editor")
}

func RiderMetadataAvailable(projectRoot, target, targetFile string) bool {
	_, err := selectRiderTarget(RiderMetadataDir(projectRoot), target, targetFile)
	return err == nil
}

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

func SynthesizeRider(in RiderInput) (RiderResult, error) {
	project, err := canonical(in.ProjectRoot)
	if err != nil {
		return RiderResult{}, err
	}
	engine, err := canonical(in.EngineRoot)
	if err != nil {
		return RiderResult{}, err
	}
	target, err := selectRiderTarget(RiderMetadataDir(project), in.Target, in.TargetFile)
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
	modules := target.Modules
	if in.EngineScopeFull {
		if editor, e := selectRiderTarget(RiderMetadataDir(project), "UnrealEditor", ""); e == nil {
			for n, m := range editor.Modules {
				if _, ok := modules[n]; !ok {
					modules[n] = m
				}
			}
		}
	}
	var all []riderResolvedModule
	for name, module := range modules {
		d, e := canonical(module.Directory)
		if e != nil || (!within(d, project) && !within(d, engine)) {
			return RiderResult{}, fmt.Errorf("rider module %q escapes project and engine roots", name)
		}
		all = append(all, riderResolvedModule{name, d, module.Rules})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].dir < all[j].dir })
	if err := os.MkdirAll(in.StagingDir, 0700); err != nil {
		return RiderResult{}, err
	}
	entries := make([]Entry, 0)
	seen := map[string]bool{}
	for _, root := range []string{project, engine} {
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
			mod := deepestModule(source, all)
			if mod == nil {
				return nil
			}
			rsp := filepath.Join(in.StagingDir, "rider-"+safeName(mod.name)+".rsp")
			if _, e := os.Stat(rsp); os.IsNotExist(e) {
				if e = os.WriteFile(rsp, []byte(response(*mod, compiler)), 0600); e != nil {
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].File < entries[j].File })
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

type riderResolvedModule struct {
	name, dir string
	rules     riderRules
}

func deepestModule(file string, mods []riderResolvedModule) *riderResolvedModule {
	var best *riderResolvedModule
	for i := range mods {
		if within(file, mods[i].dir) && (best == nil || len(mods[i].dir) > len(best.dir)) {
			best = &mods[i]
		}
	}
	return best
}
func selectRiderTarget(dir, name, targetFile string) (riderTarget, error) {
	files, e := filepath.Glob(filepath.Join(dir, "*.json"))
	if e != nil {
		return riderTarget{}, e
	}
	wanted, _ := canonical(targetFile)
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
		if strings.EqualFold(r.Name, name) && (targetFile == "" || strings.EqualFold(tf, wanted)) {
			return r, nil
		}
	}
	return riderTarget{}, fmt.Errorf("rider_metadata_missing: target %q", name)
}
func response(m riderResolvedModule, compiler string) string {
	_ = compiler
	inc := append(append(append([]string{}, m.rules.PublicIncludePaths...), m.rules.PrivateIncludePaths...), m.rules.IncludePaths...)
	defs := append(append(append([]string{}, m.rules.Definitions...), m.rules.PublicDefinitions...), m.rules.PrivateDefinitions...)
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
