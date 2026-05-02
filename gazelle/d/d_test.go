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

func TestLoadsIncludeProto(t *testing.T) {
	loads := NewLanguage().Loads()
	if len(loads) != 1 {
		t.Fatalf("got %d loads, want 1", len(loads))
	}
	want := []string{"d_binary", "d_library", "d_proto_library", "d_test"}
	if !reflect.DeepEqual(loads[0].Symbols, want) {
		t.Fatalf("load symbols = %#v, want %#v", loads[0].Symbols, want)
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

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
