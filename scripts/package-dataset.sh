#!/usr/bin/env bash
# Splits a built dataset into release-sized parts with a manifest and a
# whole-file checksum.
#
# A full dataset is larger than a single GitHub release asset may be, so it
# cannot be published as one file. This writes the parts, a manifest listing
# them in order, and the checksum of the joined whole. The server fetches the
# manifest, downloads the parts, rejoins them, and verifies the result against
# the checksum before serving it; see internal/dataset/fetch.go.
set -euo pipefail

DB="${1:?usage: package-dataset.sh <dataset.db> <outdir> [part-size]}"
OUT="${2:?usage: package-dataset.sh <dataset.db> <outdir> [part-size]}"
# Default part size stays under GitHub's per-asset limit with margin.
PART_SIZE="${3:-1900M}"

name="$(basename "$DB")"
mkdir -p "$OUT"

# Whole-file checksum first, in the "digest  name" form the server accepts.
sha256sum "$DB" | awk -v n="$name" '{print $1"  "n}' >"$OUT/$name.sha256"

# Ordered parts, each small enough to be a release asset.
split -b "$PART_SIZE" -a 2 -d "$DB" "$OUT/$name.part"

# Manifest of the parts, in order. The [0-9] glob keeps the manifest itself out.
( cd "$OUT" && ls "$name".part[0-9]* | sort ) >"$OUT/$name.parts"

echo "packaged $name into $(wc -l <"$OUT/$name.parts") part(s):"
ls -lh "$OUT"
