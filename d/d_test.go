package d

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

func TestGenerateRules(t *testing.T) {
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

	if len(res.Gen) != 3 {
		t.Fatalf("got %d generated rules, want 3", len(res.Gen))
	}
	assertRule(t, res.Gen[0], "d_library", "app_d_library", []string{"math.d"})
	assertRule(t, res.Gen[1], "d_binary", "main", []string{"main.d"})
	assertRule(t, res.Gen[2], "d_test", "app_d_test", []string{"math.d"})

	if got := res.Gen[1].AttrStrings("deps"); !reflect.DeepEqual(got, []string{":app_d_library"}) {
		t.Fatalf("binary deps = %#v, want library dep", got)
	}
	if got := res.Imports[0].([]string); !reflect.DeepEqual(got, []string{"app.math", "dep.util"}) {
		t.Fatalf("library imports = %#v", got)
	}
}

func TestGenerateProtoRules(t *testing.T) {
	proto := rule.NewRule("proto_library", "addressbook_proto")
	proto.SetAttr("srcs", []string{"addressbook.proto"})

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:   config.New(),
		Dir:      t.TempDir(),
		Rel:      "proto",
		OtherGen: []*rule.Rule{proto},
	})

	if len(res.Gen) != 1 {
		t.Fatalf("got %d generated rules, want 1", len(res.Gen))
	}
	r := res.Gen[0]
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
	assertRule(t, r, "d_library", "async", []string{
		"source/libasync/events.d",
		"source/libasync/package.d",
	})
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

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "examples/netcat",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want 2", len(res.Gen))
	}
	assertExportsFilesRule(t, res.Gen[0], []string{"dub.json"})
	r := res.Gen[1]
	assertRule(t, r, "d_binary", "ncat", []string{"source/app.d"})
	if got := r.AttrStrings("imports"); !reflect.DeepEqual(got, []string{"source"}) {
		t.Fatalf("imports = %#v, want default DUB source import path", got)
	}
	if got := r.AttrStrings("deps"); !reflect.DeepEqual(got, []string{"@dub//docopt"}) {
		t.Fatalf("deps = %#v, want non-local DUB dependency only", got)
	}
	if got := res.Imports[1].([]string); !reflect.DeepEqual(got, []string{"docopt", "libasync"}) {
		t.Fatalf("rule imports = %#v, want parsed D imports", got)
	}
}

func TestGenerateDubLockDependenciesWithoutSources(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "deps_only", "dependencies": {"memutils": "~>1.0.1"}}`)

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
		"//examples/netcat:dub.selections.json",
		"//tests:dub.sdl",
		"dub.json",
	})
}

func TestSkipDubLockDependenciesOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dub.json", `{"name": "example", "dependencies": {"docopt": "~>0.6.1-b.5"}}`)
	write(t, dir, "source/app.d", "module app;\nimport docopt;\nvoid main() {}\n")

	lang := NewLanguage()
	res := lang.GenerateRules(language.GenerateArgs{
		Config:       config.New(),
		Dir:          dir,
		Rel:          "examples/netcat",
		RegularFiles: []string{"dub.json"},
	})

	if len(res.Gen) != 2 {
		t.Fatalf("got %d generated rules, want exports plus the D target", len(res.Gen))
	}
	assertExportsFilesRule(t, res.Gen[0], []string{"dub.json"})
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

func assertRule(t *testing.T, r *rule.Rule, kind, name string, srcs []string) {
	t.Helper()
	if r.Kind() != kind || r.Name() != name {
		t.Fatalf("rule = %s %s, want %s %s", r.Kind(), r.Name(), kind, name)
	}
	if got := r.AttrStrings("srcs"); !reflect.DeepEqual(got, srcs) {
		t.Fatalf("%s srcs = %#v, want %#v", name, got, srcs)
	}
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
