# gazelle_d

Gazelle language extension for [`rules_d`](https://registry.bazel.build/modules/rules_d).

The extension discovers D source files and generates:

- `d_library` targets for package sources.
- `d_binary` targets for source files containing a D `main`.
- `d_test` targets for source files containing `unittest` blocks.
- `d_proto_library` targets for generated `proto_library` targets.

It also indexes D module names and resolves same-workspace D imports into Bazel `deps`.

## Usage

Add this module beside `gazelle` and `rules_d`, then build a Gazelle binary that embeds the extension:

```starlark
load("@gazelle//:def.bzl", "gazelle_binary")

gazelle_binary(
    name = "gazelle_d",
    languages = [
        "@gazelle_d//gazelle/d",
    ],
)
```
