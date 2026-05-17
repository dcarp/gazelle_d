# gazelle_d

Gazelle language extension for [`rules_d`](https://registry.bazel.build/modules/rules_d).

The extension discovers D source files and generates:

- `d_library` targets for package sources.
- `d_binary` targets for source files containing a D `main`.
- `d_test` targets for source files containing `unittest` blocks.
- `d_proto_library` targets for generated `proto_library` targets.
- `dub_lock_dependencies` targets for DUB dependency lock files.

When a package contains `dub.json` or `dub.sdl`, Gazelle invokes rules_d's
DUB binary as `dub convert --format=json --stdout`. The generated `d_library`
and `d_binary` rules are derived from DUB's canonical recipe JSON, including
target names, target types, source files, source paths, import paths, versions,
libraries, linker flags, and registry dependencies. JSON and SDL recipes
therefore follow DUB's own interpretation instead of a separate Bazel-side
manifest parser.

Gazelle emits one D target for the root recipe and one for each
`subPackages[]` entry. Plain DUB dependency keys are treated as external
`@dub//...` labels, while qualified subpackage keys such as `root:extra` become
local labels such as `:extra`.

At the repository root, Gazelle emits a manual `dub_lock_dependencies` target
named `dub_dependencies`. Its `srcs` contain all discovered `dub.json`,
`dub.sdl`, and `dub.selections.json` inputs; when a directory has
`dub.selections.json`, that file is used instead of the sibling recipe file.
Run `bazel run //path/to/package:dub_dependencies.update` to create or refresh
`dub.selections.lock.json`.
Generated D targets reference registry dependencies through the default
`rules_d` DUB repository created from that lock file.

It also indexes D module names and resolves same-workspace D imports into Bazel `deps`.

## Usage

Add this module beside `gazelle` and `rules_d`, then build a Gazelle binary that embeds the extension:

```starlark
load("@gazelle//:def.bzl", "gazelle_binary")

gazelle_binary(
    name = "gazelle_d",
    languages = [
        "@gazelle_d//d",
    ],
)
```

If your top-level `dub.json` has registry dependencies, wire the generated
lock file into `MODULE.bazel`:

```starlark
dub = use_extension("@rules_d//dub:extensions.bzl", "dub")

dub.from_dub_selections(
    dub_selections_lock = "//:dub.selections.lock.json",
)

use_repo(dub, "dub")
```

Then generate or refresh the lock file:

```sh
bazel run //:dub_dependencies.update
```
