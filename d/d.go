package d

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

const (
	langName          = "d"
	importsPrivateKey = "_gazelle_d_imports"
	modulesPrivateKey = "_gazelle_d_modules"
)

type dLang struct {
	language.BaseLang
}

var _ language.Language = (*dLang)(nil)

// NewLanguage returns the Gazelle language extension for rules_d.
func NewLanguage() language.Language {
	return &dLang{}
}

func (*dLang) Name() string { return langName }

func (*dLang) Kinds() map[string]rule.KindInfo {
	return map[string]rule.KindInfo{
		"d_library": {
			NonEmptyAttrs:  map[string]bool{"srcs": true},
			MergeableAttrs: map[string]bool{"srcs": true, "deps": true, "dopts": true, "imports": true, "linkopts": true, "string_imports": true, "versions": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
		"d_proto_library": {
			NonEmptyAttrs:  map[string]bool{"deps": true},
			MergeableAttrs: map[string]bool{"deps": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
		"d_binary": {
			NonEmptyAttrs:  map[string]bool{"srcs": true},
			MergeableAttrs: map[string]bool{"srcs": true, "deps": true, "data": true, "dopts": true, "imports": true, "linkopts": true, "string_imports": true, "versions": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
		"d_test": {
			NonEmptyAttrs:  map[string]bool{"srcs": true},
			MergeableAttrs: map[string]bool{"srcs": true, "deps": true, "data": true, "dopts": true, "imports": true, "linkopts": true, "string_imports": true, "versions": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
		"dub_lock_dependencies": {
			NonEmptyAttrs:  map[string]bool{"srcs": true},
			MergeableAttrs: map[string]bool{"srcs": true, "dub_selections_lock": true, "tags": true},
		},
		"exports_files": {},
	}
}

func (*dLang) Loads() []rule.LoadInfo {
	return dLoads("@rules_d")
}

func (*dLang) ApparentLoads(moduleToApparentName func(string) string) []rule.LoadInfo {
	repoName := moduleToApparentName("rules_d")
	if repoName == "" {
		repoName = "rules_d"
	}
	return dLoads("@" + repoName)
}

func dLoads(repoName string) []rule.LoadInfo {
	return []rule.LoadInfo{
		{
			Name:    repoName + "//d:defs.bzl",
			Symbols: []string{"d_binary", "d_library", "d_proto_library", "d_test"},
		},
		{
			Name:    repoName + "//dub:defs.bzl",
			Symbols: []string{"dub_lock_dependencies"},
		},
	}
}

func (*dLang) GenerateRules(args language.GenerateArgs) language.GenerateResult {
	manifest := parseDubManifest(args.Dir, args.RegularFiles)
	if manifest == nil && isUnderParentDubSource(args) {
		return language.GenerateResult{}
	}

	localDubLockSrcs := localDubLockSources(args.RegularFiles)
	files := collectDFiles(args.Rel, args.RegularFiles)
	if manifest != nil {
		files = manifest.collectFiles(args.Dir, args.Rel, args.RegularFiles)
	}
	var dubLockSrcs []string
	if args.Rel == "" {
		dubLockSrcs = collectDubLockSources(args.Dir)
	}
	if len(files) == 0 && len(args.OtherGen) == 0 && len(dubLockSrcs) == 0 && len(localDubLockSrcs) == 0 {
		return language.GenerateResult{}
	}

	libSrcs := make([]string, 0, len(files))
	var bins []dFile
	var tests []dFile
	var allImports []string
	var allModules []string

	for _, f := range files {
		info := parseDFile(args.Dir, args.Rel, f)
		allImports = append(allImports, info.imports...)
		allModules = append(allModules, info.module)
		if info.hasMain {
			bins = append(bins, info)
			continue
		}
		libSrcs = append(libSrcs, info.name)
		if info.hasUnittest {
			tests = append(tests, info)
		}
	}

	var gen []*rule.Rule
	var imports []interface{}
	if args.Rel != "" && len(localDubLockSrcs) > 0 {
		gen = append(gen, newExportsFilesRule(localDubLockSrcs))
		imports = append(imports, []string(nil))
	}
	gen, imports = appendProtoRules(gen, imports, args)
	libName := libraryName(args.Rel)
	if manifest != nil && manifest.isLibrary() && manifest.ruleName() != "" {
		libName = manifest.ruleName()
	}
	if len(libSrcs) > 0 {
		r := newDRule("d_library", libName, libSrcs, args)
		applyDubAttrs(r, manifest, args.Dir)
		addDubDependencyDeps(r, manifest)
		r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(allModules))
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(allImports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(allImports))
	}

	for _, f := range bins {
		r := newDRule("d_binary", binaryName(f.name), []string{f.name}, args)
		if manifest != nil && manifest.isExecutable() && manifest.ruleName() != "" {
			r.SetName(manifest.ruleName())
		}
		applyDubAttrs(r, manifest, args.Dir)
		if len(libSrcs) > 0 {
			r.SetAttr("deps", []string{":" + libName})
		}
		addDubDependencyDeps(r, manifest)
		r.SetPrivateAttr(modulesPrivateKey, []string{f.module})
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(f.imports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(f.imports))
	}

	if len(tests) > 0 {
		srcs := make([]string, 0, len(tests))
		testImports := make([]string, 0)
		testModules := make([]string, 0, len(tests))
		for _, f := range tests {
			srcs = append(srcs, f.name)
			testImports = append(testImports, f.imports...)
			testModules = append(testModules, f.module)
		}
		r := newDRule("d_test", testName(args.Rel), srcs, args)
		applyDubAttrs(r, manifest, args.Dir)
		if len(libSrcs) > 0 {
			r.SetAttr("deps", []string{":" + libName})
		}
		addDubDependencyDeps(r, manifest)
		r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(testModules))
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(testImports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(testImports))
	}

	if len(dubLockSrcs) > 0 {
		gen = append(gen, newDubLockDependenciesRule(dubLockSrcs))
		imports = append(imports, []string(nil))
	}

	return language.GenerateResult{
		Gen:     gen,
		Imports: imports,
	}
}

func appendProtoRules(gen []*rule.Rule, imports []interface{}, args language.GenerateArgs) ([]*rule.Rule, []interface{}) {
	for _, proto := range args.OtherGen {
		if proto.Kind() != "proto_library" || proto.Name() == "" {
			continue
		}
		r := rule.NewRule("d_proto_library", dProtoName(proto.Name()))
		r.SetAttr("deps", []string{":" + proto.Name()})
		if args.File == nil || !args.File.HasDefaultVisibility() {
			r.SetAttr("visibility", []string{"//visibility:public"})
		}
		gen = append(gen, r)
		imports = append(imports, []string(nil))
	}
	return gen, imports
}

func (*dLang) Imports(c *config.Config, r *rule.Rule, f *rule.File) []resolve.ImportSpec {
	if !isDRule(r.Kind()) {
		return nil
	}

	modules, _ := r.PrivateAttr(modulesPrivateKey).([]string)
	if len(modules) == 0 {
		modules = modulesFromRule(f.Pkg, r)
	}
	if len(modules) == 0 {
		return []resolve.ImportSpec{}
	}

	imports := make([]resolve.ImportSpec, 0, len(modules))
	for _, mod := range uniqueSorted(modules) {
		imports = append(imports, resolve.ImportSpec{Lang: langName, Imp: mod})
	}
	return imports
}

func (*dLang) Embeds(r *rule.Rule, from label.Label) []label.Label {
	return nil
}

func (*dLang) Resolve(c *config.Config, ix *resolve.RuleIndex, rc *repo.RemoteCache, r *rule.Rule, importsRaw interface{}, from label.Label) {
	imports, _ := importsRaw.([]string)
	depSet := make(map[string]bool)
	for _, imp := range imports {
		l, err := resolveDImport(c, ix, imp, from)
		if err == errNotFound || err == errSelfImport || err == errStdImport {
			continue
		}
		if err != nil {
			log.Print(err)
			continue
		}
		depSet[l.Rel(from.Repo, from.Pkg).String()] = true
	}

	existing := r.AttrStrings("deps")
	for _, dep := range existing {
		depSet[dep] = true
	}
	if len(depSet) == 0 {
		r.DelAttr("deps")
		return
	}

	deps := make([]string, 0, len(depSet))
	for dep := range depSet {
		deps = append(deps, dep)
	}
	sort.Strings(deps)
	r.SetAttr("deps", deps)
}

var (
	errNotFound   = errors.New("not found")
	errSelfImport = errors.New("self import")
	errStdImport  = errors.New("standard import")
)

func resolveDImport(c *config.Config, ix *resolve.RuleIndex, imp string, from label.Label) (label.Label, error) {
	if isStandardImport(imp) {
		return label.NoLabel, errStdImport
	}
	spec := resolve.ImportSpec{Lang: langName, Imp: imp}
	if l, ok := resolve.FindRuleWithOverride(c, spec, langName); ok {
		return l, nil
	}
	matches := ix.FindRulesByImportWithConfig(c, spec, langName)
	if len(matches) == 0 {
		return label.NoLabel, errNotFound
	}
	if len(matches) > 1 {
		return label.NoLabel, fmt.Errorf("multiple D rules (%s and %s) may be imported with %q from %s", matches[0].Label, matches[1].Label, imp, from)
	}
	if matches[0].IsSelfImport(from) {
		return label.NoLabel, errSelfImport
	}
	return matches[0].Label, nil
}

type dFile struct {
	name        string
	module      string
	imports     []string
	hasMain     bool
	hasUnittest bool
}

type dubManifest struct {
	Name                string
	TargetName          string   `json:"targetName"`
	TargetType          string   `json:"targetType"`
	MainSourceFile      string   `json:"mainSourceFile"`
	SourcePaths         []string `json:"sourcePaths"`
	SourceFiles         []string `json:"sourceFiles"`
	ExcludedSourceFiles []string `json:"excludedSourceFiles"`
	ImportPaths         []string `json:"importPaths"`
	StringImportPaths   []string `json:"stringImportPaths"`
	Versions            []string `json:"versions"`
	raw                 map[string]json.RawMessage
}

func parseDubManifest(dir string, regularFiles []string) *dubManifest {
	if !hasRegularFile(regularFiles, "dub.json") {
		return nil
	}
	return parseDubManifestFile(dir)
}

func parseDubManifestFile(dir string) *dubManifest {
	data, err := os.ReadFile(path.Join(dir, "dub.json"))
	if err != nil {
		log.Printf("error reading dub.json in %s: %v", dir, err)
		return nil
	}
	clean := cleanDubJSON(string(data))
	var m dubManifest
	if err := json.Unmarshal([]byte(clean), &m); err != nil {
		log.Printf("error parsing dub.json in %s: %v", dir, err)
		return nil
	}
	if err := json.Unmarshal([]byte(clean), &m.raw); err != nil {
		log.Printf("error parsing dub.json object in %s: %v", dir, err)
	}
	return &m
}

func (m *dubManifest) ruleName() string {
	if m == nil {
		return ""
	}
	name := firstNonEmpty(m.TargetName, m.Name)
	if name == "" {
		return ""
	}
	return sanitizeName(name)
}

func (m *dubManifest) isLibrary() bool {
	if m == nil {
		return false
	}
	switch strings.ToLower(m.TargetType) {
	case "", "autodetect", "library", "sourcelibrary", "staticlibrary", "dynamiclibrary":
		return true
	default:
		return false
	}
}

func (m *dubManifest) isExecutable() bool {
	if m == nil {
		return false
	}
	switch strings.ToLower(m.TargetType) {
	case "", "autodetect", "executable":
		return true
	default:
		return false
	}
}

func collectDubLockSources(root string) []string {
	type dubPackageFiles struct {
		selection string
		recipes   []string
	}
	byDir := make(map[string]*dubPackageFiles)
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			log.Printf("error reading directory while collecting DUB lock inputs %s: %v", name, err)
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "bazel-bin", "bazel-out", "bazel-testlogs":
				if name != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		base := entry.Name()
		if base != "dub.selections.json" && base != "dub.json" && base != "dub.sdl" {
			return nil
		}
		dir := filepath.Dir(name)
		files := byDir[dir]
		if files == nil {
			files = &dubPackageFiles{}
			byDir[dir] = files
		}
		switch entry.Name() {
		case "dub.selections.json":
			files.selection = name
		case "dub.json", "dub.sdl":
			files.recipes = append(files.recipes, name)
		}
		return nil
	})
	if err != nil {
		log.Printf("error collecting DUB lock inputs under %s: %v", root, err)
	}
	var srcs []string
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		files := byDir[dir]
		if files.selection != "" {
			if label := dubLockSourceLabel(root, files.selection); label != "" {
				srcs = append(srcs, label)
			}
			continue
		}
		sort.Strings(files.recipes)
		for _, recipe := range files.recipes {
			if label := dubLockSourceLabel(root, recipe); label != "" {
				srcs = append(srcs, label)
			}
		}
	}
	return uniqueSorted(srcs)
}

func localDubLockSources(regularFiles []string) []string {
	if hasRegularFile(regularFiles, "dub.selections.json") {
		return []string{"dub.selections.json"}
	}
	var srcs []string
	for _, name := range []string{"dub.json", "dub.sdl"} {
		if hasRegularFile(regularFiles, name) {
			srcs = append(srcs, name)
		}
	}
	return srcs
}

func (m *dubManifest) hasDependencies() bool {
	if m == nil || len(m.raw) == 0 {
		return false
	}
	for key, raw := range m.raw {
		if key != "dependencies" && !strings.HasPrefix(key, "dependencies-") {
			continue
		}
		var deps map[string]json.RawMessage
		if err := json.Unmarshal(raw, &deps); err == nil && len(deps) > 0 {
			return true
		}
	}
	return false
}

func (m *dubManifest) dependencyLabels() []string {
	if m == nil || len(m.raw) == 0 {
		return nil
	}
	var deps []string
	for key, raw := range m.raw {
		if key != "dependencies" && !strings.HasPrefix(key, "dependencies-") {
			continue
		}
		var manifestDeps map[string]json.RawMessage
		if err := json.Unmarshal(raw, &manifestDeps); err != nil {
			continue
		}
		for name, depRaw := range manifestDeps {
			if isLocalDubDependency(depRaw) {
				continue
			}
			deps = append(deps, "@dub//"+name)
		}
	}
	return uniqueSorted(deps)
}

func isLocalDubDependency(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	_, ok := obj["path"]
	return ok
}

func (m *dubManifest) collectFiles(dir, rel string, regularFiles []string) []dFile {
	srcs := uniqueSorted(m.sourceFiles(dir, regularFiles))
	dFiles := make([]dFile, 0, len(srcs))
	for _, name := range srcs {
		if isDSource(name) {
			dFiles = append(dFiles, dFile{name: name, module: fallbackModule(rel, name)})
		}
	}
	return dFiles
}

func (m *dubManifest) sourceFiles(dir string, regularFiles []string) []string {
	var srcs []string
	sourcePaths := m.effectiveSourcePaths(dir)
	for _, sourcePath := range sourcePaths {
		srcs = append(srcs, collectDSourceFiles(path.Join(dir, sourcePath), sourcePath)...)
	}
	for _, sourceFile := range m.SourceFiles {
		srcs = append(srcs, cleanRel(sourceFile))
	}
	for _, sourceFile := range m.platformStringFields("sourceFiles") {
		srcs = append(srcs, cleanRel(sourceFile))
	}
	if m.MainSourceFile != "" {
		srcs = append(srcs, cleanRel(m.MainSourceFile))
	} else if len(sourcePaths) == 0 && len(m.SourceFiles) == 0 {
		for _, candidate := range []string{"source/app.d", "src/app.d"} {
			if fileExists(path.Join(dir, candidate)) {
				srcs = append(srcs, candidate)
			}
		}
		if len(srcs) == 0 && hasRegularFile(regularFiles, "app.d") {
			srcs = append(srcs, "app.d")
		}
	}
	return excludeFiles(uniqueSorted(srcs), m.excludedSourceFiles())
}

func (m *dubManifest) effectiveSourcePaths(dir string) []string {
	if len(m.SourcePaths) > 0 {
		return cleanRels(m.SourcePaths)
	}
	var paths []string
	for _, candidate := range []string{"source", "src"} {
		if dirExists(path.Join(dir, candidate)) {
			paths = append(paths, candidate)
		}
	}
	return paths
}

func (m *dubManifest) excludedSourceFiles() []string {
	excluded := append([]string(nil), m.ExcludedSourceFiles...)
	excluded = append(excluded, m.platformStringFields("excludedSourceFiles")...)
	return cleanRels(excluded)
}

func (m *dubManifest) platformStringFields(prefix string) []string {
	if m == nil || len(m.raw) == 0 {
		return nil
	}
	var values []string
	for key, raw := range m.raw {
		if key != prefix && !strings.HasPrefix(key, prefix+"-") {
			continue
		}
		var ss []string
		if err := json.Unmarshal(raw, &ss); err == nil {
			values = append(values, ss...)
		}
	}
	return values
}

func applyDubAttrs(r *rule.Rule, manifest *dubManifest, dir string) {
	if manifest == nil {
		return
	}
	if imports := manifest.effectiveImportPaths(dir); len(imports) > 0 {
		r.SetAttr("imports", imports)
	}
	if stringImports := cleanRels(manifest.StringImportPaths); len(stringImports) > 0 {
		r.SetAttr("string_imports", stringImports)
	}
	if versions := uniqueSorted(manifest.Versions); len(versions) > 0 {
		r.SetAttr("versions", versions)
	}
	if linkopts := manifest.linkopts(); !linkopts.IsEmpty() {
		r.SetAttr("linkopts", linkopts)
	}
}

func addDubDependencyDeps(r *rule.Rule, manifest *dubManifest) {
	manifestDeps := manifest.dependencyLabels()
	if len(manifestDeps) == 0 {
		return
	}
	depSet := make(map[string]bool)
	for _, dep := range r.AttrStrings("deps") {
		depSet[dep] = true
	}
	for _, dep := range manifestDeps {
		depSet[dep] = true
	}
	deps := make([]string, 0, len(depSet))
	for dep := range depSet {
		deps = append(deps, dep)
	}
	sort.Strings(deps)
	r.SetAttr("deps", deps)
}

func newDubLockDependenciesRule(srcs []string) *rule.Rule {
	r := rule.NewRule("dub_lock_dependencies", "dub_dependencies")
	r.SetAttr("srcs", srcs)
	r.SetAttr("dub_selections_lock", "dub.selections.lock.json")
	r.SetAttr("tags", []string{"manual"})
	return r
}

func newExportsFilesRule(srcs []string) *rule.Rule {
	r := rule.NewRule("exports_files", "")
	r.AddArg(rule.ExprFromValue(srcs))
	return r
}

func (m *dubManifest) effectiveImportPaths(dir string) []string {
	if len(m.ImportPaths) > 0 {
		return cleanRels(m.ImportPaths)
	}
	return m.effectiveSourcePaths(dir)
}

func (m *dubManifest) linkopts() rule.PlatformStrings {
	if m == nil || len(m.raw) == 0 {
		return rule.PlatformStrings{}
	}
	var ps rule.PlatformStrings
	for key, raw := range m.raw {
		if key != "libs" && !strings.HasPrefix(key, "libs-") {
			continue
		}
		var libs []string
		if err := json.Unmarshal(raw, &libs); err != nil {
			continue
		}
		opts := libsToLinkopts(libs)
		if len(opts) == 0 {
			continue
		}
		if key == "libs" {
			ps.Generic = append(ps.Generic, opts...)
			continue
		}
		for _, osName := range strings.Split(strings.TrimPrefix(key, "libs-"), "-") {
			constraint := dubOSConstraint(osName)
			if constraint == "" {
				continue
			}
			if ps.OS == nil {
				ps.OS = make(map[string][]string)
			}
			ps.OS[constraint] = append(ps.OS[constraint], opts...)
		}
	}
	ps.Generic = uniqueSorted(ps.Generic)
	for k, opts := range ps.OS {
		ps.OS[k] = uniqueSorted(opts)
	}
	return ps
}

func libsToLinkopts(libs []string) []string {
	opts := make([]string, 0, len(libs))
	for _, lib := range libs {
		lib = strings.TrimSpace(lib)
		if lib == "" || strings.Contains(lib, "/") || strings.HasSuffix(lib, ".lib") || strings.HasSuffix(lib, ".a") || strings.HasSuffix(lib, ".so") {
			continue
		}
		opts = append(opts, "-l"+lib)
	}
	return uniqueSorted(opts)
}

func dubOSConstraint(osName string) string {
	switch osName {
	case "linux":
		return "@platforms//os:linux"
	case "windows":
		return "@platforms//os:windows"
	case "osx", "macos", "darwin":
		return "@platforms//os:macos"
	default:
		return ""
	}
}

func collectDFiles(rel string, files []string) []dFile {
	var dFiles []dFile
	for _, name := range files {
		if isDSource(name) {
			dFiles = append(dFiles, dFile{name: name, module: fallbackModule(rel, name)})
		}
	}
	sort.Slice(dFiles, func(i, j int) bool { return dFiles[i].name < dFiles[j].name })
	return dFiles
}

func collectDSourceFiles(dir, rel string) []string {
	var files []string
	if !dirExists(dir) {
		return nil
	}
	err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			log.Printf("error reading DUB source path %s: %v", name, err)
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !isDSource(entry.Name()) {
			return nil
		}
		fileRel, err := filepath.Rel(dir, name)
		if err != nil {
			log.Printf("error relativizing DUB source %s: %v", name, err)
			return nil
		}
		files = append(files, path.Join(rel, filepath.ToSlash(fileRel)))
		return nil
	})
	if err != nil {
		log.Printf("error walking DUB source path %s: %v", dir, err)
	}
	return uniqueSorted(files)
}

func isUnderParentDubSource(args language.GenerateArgs) bool {
	if args.Config == nil || args.Config.RepoRoot == "" || args.Rel == "" {
		return false
	}
	rel := filepath.ToSlash(args.Rel)
	for parent := path.Dir(rel); ; parent = path.Dir(parent) {
		parentRel := parent
		if parentRel == "." {
			parentRel = ""
		}
		parentDir := filepath.Join(args.Config.RepoRoot, filepath.FromSlash(parentRel))
		var manifest *dubManifest
		if fileExists(path.Join(parentDir, "dub.json")) {
			manifest = parseDubManifest(parentDir, []string{"dub.json"})
		}
		if manifest != nil {
			for _, sourcePath := range manifest.effectiveSourcePaths(parentDir) {
				sourceRel := path.Join(parentRel, sourcePath)
				if rel == sourceRel || strings.HasPrefix(rel, sourceRel+"/") {
					return true
				}
			}
		}
		if parent == "." || parent == "/" {
			break
		}
	}
	return false
}

func parseDFile(dir, rel string, file dFile) dFile {
	content := readFile(path.Join(dir, file.name))
	clean := stripDCommentsAndStrings(content)
	if mod := parseModule(clean); mod != "" {
		file.module = mod
	}
	file.imports = parseImports(clean)
	file.hasMain = hasMain(clean)
	file.hasUnittest = hasWord(clean, "unittest")
	return file
}

func readFile(name string) string {
	data, err := os.ReadFile(name)
	if err != nil {
		log.Printf("error reading D source %s: %v", name, err)
		return ""
	}
	return string(data)
}

func newDRule(kind, name string, srcs []string, args language.GenerateArgs) *rule.Rule {
	sort.Strings(srcs)
	r := rule.NewRule(kind, name)
	r.SetAttr("srcs", srcs)
	if args.File == nil || !args.File.HasDefaultVisibility() {
		r.SetAttr("visibility", []string{"//visibility:public"})
	}
	return r
}

func libraryName(rel string) string {
	base := packageBase(rel)
	if base == "" {
		base = "root"
	}
	return base + "_d_library"
}

func binaryName(src string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(path.Base(src), ".di"), ".d")
	if base == "" {
		return "d_binary"
	}
	return sanitizeName(base)
}

func testName(rel string) string {
	base := packageBase(rel)
	if base == "" {
		base = "root"
	}
	return base + "_d_test"
}

func dProtoName(protoName string) string {
	if strings.HasSuffix(protoName, "_proto") {
		return strings.TrimSuffix(protoName, "_proto") + "_d_proto"
	}
	return protoName + "_d_proto"
}

func packageBase(rel string) string {
	if rel == "" {
		return ""
	}
	return sanitizeName(path.Base(rel))
}

func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r == '-' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if r == '.' {
				r = '_'
			}
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func isDRule(kind string) bool {
	return kind == "d_library" || kind == "d_proto_library" || kind == "d_binary" || kind == "d_test"
}

func modulesFromRule(pkg string, r *rule.Rule) []string {
	srcs := r.AttrStrings("srcs")
	modules := make([]string, 0, len(srcs))
	for _, src := range srcs {
		if strings.HasPrefix(src, ":") || strings.HasPrefix(src, "//") || strings.HasPrefix(src, "@") {
			continue
		}
		if !(strings.HasSuffix(src, ".d") || strings.HasSuffix(src, ".di")) {
			continue
		}
		modules = append(modules, fallbackModule(pkg, src))
	}
	return uniqueSorted(modules)
}

func fallbackModule(rel, name string) string {
	noExt := strings.TrimSuffix(strings.TrimSuffix(path.Join(rel, name), ".di"), ".d")
	noExt = strings.Trim(noExt, "/")
	return strings.ReplaceAll(noExt, "/", ".")
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func hasRegularFile(files []string, name string) bool {
	for _, f := range files {
		if f == name {
			return true
		}
	}
	return false
}

func isDSource(name string) bool {
	return strings.HasSuffix(name, ".d") || strings.HasSuffix(name, ".di")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func cleanRels(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if cleaned := cleanRel(v); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return uniqueSorted(out)
}

func cleanRel(name string) string {
	name = filepath.ToSlash(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "./")
	name = strings.Trim(name, "/")
	if name == "" {
		return ""
	}
	if name == "." {
		return ""
	}
	return path.Clean(name)
}

func relFromRoot(root, name string) string {
	rel, err := filepath.Rel(root, name)
	if err != nil {
		log.Printf("error relativizing %s against %s: %v", name, root, err)
		return ""
	}
	return cleanRel(filepath.ToSlash(rel))
}

func dubLockSourceLabel(root, name string) string {
	rel := relFromRoot(root, name)
	if rel == "" {
		return ""
	}
	pkg := path.Dir(rel)
	base := path.Base(rel)
	if pkg == "." {
		return base
	}
	return "//" + pkg + ":" + base
}

func excludeFiles(files, excluded []string) []string {
	if len(excluded) == 0 {
		return files
	}
	exclude := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		exclude[name] = true
	}
	out := make([]string, 0, len(files))
	for _, file := range files {
		if !exclude[file] {
			out = append(out, file)
		}
	}
	return out
}

func fileExists(name string) bool {
	info, err := os.Stat(name)
	return err == nil && !info.IsDir()
}

func dirExists(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.IsDir()
}

func cleanDubJSON(s string) string {
	s = stripJSONComments(s)
	return stripTrailingJSONCommas(s)
}

func stripJSONComments(s string) string {
	var b strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(s); {
		if inString {
			b.WriteByte(s[i])
			if escaped {
				escaped = false
			} else if s[i] == '\\' {
				escaped = true
			} else if s[i] == '"' {
				inString = false
			}
			i++
			continue
		}
		if s[i] == '"' {
			inString = true
			b.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "//") {
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			b.WriteString("  ")
			i += 2
			for i < len(s) && !strings.HasPrefix(s[i:], "*/") {
				if s[i] == '\n' {
					b.WriteByte('\n')
				} else {
					b.WriteByte(' ')
				}
				i++
			}
			if i < len(s) {
				b.WriteString("  ")
				i += 2
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func stripTrailingJSONCommas(s string) string {
	var b strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		if inString {
			b.WriteByte(s[i])
			if escaped {
				escaped = false
			} else if s[i] == '\\' {
				escaped = true
			} else if s[i] == '"' {
				inString = false
			}
			continue
		}
		if s[i] == '"' {
			inString = true
			b.WriteByte(s[i])
			continue
		}
		if s[i] == ',' {
			j := i + 1
			for j < len(s) && unicode.IsSpace(rune(s[j])) {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func stripDCommentsAndStrings(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "//"):
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
		case strings.HasPrefix(s[i:], "/*"):
			b.WriteString("  ")
			i += 2
			for i < len(s) && !strings.HasPrefix(s[i:], "*/") {
				if s[i] == '\n' {
					b.WriteByte('\n')
				} else {
					b.WriteByte(' ')
				}
				i++
			}
			if i < len(s) {
				b.WriteString("  ")
				i += 2
			}
		case strings.HasPrefix(s[i:], "/+"):
			b.WriteString("  ")
			i += 2
			depth := 1
			for i < len(s) && depth > 0 {
				if strings.HasPrefix(s[i:], "/+") {
					depth++
					b.WriteString("  ")
					i += 2
				} else if strings.HasPrefix(s[i:], "+/") {
					depth--
					b.WriteString("  ")
					i += 2
				} else {
					if s[i] == '\n' {
						b.WriteByte('\n')
					} else {
						b.WriteByte(' ')
					}
					i++
				}
			}
		case strings.HasPrefix(s[i:], `q"`) || strings.HasPrefix(s[i:], `r"`) || strings.HasPrefix(s[i:], `x"`):
			i = consumeQuoted(s, i+1, &b)
		case s[i] == '"' || s[i] == '\'' || s[i] == '`':
			i = consumeQuoted(s, i, &b)
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

func consumeQuoted(s string, i int, b *strings.Builder) int {
	quote := s[i]
	b.WriteByte(' ')
	i++
	for i < len(s) {
		if s[i] == '\n' {
			b.WriteByte('\n')
		} else {
			b.WriteByte(' ')
		}
		if s[i] == '\\' && i+1 < len(s) {
			i += 2
			b.WriteByte(' ')
			continue
		}
		if s[i] == quote {
			i++
			break
		}
		i++
	}
	return i
}

func parseModule(s string) string {
	i := findWord(s, "module", 0)
	if i < 0 {
		return ""
	}
	i += len("module")
	return readQualifiedIdent(s, skipSpace(s, i))
}

func parseImports(s string) []string {
	var imports []string
	for i := findWord(s, "import", 0); i >= 0; i = findWord(s, "import", i+len("import")) {
		if previousWord(s, i) == "mixin" {
			continue
		}
		start := skipSpace(s, i+len("import"))
		if previousWord(s, i) == "static" || previousWord(s, i) == "public" {
			start = skipSpace(s, i+len("import"))
		}
		end := strings.IndexByte(s[start:], ';')
		if end < 0 {
			continue
		}
		imports = append(imports, parseImportClause(s[start:start+end])...)
	}
	return uniqueSorted(imports)
}

func parseImportClause(clause string) []string {
	var imports []string
	for _, part := range strings.Split(clause, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.IndexByte(part, ':'); idx >= 0 {
			part = part[:idx]
		}
		if idx := strings.IndexByte(part, '='); idx >= 0 {
			part = part[idx+1:]
		}
		part = strings.TrimSpace(part)
		if mod := readQualifiedIdent(part, 0); mod != "" {
			imports = append(imports, mod)
		}
	}
	return imports
}

func hasMain(s string) bool {
	for _, name := range []string{"main", "D main"} {
		if hasWord(s, name) {
			return true
		}
	}
	return false
}

func hasWord(s, word string) bool {
	return findWord(s, word, 0) >= 0
}

func findWord(s, word string, start int) int {
	for {
		i := strings.Index(s[start:], word)
		if i < 0 {
			return -1
		}
		i += start
		beforeOK := i == 0 || !isIdentRune(rune(s[i-1]))
		after := i + len(word)
		afterOK := after >= len(s) || !isIdentRune(rune(s[after]))
		if beforeOK && afterOK {
			return i
		}
		start = i + len(word)
	}
}

func previousWord(s string, i int) string {
	j := i - 1
	for j >= 0 && unicode.IsSpace(rune(s[j])) {
		j--
	}
	end := j + 1
	for j >= 0 && isIdentRune(rune(s[j])) {
		j--
	}
	return s[j+1 : end]
}

func skipSpace(s string, i int) int {
	for i < len(s) && unicode.IsSpace(rune(s[i])) {
		i++
	}
	return i
}

func readQualifiedIdent(s string, i int) string {
	i = skipSpace(s, i)
	start := i
	for i < len(s) {
		r := rune(s[i])
		if r == '.' || isIdentRune(r) {
			i++
			continue
		}
		break
	}
	return strings.Trim(strings.TrimSpace(s[start:i]), ".")
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isStandardImport(imp string) bool {
	root := imp
	if i := strings.IndexByte(root, '.'); i >= 0 {
		root = root[:i]
	}
	switch root {
	case "core", "etc", "object", "std":
		return true
	default:
		return false
	}
}
