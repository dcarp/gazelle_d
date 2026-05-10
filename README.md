# gazelle_d

Gazelle language extension for [`rules_d`](https://registry.bazel.build/modules/rules_d).

The extension discovers D source files and generates:

- `d_library` targets for package sources.
- `d_binary` targets for source files containing a D `main`.
- `d_test` targets for source files containing `unittest` blocks.
- `d_proto_library` targets for generated `proto_library` targets.
- `dub_lock_dependencies` targets for DUB dependency lock files.

When a package contains `dub.json`, Gazelle uses the DUB recipe to choose
target names, source roots, import paths, versions, and platform libraries.
This supports DUB's default `source/` and `src/` layouts, including packages
that define a static library at the repository root and executables under
nested example or test packages.

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
