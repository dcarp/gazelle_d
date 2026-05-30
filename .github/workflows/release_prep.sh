#!/usr/bin/env bash
set -euo pipefail

tag_name="${1:?usage: release_prep.sh <tag>}"
version="${tag_name#v}"
prefix="gazelle_d-${version}"
archive="gazelle_d-${tag_name}.tar.gz"
docs_archive="${archive%.tar.gz}.docs.tar.gz"

git archive \
  --format=tar.gz \
  --prefix="${prefix}/" \
  --output="${archive}" \
  HEAD

docs_output_base="$(mktemp -d)"
docs_targets="$(mktemp)"
bazel --output_base="${docs_output_base}" query \
  --output=label \
  --output_file="${docs_targets}" \
  'kind("starlark_doc_extract rule", //...)'
if [[ -s "${docs_targets}" ]]; then
  bazel --output_base="${docs_output_base}" build \
    --target_pattern_file="${docs_targets}" \
    --remote_download_regex='.*doc_extract\.binaryproto'
  tar --create --auto-compress \
    --directory "$(bazel --output_base="${docs_output_base}" info bazel-bin)" \
    --file "${GITHUB_WORKSPACE:-$PWD}/${docs_archive}" .
else
  tar --create --auto-compress \
    --file "${GITHUB_WORKSPACE:-$PWD}/${docs_archive}" \
    README.md
fi

cat <<EOF
## gazelle_d ${version}

### Bzlmod

Add this module to \`MODULE.bazel\`:

\`\`\`starlark
bazel_dep(name = "gazelle_d", version = "${version}")
\`\`\`

### Release Archive

- ${archive}
- ${docs_archive}
EOF
