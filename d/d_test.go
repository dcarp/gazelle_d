package d

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
	bzl "github.com/bazelbuild/buildtools/build"
)

func TestGenerateRulesRequiresDubRecipe(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "math.d", "module app.math;\nimport dep.util;\nunittest { assert(true); }\n")
	write(t, dir, "main.d", "module app.main;\nimport app.math;\nvoid main() {}\n")

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "app",
		RegularFiles: []string{"main.d", "math.d"},
	})

	if len(res.Gen) != 0 {
		t.Fatalf("got %d generated rules, want none without DUB recipe", len(res.Gen))
	}
}

func TestGenerateRulesWithDubSDL(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "app"`)
	write(t, dir, "math.d", "module app.math;\nimport dep.util;\nunittest { assert(true); }\n")
	write(t, dir, "main.d", "module app.main;\nimport app.math;\nvoid main() {}\n")
	setFakeDubDescribe(t, dir, `{"name":"app","targetType":"executable","sourceFiles":["math.d","main.d"]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "app",
		RegularFiles: []string{"dub.sdl", "main.d", "math.d"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want exports_files and D target", len(res.Gen))
	}
	assertExportsFilesRule(t, res.Gen[0], []string{"dub.sdl"})
	assertRule(t, res.Gen[1], "d_binary", "app", []string{"main.d", "math.d"})

	if got := res.Imports[1].([]string); !reflect.DeepEqual(got, []string{"app.math", "dep.util"}) {
		t.Fatalf("binary imports = %#v", got)
	}
}

func TestGenerateRootRulesWithDubSDL(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "rootapp"`)
	write(t, dir, "source/app.d", "module app;\nvoid main() {}\n")
	setFakeDubDescribe(t, dir, `{"name":"rootapp","targetName":"rootapp","targetType":"executable","importPaths":["source/"],"sourceFiles":["source/app.d"]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.sdl"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want root D target plus lock target", len(res.Gen))
	}
	assertRule(t, res.Gen[0], "d_binary", "rootapp", []string{"source/app.d"})
	assertDubLockDependenciesRule(t, res.Gen[1], []string{"dub.sdl"})
}

func TestGenerateProtoRules(t *testing.T) {
	proto := rule.NewRule("proto_library", "addressbook_proto")
	proto.SetAttr("srcs", []string{"addressbook.proto"})
	dir := t.TempDir()
	setFakeDubDescribe(t, dir, `{"name":"proto","sourceFiles":[]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "proto",
		RegularFiles: []string{"dub.sdl"},
		OtherGen:     []*rule.Rule{proto},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want exports_files and proto target", len(res.Gen))
	}
	assertExportsFilesRule(t, res.Gen[0], []string{"dub.sdl"})
	r := res.Gen[1]
	if r.Kind() != "d_proto_library" || r.Name() != "addressbook_d_proto" {
		t.Fatalf("rule = %s %s, want d_proto_library addressbook_d_proto", r.Kind(), r.Name())
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, []string{":addressbook_proto"}) {
		t.Fatalf("deps = %#v, want proto dependency", got)
	}
}

func TestGenerateDubStaticLibrary(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{
    "name": "libasync",
    "targetName": "async",
    "targetType": "staticLibrary",
    "libs-linux": ["rt", "resolv"],
    "libs-windows": ["advapi32", "user32", "ws2_32"],
    "dependencies": {
        "memutils": { "version": "~>1.0.1" }
    }
}`)
	write(t, dir, "source/libasync/package.d", "module libasync;\npublic import libasync.events;\n")
	write(t, dir, "source/libasync/events.d", "module libasync.events;\nimport memutils.vector;\n")
	setFakeDubDescribe(t, dir, `{
  "name": "libasync",
  "targetName": "async",
  "targetType": "staticLibrary",
  "importPaths": ["source/"],
  "dependencies": {"memutils": "~>1.0.1"}
}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want 2", len(res.Gen))
	}
	r := res.Gen[0]
	assertRuleName(t, r, "d_library", "async")
	assertGlob(t, r, []string{"source/**/*.d"}, nil)
	if got := r.AttrStrings("imports"); !reflect.DeepEqual(got, []string{"source"}) {
		t.Fatalf("imports = %#v, want default DUB source import path", got)
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, []string{"@dub//memutils"}) {
		t.Fatalf("deps = %#v, want DUB dependency", got)
	}
	if got := res.Imports[0].([]string); !reflect.DeepEqual(got, []string{"libasync.events", "memutils.vector"}) {
		t.Fatalf("rule imports = %#v, want parsed D imports", got)
	}
	assertDubLockDependenciesRule(t, res.Gen[1], []string{"dub.json"})
}

func TestGenerateDubSourcePathsUseGlobAndExclude(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "paths"}`)
	write(t, dir, "src/kept.d", "module paths.kept;\nimport kept.dep;\nunittest { assert(true); }\n")
	write(t, dir, "src/skip.d", "module paths.skip;\nimport skipped.dep;\nunittest { assert(true); }\n")
	write(t, dir, "generated/gen.di", "module paths.gen;\n")
	setFakeDubDescribe(t, dir, `{
  "name": "paths",
  "targetType": "library",
  "sourcePaths": ["src", "generated"],
  "excludedSourceFiles": ["src/skip.d", "src/vendor/**/*.d"]
}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 3 {
		t.Fatalf("got %d generated rules, want library, test, and lock target", len(res.Gen))
	}
	wantGlobs := []rule.GlobValue{
		{
			Patterns: []string{"generated/**/*.di"},
		},
		{
			Patterns: []string{"src/**/*.d"},
			Excludes: []string{"src/skip.d", "src/vendor/**/*.d"},
		},
	}
	r := res.Gen[0]
	assertRuleName(t, r, "d_library", "paths")
	assertGlobConcat(t, r, wantGlobs)
	if got := res.Imports[0].([]string); !reflect.DeepEqual(got, []string{"kept.dep"}) {
		t.Fatalf("rule imports = %#v, want excluded source imports removed", got)
	}
	testRule := res.Gen[1]
	assertRuleName(t, testRule, "d_test", "paths_test")
	assertGlobConcat(t, testRule, wantGlobs)
	assertDubLockDependenciesRule(t, res.Gen[2], []string{"dub.json"})
}

func TestGenerateDubLibraryKeepsMainSourceInLibrary(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "ae"
targetType "library"`)
	write(t, dir, "utils/main.d", "module ae.utils.main;\nvoid main() {}\n")
	setFakeDubDescribe(t, dir, `{"name":"ae","targetName":"ae","targetType":"library","sourceFiles":["utils/main.d"]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.sdl"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want library plus lock target", len(res.Gen))
	}
	assertRule(t, res.Gen[0], "d_library", "ae", []string{"utils/main.d"})
	assertDubLockDependenciesRule(t, res.Gen[1], []string{"dub.sdl"})
}

func TestGenerateRootTestUsesMainTargetName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "ae"
targetType "library"`)
	write(t, dir, "utils/aa.d", "module ae.utils.aa;\nunittest { assert(true); }\n")
	setFakeDubDescribe(t, dir, `{"name":"ae","targetName":"ae","targetType":"library","sourceFiles":["utils/aa.d"]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.sdl"},
	})

	if len(res.Gen) != 3 {
		t.Fatalf("got %d generated rules, want library, test, and lock target", len(res.Gen))
	}
	assertRule(t, res.Gen[0], "d_library", "ae", []string{"utils/aa.d"})
	assertRule(t, res.Gen[1], "d_test", "ae_test", []string{"utils/aa.d"})
	assertDubLockDependenciesRule(t, res.Gen[2], []string{"dub.sdl"})
}

func TestGenerateDubSubpackageTargets(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "root"
targetType "library"

subPackage {
    name "extra"
    targetType "library"
}`)
	write(t, dir, "source/root.d", "module root;\n")
	write(t, dir, "source/extra.d", "module root.extra;\n")
	setFakeDubDescribe(t, dir, `{
  "name": "root",
  "targetType": "library",
  "sourceFiles": ["source/root.d"],
  "subPackages": [{
    "name": "extra",
    "targetType": "library",
    "dependencies": {"root": "*"},
    "sourceFiles": ["source/extra.d"]
  }]
}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.sdl"},
	})

	if len(res.Gen) != 3 {
		t.Fatalf("got %d generated rules, want root library, subpackage library, and lock target", len(res.Gen))
	}
	assertRule(t, res.Gen[0], "d_library", "root", []string{"source/root.d"})
	assertRule(t, res.Gen[1], "d_library", "extra", []string{"source/extra.d"})
	if got := res.Gen[1].AttrStrings("deps"); !reflect.DeepEqual(got, []string{":root"}) {
		t.Fatalf("subpackage deps = %#v, want root library dep", got)
	}
	assertDubLockDependenciesRule(t, res.Gen[2], []string{"dub.sdl"})
}

func TestGenerateDubSubpackageDoesNotMaskSameNamedExternalDependency(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "root"
targetType "library"

dependency "extra" version="~>1.0.0"

subPackage {
    name "extra"
    targetType "library"
}`)
	write(t, dir, "source/root.d", "module root;\n")
	write(t, dir, "source/subpackage_extra.d", "module root.extra;\nimport extra;\n")
	setFakeDubDescribe(t, dir, `{
  "name": "root",
  "targetType": "library",
  "sourceFiles": ["source/root.d"],
  "subPackages": [{
    "name": "extra",
    "targetType": "library",
    "dependencies": {"extra": "~>1.0.0"},
    "sourceFiles": ["source/subpackage_extra.d"]
  }]
}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.sdl"},
	})

	if len(res.Gen) != 3 {
		t.Fatalf("got %d generated rules, want root library, subpackage library, and lock target", len(res.Gen))
	}
	assertRule(t, res.Gen[1], "d_library", "extra", []string{"source/subpackage_extra.d"})
	if got := res.Gen[1].AttrStrings("deps"); !reflect.DeepEqual(got, []string{"@dub//extra"}) {
		t.Fatalf("subpackage deps = %#v, want external dependency label", got)
	}
}

func TestGenerateDubPlainDependencyNameStaysExternalWhenSubpackageHasSameTargetName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "ae"
targetType "library"

dependency "openssl" version="~>3.0.0"
dependency "zlib" version="~>1.0.0"

subPackage { name "openssl" }
subPackage { name "zlib" }`)
	write(t, dir, "source/ae.d", "module ae;\nimport openssl;\nimport zlib;\n")
	setFakeDubDescribe(t, dir, `{
  "name": "ae",
  "targetType": "library",
  "dependencies": {"openssl": "~>3.0.0", "zlib": "~>1.0.0"},
  "sourceFiles": ["source/ae.d"],
  "subPackages": [{"name": "openssl", "sourceFiles": []}, {"name": "zlib", "sourceFiles": []}]
}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.sdl"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want D target and lock target", len(res.Gen))
	}
	assertRule(t, res.Gen[0], "d_library", "ae", []string{"source/ae.d"})
	if got := res.Gen[0].AttrStrings("deps"); !reflect.DeepEqual(got, []string{"@dub//openssl", "@dub//zlib"}) {
		t.Fatalf("deps = %#v, want external dependency labels", got)
	}
}

func TestGenerateDubQualifiedSubpackageDependencyIsLocal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "ae"}`)
	write(t, dir, "source/openssl.d", "module ae.openssl;\nimport ae.zlib;\n")
	write(t, dir, "source/zlib.d", "module ae.zlib;\n")
	setFakeDubDescribe(t, dir, `{
  "name": "ae",
  "sourceFiles": [],
  "subPackages": [
    {
      "name": "zlib",
      "targetType": "library",
      "sourceFiles": ["source/zlib.d"]
    },
    {
      "name": "openssl",
      "targetType": "library",
      "dependencies": {"ae:zlib": "*"},
      "sourceFiles": ["source/openssl.d"]
    }
  ]
}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 3 {
		t.Fatalf("got %d generated rules, want subpackage libraries and lock target", len(res.Gen))
	}
	assertRule(t, res.Gen[0], "d_library", "openssl", []string{"source/openssl.d"})
	if got := res.Gen[0].AttrStrings("deps"); !reflect.DeepEqual(got, []string{":zlib"}) {
		t.Fatalf("qualified subpackage deps = %#v, want local zlib label", got)
	}
	assertRule(t, res.Gen[1], "d_library", "zlib", []string{"source/zlib.d"})
}

func TestGenerateDubConvertIncludesMatchingPlatformFields(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "platformed"}`)
	write(t, dir, "source/base.d", "module platformed.base;\n")
	write(t, dir, "source/posix.d", "module platformed.posix;\n")
	write(t, dir, "source/windows.d", "module platformed.windows;\n")
	setFakeDubDescribe(t, dir, `{
  "name": "platformed",
  "targetType": "library",
  "sourceFiles": ["source/base.d"],
  "sourceFiles-posix": ["source/posix.d"],
  "sourceFiles-windows": ["source/windows.d"],
  "libs-posix": ["rt"],
  "libs-windows": ["ws2_32"]
}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want library and lock target", len(res.Gen))
	}
	r := res.Gen[0]
	assertRule(t, r, "d_library", "platformed", []string{"source/base.d", "source/posix.d"})
	if got := r.AttrStrings("linkopts"); !reflect.DeepEqual(got, []string{"-lrt"}) {
		t.Fatalf("linkopts = %#v, want matching platform libs only", got)
	}
}

func TestExternalDependencySuppressesSameNamedLocalImportResolution(t *testing.T) {
	deps := map[string]bool{"@dub//openssl": true}
	if !hasExternalDepForImport(deps, "openssl") {
		t.Fatalf("openssl import should be covered by @dub//openssl")
	}
	if !hasExternalDepForImport(deps, "openssl.ssl") {
		t.Fatalf("openssl.ssl import should be covered by @dub//openssl")
	}
	if hasExternalDepForImport(deps, "openssl_extra") {
		t.Fatalf("openssl_extra import should not be covered by @dub//openssl")
	}
}

func TestResolveDubGeneratedRuleWarnsMissingLocalDepsWithoutMutatingDeps(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "ae"`)
	write(t, dir, "source/ae.d", "module ae;\nimport ae.utils.zlib;\n")
	setFakeDubDescribe(t, dir, `{"name":"ae","targetType":"library","sourceFiles":["source/ae.d"]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.sdl"},
	})
	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want D target and lock target", len(res.Gen))
	}

	provider := rule.NewRule("d_library", "zlib")
	provider.SetAttr("srcs", []string{"utils/zlib.d"})
	provider.SetPrivateAttr(modulesPrivateKey, []string{"ae.utils.zlib"})

	cfg := config.New()
	ix := resolve.NewRuleIndex(func(r *rule.Rule, pkgRel string) resolve.Resolver {
		if isDRule(r.Kind()) {
			return lang.(resolve.Resolver)
		}
		return nil
	})
	ix.AddRule(cfg, provider, &rule.File{Pkg: ""})
	ix.Finish()

	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	lang.(resolve.Resolver).Resolve(cfg, ix, nil, res.Gen[0], res.Imports[0], label.New("", "", "ae"))

	if got := res.Gen[0].AttrStrings("deps"); len(got) != 0 {
		t.Fatalf("deps = %#v, want no Gazelle-added deps for DUB-generated rule", got)
	}
	logText := logs.String()
	for _, want := range []string{"//:ae missing dep :zlib in dub.json/dub.sdl for imports [ae.utils.zlib]"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log = %q, want substring %q", logText, want)
		}
	}
}

func TestGenerateDubExecutableWithTrailingCommas(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{
    "name": "netcat",
    "targetName": "ncat",
    "dependencies": {
        "libasync": { "path": "../../" },
        "docopt": "~>0.6.1-b.5",
    },
}`)
	write(t, dir, "source/app.d", "module app;\nimport libasync;\nimport docopt;\nvoid main() {}\n")
	repoRoot := filepath.Dir(dir)
	setFakeDubDescribe(t, dir, `{
  "name": "netcat",
  "targetName": "ncat",
  "targetType": "executable",
  "dependencies": {"libasync": {"path": "../../"}, "docopt": "~>0.6.1-b.5"},
  "importPaths": ["source/"],
  "sourceFiles": ["source/app.d"]
}`)

	lang := NewLanguage()
	cfg := config.New()
	cfg.RepoRoot = repoRoot
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       cfg,
		Dir:          dir,
		Rel:          "examples/netcat",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want exports_files and D target", len(res.Gen))
	}
	assertExportsFilesRule(t, res.Gen[0], []string{"dub.json"})
	r := res.Gen[1]
	assertRule(t, r, "d_binary", "ncat", []string{"source/app.d"})
	if got := r.AttrStrings("imports"); !reflect.DeepEqual(got, []string{"source"}) {
		t.Fatalf("imports = %#v, want default DUB source import path", got)
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, []string{"@dub//docopt", "@dub//libasync"}) {
		t.Fatalf("deps = %#v, want explicit DUB dependency labels", got)
	}
	if got := res.Imports[1].([]string); !reflect.DeepEqual(got, []string{"docopt", "libasync"}) {
		t.Fatalf("rule imports = %#v, want parsed D imports", got)
	}
}

func TestGenerateRulesUsesDubConvert(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "ignored", "targetName": "ignored", "sourceFiles": ["source/ignored.d"]}`)
	write(t, dir, "source/from_describe.d", "module libasync.from_describe;\nimport memutils.vector;\n")
	write(t, dir, "source/ignored.d", "module ignored;\n")

	recipe := strings.ReplaceAll(`{
  "name": "libasync",
  "targetName": "from_convert",
  "targetType": "staticLibrary",
  "dependencies": {"memutils": "~>1.0.0", "localdep": {"path": "localdep"}},
  "libs": ["resolv"],
  "versions": ["Have_libasync", "Have_memutils"],
  "importPaths": ["__DIR__/source/", "/tmp/dub/memutils/source/"],
  "stringImportPaths": [],
  "sourceFiles": ["__DIR__/source/from_describe.d"]
}`, "__DIR__", filepath.ToSlash(dir))
	setFakeDubDescribe(t, dir, recipe)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want 2", len(res.Gen))
	}
	r := res.Gen[0]
	assertRule(t, r, "d_library", "from_convert", []string{"source/from_describe.d"})
	if got := r.AttrStrings("imports"); !reflect.DeepEqual(got, []string{"source"}) {
		t.Fatalf("imports = %#v, want convert-local import path", got)
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, []string{"@dub//localdep", "@dub//memutils"}) {
		t.Fatalf("deps = %#v, want explicit DUB dependency labels", got)
	}
	if got := r.AttrStrings("versions"); !reflect.DeepEqual(got, []string{"Have_libasync", "Have_memutils"}) {
		t.Fatalf("versions = %#v, want convert recipe versions", got)
	}
	if got := r.AttrStrings("linkopts"); !reflect.DeepEqual(got, []string{"-lresolv"}) {
		t.Fatalf("linkopts = %#v, want convert libs", got)
	}
	assertDubLockDependenciesRule(t, res.Gen[1], []string{"dub.json"})
}

func TestGenerateDubLockDependenciesWithoutSources(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "deps_only", "dependencies": {"memutils": "~>1.0.1"}}`)
	setFakeDubDescribe(t, dir, `{"name":"deps_only","sourceFiles":[]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 1 {
		t.Fatalf("got %d generated rules, want 1", len(res.Gen))
	}
	assertDubLockDependenciesRule(t, res.Gen[0], []string{"dub.json"})
}

func TestGenerateDubLockDependenciesCollectsNestedInputs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "root"}`)
	write(t, dir, "examples/netcat/dub.json", `{"name": "netcat"}`)
	write(t, dir, "examples/netcat/dub.selections.json", `{"fileVersion": 1, "versions": {"docopt": "0.6.1"}}`)
	write(t, dir, "tests/dub.sdl", `name "tests"`)
	setFakeDubDescribe(t, dir, `{"name":"root","sourceFiles":[]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 1 {
		t.Fatalf("got %d generated rules, want 1", len(res.Gen))
	}
	assertDubLockDependenciesRule(t, res.Gen[0], []string{
		"//examples/netcat:dub.json",
		"//examples/netcat:dub.selections.json",
		"//tests:dub.sdl",
		"dub.json",
	})
}

func TestGenerateRulesExportsDubSelectionsWithManifest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.sdl", `name "tool"`)
	write(t, dir, "dub.selections.json", `{"fileVersion": 1, "versions": {}}`)
	write(t, dir, "app.d", "module app;\nvoid main() {}\n")
	setFakeDubDescribe(t, dir, `{"name":"tool","targetName":"tool","targetType":"executable","sourceFiles":["app.d"]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "demo/tool",
		RegularFiles: []string{"dub.sdl", "dub.selections.json", "app.d"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want exports_files and nested DUB target", len(res.Gen))
	}
	assertExportsFilesRule(t, res.Gen[0], []string{"dub.sdl", "dub.selections.json"})
	assertRule(t, res.Gen[1], "d_binary", "tool", []string{"app.d"})
}

func TestDubLockSourcesWarnAndPreferDubJSONWhenBothManifestsExist(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "demo/dub.json", `{"name": "demo-json"}`)
	write(t, dir, "demo/dub.sdl", `name "demo-sdl"`)
	write(t, dir, "demo/dub.selections.json", `{"fileVersion": 1, "versions": {}}`)

	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	got := collectDubLockSources(dir)
	want := []string{
		"//demo:dub.json",
		"//demo:dub.selections.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dub lock sources = %#v, want %#v", got, want)
	}
	if logText := logs.String(); !strings.Contains(logText, "both dub.json and dub.sdl found in demo; using dub.json") {
		t.Fatalf("log = %q, want manifest conflict warning", logText)
	}
}

func TestGenerateNestedDubRecipeUnderRootDubPackage(t *testing.T) {
	repoRoot := t.TempDir()
	write(t, repoRoot, "dub.sdl", `name "root"`)
	dir := filepath.Join(repoRoot, "demo", "tool")
	write(t, dir, "dub.sdl", `name "tool"`)
	write(t, dir, "app.d", "module app;\nvoid main() {}\n")
	setFakeDubDescribe(t, dir, `{"name":"tool","targetName":"tool","targetType":"executable","sourceFiles":["app.d"]}`)

	lang := NewLanguage()
	cfg := config.New()
	cfg.RepoRoot = repoRoot
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       cfg,
		Dir:          dir,
		Rel:          "demo/tool",
		RegularFiles: []string{"dub.sdl", "app.d"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want exports_files and nested DUB target", len(res.Gen))
	}
	assertExportsFilesRule(t, res.Gen[0], []string{"dub.sdl"})
	assertRule(t, res.Gen[1], "d_binary", "tool", []string{"app.d"})
}

func TestSkipDubLockDependenciesOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "example", "dependencies": {"docopt": "~>0.6.1-b.5"}}`)
	write(t, dir, "source/app.d", "module app;\nimport docopt;\nvoid main() {}\n")
	setFakeDubDescribe(t, dir, `{"name":"example","targetType":"executable","dependencies":{"docopt":"~>0.6.1-b.5"},"sourceFiles":["source/app.d"]}`)

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "examples/netcat",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want exports_files and the D target", len(res.Gen))
	}
	if res.Gen[0].Kind() == "dub_lock_dependencies" || res.Gen[1].Kind() == "dub_lock_dependencies" {
		t.Fatalf("generated dub_lock_dependencies outside repository root")
	}
}

func TestSkipDubOwnedSourceSubdirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "libasync", "targetName": "async", "targetType": "staticLibrary"}`)
	write(t, dir, "source/libasync/package.d", "module libasync;\n")

	cfg := config.New()
	cfg.RepoRoot = dir
	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       cfg,
		Dir:          filepath.Join(dir, "source", "libasync"),
		Rel:          "source/libasync",
		RegularFiles: []string{"package.d"},
	})

	if len(res.Gen) != 0 {
		t.Fatalf("got %d generated rules, want none under DUB-owned source directory", len(res.Gen))
	}
}

func TestSkipSourceDirectoryEvenWithoutParentConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.d", "module libasync;\n")

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "source/libasync",
		RegularFiles: []string{"package.d"},
	})

	if len(res.Gen) != 0 {
		t.Fatalf("got %d generated rules, want none without local DUB recipe", len(res.Gen))
	}
}

func TestLoadsIncludeProto(t *testing.T) {
	loads := NewLanguage().Loads()
	if len(loads) != 2 {
		t.Fatalf("got %d loads, want 2", len(loads))
	}
	want := []string{"d_binary", "d_library", "d_proto_library", "d_test"}
	if !reflect.DeepEqual(loads[0].Symbols, want) {
		t.Fatalf("load symbols = %#v, want %#v", loads[0].Symbols, want)
	}
	if loads[1].Name != "@rules_d//dub:defs.bzl" || !reflect.DeepEqual(loads[1].Symbols, []string{"dub_lock_dependencies"}) {
		t.Fatalf("dub load = %#v, want dub_lock_dependencies load", loads[1])
	}
}

func TestParseImports(t *testing.T) {
	src := stripDCommentsAndStrings(`
module sample;
import std.stdio;
static import foo.bar;
public import aliasName = dep.one, dep.two : Thing;
mixin("import ignored.value;");
// import ignored.comment;
`)
	got := parseImports(src)
	want := []string{"dep.one", "dep.two", "foo.bar", "std.stdio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseImports() = %#v, want %#v", got, want)
	}
}

func TestImportsFallbackToSourcePaths(t *testing.T) {
	f := &rule.File{Pkg: "lib"}
	r := rule.NewRule("d_library", "lib")
	r.SetAttr("srcs", []string{"foo/bar.d", "baz.di"})

	got := NewLanguage().Imports(config.New(), r, f)
	want := []string{"lib.baz", "lib.foo.bar"}
	if len(got) != len(want) {
		t.Fatalf("got %d import specs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Lang != langName || got[i].Imp != want[i] {
			t.Fatalf("import spec %d = %#v, want %q", i, got[i], want[i])
		}
	}
}

func TestImportsOnlyIndexesLibraries(t *testing.T) {
	f := &rule.File{Pkg: "lib"}
	for _, kind := range []string{"d_binary", "d_test", "d_proto_library"} {
		r := rule.NewRule(kind, "target")
		r.SetAttr("srcs", []string{"foo/bar.d"})
		if got := NewLanguage().Imports(config.New(), r, f); got != nil {
			t.Fatalf("%s imports = %#v, want nil", kind, got)
		}
	}
}

func assertRule(t *testing.T, r *rule.Rule, kind, name string, srcs []string) {
	t.Helper()
	assertRuleName(t, r, kind, name)
	if got := r.AttrStrings("srcs"); !reflect.DeepEqual(got, srcs) {
		t.Fatalf("%s srcs = %#v, want %#v", name, got, srcs)
	}
}

func assertRuleName(t *testing.T, r *rule.Rule, kind, name string) {
	t.Helper()
	if r.Kind() != kind || r.Name() != name {
		t.Fatalf("rule = %s %s, want %s %s", r.Kind(), r.Name(), kind, name)
	}
}

func assertGlob(t *testing.T, r *rule.Rule, patterns, excludes []string) {
	t.Helper()
	got, ok := rule.ParseGlobExpr(r.Attr("srcs"))
	if !ok {
		t.Fatalf("%s srcs = %#v, want glob", r.Name(), r.Attr("srcs"))
	}
	if !reflect.DeepEqual(got.Patterns, patterns) {
		t.Fatalf("%s glob patterns = %#v, want %#v", r.Name(), got.Patterns, patterns)
	}
	if !reflect.DeepEqual(got.Excludes, excludes) {
		t.Fatalf("%s glob excludes = %#v, want %#v", r.Name(), got.Excludes, excludes)
	}
}

func assertGlobConcat(t *testing.T, r *rule.Rule, want []rule.GlobValue) {
	t.Helper()
	got, ok := flattenGlobExpr(r.Attr("srcs"))
	if !ok {
		t.Fatalf("%s srcs = %#v, want glob concatenation", r.Name(), r.Attr("srcs"))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s glob concat = %#v, want %#v", r.Name(), got, want)
	}
}

func flattenGlobExpr(expr bzl.Expr) ([]rule.GlobValue, bool) {
	if glob, ok := rule.ParseGlobExpr(expr); ok {
		return []rule.GlobValue{glob}, true
	}
	binary, ok := expr.(*bzl.BinaryExpr)
	if !ok || binary.Op != "+" {
		return nil, false
	}
	left, ok := flattenGlobExpr(binary.X)
	if !ok {
		return nil, false
	}
	right, ok := flattenGlobExpr(binary.Y)
	if !ok {
		return nil, false
	}
	return append(left, right...), true
}

func assertDubLockDependenciesRule(t *testing.T, r *rule.Rule, srcs []string) {
	t.Helper()
	if r.Kind() != "dub_lock_dependencies" || r.Name() != "dub_dependencies" {
		t.Fatalf("rule = %s %s, want dub_lock_dependencies dub_dependencies", r.Kind(), r.Name())
	}
	if got := r.AttrStrings("srcs"); !reflect.DeepEqual(got, srcs) {
		t.Fatalf("dub lock srcs = %#v, want %#v", got, srcs)
	}
	if got := r.AttrString("dub_selections_lock"); got != "dub.selections.lock.json" {
		t.Fatalf("dub lock output = %q, want dub.selections.lock.json", got)
	}
	if got := r.AttrStrings("tags"); !reflect.DeepEqual(got, []string{"manual"}) {
		t.Fatalf("dub lock tags = %#v, want manual", got)
	}
}

func assertExportsFilesRule(t *testing.T, r *rule.Rule, srcs []string) {
	t.Helper()
	if r.Kind() != "exports_files" {
		t.Fatalf("rule = %s, want exports_files", r.Kind())
	}
	args := r.Args()
	if len(args) != 1 {
		t.Fatalf("exports_files args = %#v, want one list arg", args)
	}
	want := rule.ExprFromValue(srcs)
	if !reflect.DeepEqual(args[0], want) {
		t.Fatalf("exports_files arg = %#v, want %#v", args[0], want)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setFakeDubDescribe(t *testing.T, dir, describe string) {
	t.Helper()
	buildContent := fakeDubBuildFile(t, dir, describe)
	tool := filepath.Join(t.TempDir(), "generate_build_file")
	write(t, filepath.Dir(tool), filepath.Base(tool), "#!/bin/sh\ncat <<'BUILD'\n"+buildContent+"\nBUILD\n")
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GAZELLE_D_GENERATE_BUILD_FILE", tool)
	dub := filepath.Join(t.TempDir(), "dub")
	write(t, filepath.Dir(dub), filepath.Base(dub), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(dub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GAZELLE_D_DUB", dub)
}

func fakeDubBuildFile(t *testing.T, dir, describe string) string {
	t.Helper()
	recipe, ok := parseFakeDubRecipe(t, describe)
	if !ok {
		t.Fatal("failed to parse fake DUB recipe")
	}
	targets := compactDubTargets(recipe.buildTargets(dir))
	gen := rule.EmptyFile(filepath.Join(dir, "BUILD.bazel"), "")
	args := language.GenerateArgs{
		Config: config.New(),
		Dir:    dir,
	}
	var rules []*rule.Rule
	var imports []interface{}
	for _, target := range targets {
		rules, imports = appendDubTargetRules(rules, imports, target, args)
	}
	_ = imports
	for _, r := range rules {
		r.Insert(gen)
	}
	return string(gen.Format())
}

func parseFakeDubRecipe(t *testing.T, describe string) (dubRecipe, bool) {
	t.Helper()
	var recipe dubRecipe
	if err := json.Unmarshal([]byte(describe), &recipe); err != nil {
		t.Fatal(err)
	}
	return recipe, true
}
