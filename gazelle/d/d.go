package d

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
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
			MergeableAttrs: map[string]bool{"srcs": true, "deps": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
		"d_proto_library": {
			NonEmptyAttrs:  map[string]bool{"deps": true},
			MergeableAttrs: map[string]bool{"deps": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
		"d_binary": {
			NonEmptyAttrs:  map[string]bool{"srcs": true},
			MergeableAttrs: map[string]bool{"srcs": true, "deps": true, "data": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
		"d_test": {
			NonEmptyAttrs:  map[string]bool{"srcs": true},
			MergeableAttrs: map[string]bool{"srcs": true, "deps": true, "data": true},
			ResolveAttrs:   map[string]bool{"deps": true},
		},
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
	return []rule.LoadInfo{{
		Name:    repoName + "//d:defs.bzl",
		Symbols: []string{"d_binary", "d_library", "d_proto_library", "d_test"},
	}}
}

func (*dLang) GenerateRules(args language.GenerateArgs) language.GenerateResult {
	files := collectDFiles(args.Rel, args.RegularFiles)
	if len(files) == 0 && len(args.OtherGen) == 0 {
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
	gen, imports = appendProtoRules(gen, imports, args)
	libName := libraryName(args.Rel)
	if len(libSrcs) > 0 {
		r := newDRule("d_library", libName, libSrcs, args)
		r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(allModules))
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(allImports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(allImports))
	}

	for _, f := range bins {
		r := newDRule("d_binary", binaryName(f.name), []string{f.name}, args)
		if len(libSrcs) > 0 {
			r.SetAttr("deps", []string{":" + libName})
		}
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
		if len(libSrcs) > 0 {
			r.SetAttr("deps", []string{":" + libName})
		}
		r.SetPrivateAttr(modulesPrivateKey, uniqueSorted(testModules))
		r.SetPrivateAttr(importsPrivateKey, uniqueSorted(testImports))
		gen = append(gen, r)
		imports = append(imports, uniqueSorted(testImports))
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

func collectDFiles(rel string, files []string) []dFile {
	var dFiles []dFile
	for _, name := range files {
		if strings.HasSuffix(name, ".d") || strings.HasSuffix(name, ".di") {
			dFiles = append(dFiles, dFile{name: name, module: fallbackModule(rel, name)})
		}
	}
	sort.Slice(dFiles, func(i, j int) bool { return dFiles[i].name < dFiles[j].name })
	return dFiles
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
