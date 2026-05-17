package d

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
	"github.com/bazelbuild/rules_go/go/runfiles"
)

const (
	langName               = "d"
	dubGeneratedPrivateKey = "_gazelle_d_dub_generated"
	importsPrivateKey      = "_gazelle_d_imports"
	modulesPrivateKey      = "_gazelle_d_modules"
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
		"config_setting": {
			MergeableAttrs: map[string]bool{"constraint_values": true},
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
	hasDubRecipe := hasRegularFile(args.RegularFiles, "dub.json") || hasRegularFile(args.RegularFiles, "dub.sdl")
	dubRules := dubBuildFileRules(args.Dir, args.Rel, args.RegularFiles)

	localDubLockSrcs := localDubLockSources(args.RegularFiles)
	var dubLockSrcs []string
	if args.Rel == "" && hasDubRecipe {
		dubLockSrcs = collectDubLockSources(args.Dir)
	}
	if len(dubRules) == 0 && len(args.OtherGen) == 0 && len(dubLockSrcs) == 0 && len(localDubLockSrcs) == 0 {
		return language.GenerateResult{}
	}

	var gen []*rule.Rule
	var imports []interface{}
	if args.Rel != "" && len(localDubLockSrcs) > 0 {
		gen = append(gen, newExportsFilesRule(localDubLockSrcs))
		imports = append(imports, []string(nil))
	}
	if hasDubRecipe {
		gen, imports = appendProtoRules(gen, imports, args)
	}
	for _, r := range dubRules {
		gen, imports = appendParsedDubRule(gen, imports, r, args)
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

func appendDubTargetRules(gen []*rule.Rule, imports []interface{}, target *dubBuildTarget, args language.GenerateArgs) ([]*rule.Rule, []interface{}) {
	files := target.collectFiles(args.Dir, args.Rel)
	libSrcs := make([]string, 0, len(files))
	var tests []dFile
	var allImports []string
	var allModules []string

	for _, f := range files {
		info := parseDFile(args.Dir, args.Rel, f)
		allImports = append(allImports, info.imports...)
		allModules = append(allModules, info.module)
		libSrcs = append(libSrcs, info.name)
		if info.hasUnittest {
			tests = append(tests, info)
		}
	}

	libName := target.ruleName()
	if libName == "" {
		libName = libraryName(args.Rel)
	}
	if target.isLibrary() && len(libSrcs) > 0 {
		r := newDRule("d_library", libName, libSrcs, args)
		setDubSrcsAttr(r, target)
		applyDubAttrs(r, target, args.Dir)
		addDubDependencyDeps(r, target)
		r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(allModules))
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(allImports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(allImports))
	}

	if target.isExecutable() && len(files) > 0 {
		srcs := make([]string, 0, len(files))
		for _, f := range files {
			srcs = append(srcs, f.name)
		}
		r := newDRule("d_binary", libName, srcs, args)
		setDubSrcsAttr(r, target)
		applyDubAttrs(r, target, args.Dir)
		addDubDependencyDeps(r, target)
		r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(allModules))
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(allImports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(allImports))
	}

	if target.isLibrary() && len(tests) > 0 {
		srcs := make([]string, 0, len(tests))
		testImports := make([]string, 0)
		testModules := make([]string, 0, len(tests))
		for _, f := range tests {
			srcs = append(srcs, f.name)
			testImports = append(testImports, f.imports...)
			testModules = append(testModules, f.module)
		}
		r := newDRule("d_test", testName(args.Rel, libName), srcs, args)
		setDubSrcsAttr(r, target)
		applyDubAttrs(r, target, args.Dir)
		if len(libSrcs) > 0 {
			r.SetAttr("deps", []string{":" + libName})
		}
		addDubDependencyDeps(r, target)
		r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(testModules))
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(testImports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(testImports))
	}
	return gen, imports
}

func appendParsedDubRule(gen []*rule.Rule, imports []interface{}, r *rule.Rule, args language.GenerateArgs) ([]*rule.Rule, []interface{}) {
	switch r.Kind() {
	case "d_library", "d_binary", "d_test":
	default:
		gen = append(gen, r)
		imports = append(imports, []string(nil))
		return gen, imports
	}

	files := collectFilesFromRuleSrcs(args.Dir, args.Rel, r)
	var allImports []string
	var allModules []string
	for _, f := range files {
		info := parseDFile(args.Dir, args.Rel, f)
		allImports = append(allImports, info.imports...)
		allModules = append(allModules, info.module)
	}
	normalizeDubRepositoryDeps(r)
	r.SetPrivateAttr(dubGeneratedPrivateKey, true)
	r.SetPrivateAttr(importsPrivateKey, uniqueSorted(allImports))
	r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(allModules))
	gen = append(gen, r)
	imports = append(imports, uniqueSorted(allImports))
	return gen, imports
}

func normalizeDubRepositoryDeps(r *rule.Rule) {
	replaceDubRepositoryPlaceholder(r.Attr("deps"))
}

func replaceDubRepositoryPlaceholder(expr bzl.Expr) {
	switch x := expr.(type) {
	case *bzl.StringExpr:
		x.Value = strings.ReplaceAll(x.Value, "@%DUB_REPOSITORY_NAME%//", "@dub//")
		x.Token = ""
	case *bzl.ListExpr:
		for _, item := range x.List {
			replaceDubRepositoryPlaceholder(item)
		}
	case *bzl.BinaryExpr:
		replaceDubRepositoryPlaceholder(x.X)
		replaceDubRepositoryPlaceholder(x.Y)
	case *bzl.CallExpr:
		for _, item := range x.List {
			replaceDubRepositoryPlaceholder(item)
		}
	case *bzl.DictExpr:
		for _, item := range x.List {
			replaceDubRepositoryPlaceholder(item.Value)
		}
	case *bzl.KeyValueExpr:
		replaceDubRepositoryPlaceholder(x.Value)
	}
}

func collectFilesFromRuleSrcs(dir, rel string, r *rule.Rule) []dFile {
	srcs := uniqueSorted(expandSrcsExpr(dir, r.Attr("srcs")))
	dFiles := make([]dFile, 0, len(srcs))
	for _, name := range srcs {
		if isDSource(name) {
			dFiles = append(dFiles, dFile{name: name, module: fallbackModule(rel, name)})
		}
	}
	return dFiles
}

func expandSrcsExpr(dir string, expr bzl.Expr) []string {
	switch x := expr.(type) {
	case nil:
		return nil
	case *bzl.StringExpr:
		return []string{cleanRel(x.Value)}
	case *bzl.ListExpr:
		var srcs []string
		for _, item := range x.List {
			srcs = append(srcs, expandSrcsExpr(dir, item)...)
		}
		return srcs
	case *bzl.BinaryExpr:
		if x.Op != "+" {
			return nil
		}
		return append(expandSrcsExpr(dir, x.X), expandSrcsExpr(dir, x.Y)...)
	case *bzl.CallExpr:
		if ident, ok := x.X.(*bzl.LiteralExpr); ok && ident.Token == "select" && len(x.List) == 1 {
			dict, ok := x.List[0].(*bzl.DictExpr)
			if !ok {
				return nil
			}
			var srcs []string
			for _, item := range dict.List {
				srcs = append(srcs, expandSrcsExpr(dir, item.Value)...)
			}
			return srcs
		}
		if glob, ok := rule.ParseGlobExpr(x); ok {
			return expandGlobValue(dir, glob)
		}
	}
	if glob, ok := rule.ParseGlobExpr(expr); ok {
		return expandGlobValue(dir, glob)
	}
	return nil
}

func expandGlobValue(dir string, glob rule.GlobValue) []string {
	var srcs []string
	err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel := relFromRoot(dir, name)
		if !matchesAnyBazelGlob(glob.Patterns, rel) || matchesAnyBazelGlob(glob.Excludes, rel) {
			return nil
		}
		srcs = append(srcs, rel)
		return nil
	})
	if err != nil {
		log.Printf("error expanding generated DUB glob under %s: %v", dir, err)
	}
	return uniqueSorted(srcs)
}

func matchesAnyBazelGlob(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matchBazelGlob(pattern, name) {
			return true
		}
	}
	return false
}

func matchBazelGlob(pattern, name string) bool {
	patternParts := strings.Split(filepath.ToSlash(pattern), "/")
	nameParts := strings.Split(filepath.ToSlash(name), "/")
	return matchBazelGlobParts(patternParts, nameParts)
}

func matchBazelGlobParts(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchBazelGlobParts(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchBazelGlobParts(pattern[1:], name[1:])
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
	if r.Kind() != "d_library" {
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
	if isDubGeneratedRule(r) {
		warnDubGeneratedMissingDeps(c, ix, r, imports, from)
		return
	}

	depSet := make(map[string]bool)
	for _, dep := range r.AttrStrings("deps") {
		depSet[dep] = true
	}
	for _, imp := range imports {
		if hasExternalDepForImport(depSet, imp) {
			continue
		}
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

func isDubGeneratedRule(r *rule.Rule) bool {
	generated, _ := r.PrivateAttr(dubGeneratedPrivateKey).(bool)
	return generated
}

func warnDubGeneratedMissingDeps(c *config.Config, ix *resolve.RuleIndex, r *rule.Rule, imports []string, from label.Label) {
	depSet := make(map[string]bool)
	for _, dep := range r.AttrStrings("deps") {
		depSet[dep] = true
	}
	missingImportsByDep := make(map[string][]string)
	for _, imp := range imports {
		if hasExternalDepForImport(depSet, imp) {
			continue
		}
		l, err := resolveDImport(c, ix, imp, from)
		if err == errNotFound || err == errSelfImport || err == errStdImport {
			continue
		}
		if err != nil {
			log.Print(err)
			continue
		}
		dep := l.Rel(from.Repo, from.Pkg).String()
		if depSet[dep] {
			continue
		}
		missingImportsByDep[dep] = append(missingImportsByDep[dep], imp)
	}
	deps := make([]string, 0, len(missingImportsByDep))
	for dep := range missingImportsByDep {
		deps = append(deps, dep)
	}
	sort.Strings(deps)
	for _, dep := range deps {
		imports := uniqueSorted(missingImportsByDep[dep])
		log.Printf("gazelle_d: %s missing dep %s in dub.json/dub.sdl for imports [%s]", from, dep, strings.Join(imports, ", "))
	}
}

func hasExternalDepForImport(deps map[string]bool, imp string) bool {
	for dep := range deps {
		const prefix = "@dub//"
		if !strings.HasPrefix(dep, prefix) {
			continue
		}
		name := strings.TrimPrefix(dep, prefix)
		if imp == name || strings.HasPrefix(imp, name+".") {
			return true
		}
	}
	return false
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
	if l, ok := findRuleWithOverride(c, spec); ok {
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

func findRuleWithOverride(c *config.Config, spec resolve.ImportSpec) (label.Label, bool) {
	if c == nil || c.Exts == nil || c.Exts["_resolve"] == nil {
		return label.NoLabel, false
	}
	return resolve.FindRuleWithOverride(c, spec, langName)
}

type dFile struct {
	name        string
	module      string
	imports     []string
	hasMain     bool
	hasUnittest bool
}

type dubBuildTarget struct {
	PackageName       string
	TargetName        string
	TargetType        dubTargetType
	MainSourceFile    string
	ImportPaths       []string
	StringImportPaths []string
	Versions          []string
	SrcsAttr          interface{}
	recipeSourceFiles []string
	recipeDeps        []string
	recipeLibs        []string
	recipeLFlags      []string
}

type dubRecipe struct {
	Name                string
	TargetName          string
	TargetType          dubTargetType
	MainSourceFile      string
	Dependencies        []string
	SourceFiles         []string
	SourcePaths         []string
	ExcludedSourceFiles []string
	ImportPaths         []string
	StringImportPaths   []string
	Versions            []string
	Libs                []string
	LFlags              []string
	SubPackages         []dubRecipe
	sourceFilesSet      bool
	sourcePathsSet      bool
}

type dubTargetType string

const (
	dubTargetAutodetect    dubTargetType = "autodetect"
	dubTargetNone          dubTargetType = "none"
	dubTargetExecutable    dubTargetType = "executable"
	dubTargetLibrary       dubTargetType = "library"
	dubTargetSourceLibrary dubTargetType = "sourceLibrary"
	dubTargetDynamicLib    dubTargetType = "dynamicLibrary"
	dubTargetStaticLib     dubTargetType = "staticLibrary"
	dubTargetObject        dubTargetType = "object"
)

func (t *dubTargetType) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*t = dubTargetType(name)
		return nil
	}
	var index int
	if err := json.Unmarshal(data, &index); err != nil {
		return err
	}
	names := []dubTargetType{
		dubTargetAutodetect,
		dubTargetNone,
		dubTargetExecutable,
		dubTargetLibrary,
		dubTargetSourceLibrary,
		dubTargetDynamicLib,
		dubTargetStaticLib,
		dubTargetObject,
	}
	if index < 0 || index >= len(names) {
		*t = dubTargetAutodetect
		return nil
	}
	*t = names[index]
	return nil
}

func dubBuildFileRules(dir, rel string, regularFiles []string) []*rule.Rule {
	input := localDubRecipeInput(regularFiles)
	if input == "" {
		return nil
	}
	tool := os.Getenv("GAZELLE_D_GENERATE_BUILD_FILE")
	if tool == "" {
		var err error
		tool, err = runfiles.Rlocation("rules_d+/dub/selections_lock/generate_build_file")
		if err != nil {
			log.Print("rules_d generate_build_file tool not found; skipping DUB-derived D targets")
			return nil
		}
	}
	dub := os.Getenv("GAZELLE_D_DUB")
	if dub == "" {
		var err error
		dub, err = runfiles.Rlocation("rules_d+/d/dub.exe")
		if err != nil {
			log.Print("rules_d dub tool not found; skipping DUB-derived D targets")
			return nil
		}
	}
	if tool == "" {
		log.Print("rules_d generate_build_file tool not found; skipping DUB-derived D targets")
		return nil
	}
	if dub == "" {
		log.Print("rules_d dub tool not found; skipping DUB-derived D targets")
		return nil
	}
	cmd := exec.Command(tool, "--dub="+dub, "--include_tests", "--input="+filepath.Join(dir, input))
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("error running %s for DUB BUILD generation in %s: %v: %s", tool, dir, err, strings.TrimSpace(string(output)))
		return nil
	}
	f, err := rule.LoadData(filepath.Join(dir, "BUILD.bazel"), rel, output)
	if err != nil {
		log.Printf("error parsing DUB-generated BUILD content in %s: %v", dir, err)
		return nil
	}
	var rules []*rule.Rule
	for _, r := range f.Rules {
		switch r.Kind() {
		case "d_library", "d_binary", "d_test", "config_setting":
			rules = append(rules, r)
		}
	}
	return rules
}

func localDubRecipeInput(regularFiles []string) string {
	if hasRegularFile(regularFiles, "dub.json") {
		return "dub.json"
	}
	if hasRegularFile(regularFiles, "dub.sdl") {
		return "dub.sdl"
	}
	return ""
}

func dubRecipeTargets(dir string, regularFiles []string) []*dubBuildTarget {
	if !hasRegularFile(regularFiles, "dub.json") && !hasRegularFile(regularFiles, "dub.sdl") {
		return nil
	}
	recipe, ok := convertDubRecipe(dir)
	if !ok {
		return nil
	}
	return compactDubTargets(recipe.buildTargets(dir))
}

func convertDubRecipe(dir string) (dubRecipe, bool) {
	dub, err := runfiles.Rlocation("rules_d+/d/dub.exe")
	if err != nil {
		return dubRecipe{}, false
	}
	cmdArgs := []string{"convert", "--format=json", "--stdout", "--root=" + dir}
	cmd := exec.Command(dub, cmdArgs...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			log.Printf("error running %s convert --root=%s: %v: %s", dub, dir, err, strings.TrimSpace(string(exitErr.Stderr)))
		} else {
			log.Printf("error running %s convert --root=%s: %v", dub, dir, err)
		}
		return dubRecipe{}, false
	}
	var recipe dubRecipe
	if err := json.Unmarshal(output, &recipe); err != nil {
		log.Printf("error parsing dub convert output in %s: %v", dir, err)
		return dubRecipe{}, false
	}
	return recipe, true
}

func compactDubTargets(targets []*dubBuildTarget) []*dubBuildTarget {
	out := make([]*dubBuildTarget, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		key := firstNonEmpty(target.PackageName, target.ruleName())
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, target)
	}
	return out
}

func runfilesRoots() []string {
	var roots []string
	if dir := os.Getenv("RUNFILES_DIR"); dir != "" {
		roots = append(roots, dir)
	}
	if manifest := os.Getenv("RUNFILES_MANIFEST_FILE"); manifest != "" {
		if dir := filepath.Dir(manifest); dir != "" && dir != "." {
			roots = append(roots, dir)
		}
	}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, exe+".runfiles")
	}
	return uniqueSorted(roots)
}

func (r *dubRecipe) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var base struct {
		Name                string        `json:"name"`
		TargetName          string        `json:"targetName"`
		TargetType          dubTargetType `json:"targetType"`
		MainSourceFile      string        `json:"mainSourceFile"`
		SourceFiles         []string      `json:"sourceFiles"`
		SourcePaths         []string      `json:"sourcePaths"`
		ExcludedSourceFiles []string      `json:"excludedSourceFiles"`
		ImportPaths         []string      `json:"importPaths"`
		StringImportPaths   []string      `json:"stringImportPaths"`
		Versions            []string      `json:"versions"`
		Libs                []string      `json:"libs"`
		LFlags              []string      `json:"lflags"`
		SubPackages         []dubRecipe   `json:"subPackages"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return err
	}
	r.Name = base.Name
	r.TargetName = base.TargetName
	r.TargetType = base.TargetType
	r.MainSourceFile = base.MainSourceFile
	r.SourceFiles = base.SourceFiles
	r.SourcePaths = base.SourcePaths
	r.ExcludedSourceFiles = base.ExcludedSourceFiles
	r.ImportPaths = base.ImportPaths
	r.StringImportPaths = base.StringImportPaths
	r.Versions = base.Versions
	r.Libs = base.Libs
	r.LFlags = base.LFlags
	r.SubPackages = base.SubPackages
	_, r.sourceFilesSet = raw["sourceFiles"]
	_, r.sourcePathsSet = raw["sourcePaths"]
	r.Dependencies = append([]string(nil), dependencyNames(raw["dependencies"])...)

	for key, value := range raw {
		field, suffix, ok := strings.Cut(key, "-")
		if !ok || !matchesHostSuffix(suffix) {
			continue
		}
		switch field {
		case "sourceFiles":
			r.SourceFiles = append(r.SourceFiles, stringList(value)...)
			r.sourceFilesSet = true
		case "sourcePaths":
			r.SourcePaths = append(r.SourcePaths, stringList(value)...)
			r.sourcePathsSet = true
		case "excludedSourceFiles":
			r.ExcludedSourceFiles = append(r.ExcludedSourceFiles, stringList(value)...)
		case "importPaths":
			r.ImportPaths = append(r.ImportPaths, stringList(value)...)
		case "stringImportPaths":
			r.StringImportPaths = append(r.StringImportPaths, stringList(value)...)
		case "versions":
			r.Versions = append(r.Versions, stringList(value)...)
		case "libs":
			r.Libs = append(r.Libs, stringList(value)...)
		case "lflags":
			r.LFlags = append(r.LFlags, stringList(value)...)
		}
	}
	r.SourceFiles = uniqueSorted(r.SourceFiles)
	r.SourcePaths = uniqueSorted(r.SourcePaths)
	r.ExcludedSourceFiles = uniqueSorted(r.ExcludedSourceFiles)
	r.ImportPaths = uniqueSorted(r.ImportPaths)
	r.StringImportPaths = uniqueSorted(r.StringImportPaths)
	r.Versions = uniqueSorted(r.Versions)
	r.Libs = uniqueSorted(r.Libs)
	r.LFlags = uniqueSorted(r.LFlags)
	return nil
}

func dependencyNames(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		return uniqueSorted(names)
	}
	var deps map[string]json.RawMessage
	if err := json.Unmarshal(raw, &deps); err != nil {
		return nil
	}
	for name := range deps {
		names = append(names, strings.TrimSpace(name))
	}
	return uniqueSorted(names)
}

func stringList(raw json.RawMessage) []string {
	var values []string
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func matchesHostSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	host := map[string]bool{
		runtime.GOOS: true,
		"posix":      runtime.GOOS != "windows",
	}
	knownPlatforms := map[string]bool{
		"android": true, "darwin": true, "dragonfly": true, "freebsd": true,
		"linux": true, "netbsd": true, "openbsd": true, "osx": true,
		"posix": true, "solaris": true, "unix": true, "windows": true,
	}
	matchedPlatform := false
	for _, token := range strings.Split(suffix, "-") {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" {
			continue
		}
		if knownPlatforms[token] {
			if !host[token] {
				return false
			}
			matchedPlatform = true
		}
	}
	return matchedPlatform
}

func (r dubRecipe) buildTargets(dir string) []*dubBuildTarget {
	targets := []*dubBuildTarget{r.buildTarget(dir, r.Name, r.Name, true)}
	subpackages := append([]dubRecipe(nil), r.SubPackages...)
	sort.Slice(subpackages, func(i, j int) bool {
		return sanitizeName(subpackages[i].Name) < sanitizeName(subpackages[j].Name)
	})
	for _, subpackage := range subpackages {
		if subpackage.Name == "" {
			continue
		}
		targets = append(targets, subpackage.buildTarget(dir, r.Name, r.Name+":"+subpackage.Name, false))
	}
	return targets
}

func (r dubRecipe) buildTarget(dir, rootName, packageName string, root bool) *dubBuildTarget {
	targetName := r.Name
	if root {
		targetName = firstNonEmpty(r.TargetName, r.Name)
	}
	return &dubBuildTarget{
		PackageName:       packageName,
		TargetName:        targetName,
		TargetType:        r.TargetType,
		MainSourceFile:    relUnder(dir, r.MainSourceFile),
		ImportPaths:       localOrCleanRelsUnder(dir, r.ImportPaths),
		StringImportPaths: localOrCleanRelsUnder(dir, r.StringImportPaths),
		Versions:          uniqueSorted(r.Versions),
		SrcsAttr:          r.srcsAttr(dir),
		recipeLibs:        uniqueSorted(r.Libs),
		recipeLFlags:      uniqueSorted(r.LFlags),
		recipeDeps:        dubDependencyLabels(rootName, packageName, r.Dependencies),
		recipeSourceFiles: r.sourceFiles(dir),
	}
}

func (r dubRecipe) srcsAttr(dir string) interface{} {
	if r.sourceFilesSet {
		return r.sourceFiles(dir)
	}
	return sourcePathGlobExprs(dir, r.SourcePaths, r.sourcePathsSet, dubRecipeExcludePatterns(dir, r.ExcludedSourceFiles))
}

func (r dubRecipe) sourceFiles(dir string) []string {
	srcs := expandDubRecipeSources(dir, r.SourceFiles, r.SourcePaths, r.sourceFilesSet, r.sourcePathsSet)
	excluded := make(map[string]bool)
	for _, name := range expandDubSourcePatterns(dir, r.ExcludedSourceFiles) {
		excluded[name] = true
	}
	out := make([]string, 0, len(srcs))
	for _, src := range srcs {
		if !excluded[src] {
			out = append(out, src)
		}
	}
	return uniqueSorted(out)
}

func sourcePathGlobExprs(dir string, sourcePaths []string, sourcePathsSet bool, excludes []string) interface{} {
	globs := sourcePathGlobValues(dir, sourcePaths, sourcePathsSet, excludes)
	if len(globs) == 0 {
		return []string(nil)
	}
	if len(globs) == 1 {
		return globs[0]
	}
	var expr bzl.Expr = rule.ExprFromValue(globs[0])
	for _, glob := range globs[1:] {
		expr = &bzl.BinaryExpr{
			X:  expr,
			Op: "+",
			Y:  rule.ExprFromValue(glob),
		}
	}
	return expr
}

func sourcePathGlobValues(dir string, sourcePaths []string, sourcePathsSet bool, excludes []string) []rule.GlobValue {
	if len(sourcePaths) == 0 && !sourcePathsSet {
		sourcePaths = []string{"source"}
	}
	var globs []rule.GlobValue
	for _, sourcePath := range cleanRels(sourcePaths) {
		patterns := sourcePathGlobPatterns(dir, sourcePath, excludes)
		if len(patterns) == 0 {
			continue
		}
		globs = append(globs, rule.GlobValue{
			Patterns: patterns,
			Excludes: sourcePathExcludes(sourcePath, excludes),
		})
	}
	return globs
}

func sourcePathGlobPatterns(dir, sourcePath string, excludes []string) []string {
	files := expandDubRecipeSources(dir, nil, []string{sourcePath}, false, true)
	excluded := make(map[string]bool)
	for _, name := range expandDubSourcePatterns(dir, sourcePathExcludes(sourcePath, excludes)) {
		excluded[name] = true
	}
	extensions := make(map[string]bool)
	for _, file := range files {
		if excluded[file] {
			continue
		}
		switch {
		case strings.HasSuffix(file, ".d"):
			extensions[".d"] = true
		case strings.HasSuffix(file, ".di"):
			extensions[".di"] = true
		}
	}
	var patterns []string
	if extensions[".d"] {
		patterns = append(patterns, path.Join(sourcePath, "**", "*.d"))
	}
	if extensions[".di"] {
		patterns = append(patterns, path.Join(sourcePath, "**", "*.di"))
	}
	return patterns
}

func sourcePathExcludes(sourcePath string, excludes []string) []string {
	var relevant []string
	for _, exclude := range excludes {
		if exclude == sourcePath || strings.HasPrefix(exclude, sourcePath+"/") {
			relevant = append(relevant, exclude)
		}
	}
	return uniqueSorted(relevant)
}

func dubRecipeExcludePatterns(dir string, patterns []string) []string {
	var excludes []string
	for _, pattern := range patterns {
		if rel := relDubPatternUnder(dir, pattern); rel != "" {
			excludes = append(excludes, rel)
		}
	}
	return uniqueSorted(excludes)
}

func expandDubRecipeSources(dir string, sourceFiles, sourcePaths []string, sourceFilesSet, sourcePathsSet bool) []string {
	if sourceFilesSet {
		return expandDubSourcePatterns(dir, sourceFiles)
	}
	if len(sourcePaths) == 0 && !sourcePathsSet {
		sourcePaths = []string{"source"}
	}
	var srcs []string
	for _, sourcePath := range cleanRels(sourcePaths) {
		root := filepath.Join(dir, filepath.FromSlash(sourcePath))
		err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			if isDSource(entry.Name()) {
				srcs = append(srcs, relFromRoot(dir, name))
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("error expanding DUB source path %s: %v", root, err)
		}
	}
	return uniqueSorted(srcs)
}

func expandDubSourcePatterns(dir string, patterns []string) []string {
	var srcs []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		relPattern := relDubPatternUnder(dir, pattern)
		if relPattern == "" {
			continue
		}
		if !strings.ContainsAny(relPattern, "*?[") {
			srcs = append(srcs, relPattern)
			continue
		}
		matches, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(relPattern)))
		if err != nil {
			log.Printf("error expanding DUB source glob %s: %v", relPattern, err)
			continue
		}
		for _, match := range matches {
			if info, err := os.Stat(match); err == nil && !info.IsDir() {
				srcs = append(srcs, relFromRoot(dir, match))
			}
		}
	}
	return uniqueSorted(srcs)
}

func relDubPatternUnder(root, pattern string) string {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" {
		return ""
	}
	if !path.IsAbs(pattern) {
		return cleanRel(pattern)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	rootPattern := filepath.ToSlash(filepath.Clean(rootAbs))
	pattern = path.Clean(pattern)
	if pattern == rootPattern {
		return ""
	}
	if strings.HasPrefix(pattern, rootPattern+"/") {
		return cleanRel(strings.TrimPrefix(pattern, rootPattern+"/"))
	}
	return ""
}

func dubDependencyLabels(rootName, currentName string, names []string) []string {
	var deps []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name == currentName {
			continue
		}
		if strings.Contains(name, ":") {
			deps = append(deps, ":"+dubPackageNameRuleName(name))
			continue
		}
		if name == rootName {
			deps = append(deps, ":"+sanitizeName(name))
			continue
		}
		deps = append(deps, "@dub//"+name)
	}
	return uniqueSorted(deps)
}

func (t *dubBuildTarget) ruleName() string {
	if t == nil {
		return ""
	}
	return dubTargetRuleName(t.PackageName, t.TargetName)
}

func dubTargetRuleName(packageName, targetName string) string {
	name := firstNonEmpty(subpackageName(packageName), targetName, packageName)
	if name == "" {
		return ""
	}
	return sanitizeName(name)
}

func dubPackageNameRuleName(packageName string) string {
	return sanitizeName(firstNonEmpty(subpackageName(packageName), strings.ReplaceAll(packageName, ":", "_")))
}

func subpackageName(packageName string) string {
	if idx := strings.LastIndex(packageName, ":"); idx >= 0 && idx+1 < len(packageName) {
		return packageName[idx+1:]
	}
	return ""
}

func (t *dubBuildTarget) isLibrary() bool {
	if t == nil {
		return false
	}
	switch strings.ToLower(string(t.TargetType)) {
	case "", "autodetect", "library", "sourcelibrary", "staticlibrary", "dynamiclibrary":
		return true
	default:
		return false
	}
}

func (t *dubBuildTarget) isExecutable() bool {
	if t == nil {
		return false
	}
	switch strings.ToLower(string(t.TargetType)) {
	case "executable":
		return true
	default:
		return false
	}
}

func collectDubLockSources(root string) []string {
	type dubPackageFiles struct {
		selection string
		dubJSON   string
		dubSDL    string
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
		case "dub.json":
			files.dubJSON = name
		case "dub.sdl":
			files.dubSDL = name
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
		if files.dubJSON != "" && files.dubSDL != "" {
			log.Printf("gazelle_d: both dub.json and dub.sdl found in %s; using dub.json", relFromRoot(root, dir))
		}
		selectedManifest := selectedDubManifest(files.dubJSON, files.dubSDL)
		if selectedManifest != "" {
			if label := dubLockSourceLabel(root, selectedManifest); label != "" {
				srcs = append(srcs, label)
			}
		}
		if files.selection != "" {
			if label := dubLockSourceLabel(root, files.selection); label != "" {
				srcs = append(srcs, label)
			}
		}
	}
	return uniqueSorted(srcs)
}

func localDubLockSources(regularFiles []string) []string {
	var srcs []string
	if hasRegularFile(regularFiles, "dub.json") && hasRegularFile(regularFiles, "dub.sdl") {
		log.Print("gazelle_d: both dub.json and dub.sdl found in current package; using dub.json")
	}
	if manifest := selectedLocalDubManifest(regularFiles); manifest != "" {
		srcs = append(srcs, manifest)
	}
	if hasRegularFile(regularFiles, "dub.selections.json") {
		srcs = append(srcs, "dub.selections.json")
	}
	return uniqueSorted(srcs)
}

func selectedLocalDubManifest(regularFiles []string) string {
	if hasRegularFile(regularFiles, "dub.json") {
		return "dub.json"
	}
	if hasRegularFile(regularFiles, "dub.sdl") {
		return "dub.sdl"
	}
	return ""
}

func selectedDubManifest(dubJSON, dubSDL string) string {
	if dubJSON != "" {
		return dubJSON
	}
	return dubSDL
}

func (t *dubBuildTarget) dependencyLabels() []string {
	if t == nil {
		return nil
	}
	return uniqueSorted(t.recipeDeps)
}

func (t *dubBuildTarget) collectFiles(dir, rel string) []dFile {
	srcs := uniqueSorted(t.recipeSourceFiles)
	dFiles := make([]dFile, 0, len(srcs))
	for _, name := range srcs {
		if isDSource(name) {
			dFiles = append(dFiles, dFile{name: name, module: fallbackModule(rel, name)})
		}
	}
	return dFiles
}

func applyDubAttrs(r *rule.Rule, target *dubBuildTarget, dir string) {
	if target == nil {
		return
	}
	if imports := target.effectiveImportPaths(dir); len(imports) > 0 {
		r.SetAttr("imports", imports)
	}
	if stringImports := cleanRels(target.StringImportPaths); len(stringImports) > 0 {
		r.SetAttr("string_imports", stringImports)
	}
	if versions := uniqueSorted(target.Versions); len(versions) > 0 {
		r.SetAttr("versions", versions)
	}
	if linkopts := target.linkopts(); !linkopts.IsEmpty() {
		r.SetAttr("linkopts", linkopts)
	}
}

func addDubDependencyDeps(r *rule.Rule, target *dubBuildTarget) {
	manifestDeps := target.dependencyLabels()
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

func (t *dubBuildTarget) effectiveImportPaths(dir string) []string {
	if len(t.ImportPaths) > 0 {
		return cleanRels(t.ImportPaths)
	}
	return nil
}

func (t *dubBuildTarget) linkopts() rule.PlatformStrings {
	if t == nil {
		return rule.PlatformStrings{}
	}
	return rule.PlatformStrings{Generic: uniqueSorted(append(libsToLinkopts(t.recipeLibs), t.recipeLFlags...))}
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

func setDubSrcsAttr(r *rule.Rule, target *dubBuildTarget) {
	if target == nil || target.SrcsAttr == nil {
		return
	}
	r.SetAttr("srcs", target.SrcsAttr)
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

func testName(rel, mainTarget string) string {
	base := packageBase(rel)
	if base == "" {
		return mainTarget + "_test"
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

func firstNonEmptySlice(values ...[]string) []string {
	for _, v := range values {
		if len(v) > 0 {
			return v
		}
	}
	return nil
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

func relUnder(root, name string) string {
	if name == "" {
		return ""
	}
	if !filepath.IsAbs(name) {
		return cleanRel(name)
	}
	if !pathWithin(root, name) {
		return ""
	}
	return relFromRoot(root, name)
}

func localRelsUnder(root string, names []string) []string {
	rels := make([]string, 0, len(names))
	for _, name := range names {
		if rel := relUnder(root, name); rel != "" {
			rels = append(rels, rel)
		}
	}
	return uniqueSorted(rels)
}

func localOrCleanRelsUnder(root string, names []string) []string {
	rels := make([]string, 0, len(names))
	for _, name := range names {
		if filepath.IsAbs(name) {
			if rel := relUnder(root, name); rel != "" {
				rels = append(rels, rel)
			}
			continue
		}
		if rel := cleanRel(name); rel != "" {
			rels = append(rels, rel)
		}
	}
	return uniqueSorted(rels)
}

func pathWithin(root, name string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	nameAbs, err := filepath.Abs(name)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, nameAbs)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func samePath(a, b string) bool {
	aAbs, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	bAbs, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return filepath.Clean(aAbs) == filepath.Clean(bAbs)
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

func fileExists(name string) bool {
	info, err := os.Stat(name)
	return err == nil && !info.IsDir()
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
