# Cutting a release

There are two things a user consumes: the container image and the dataset.
They are released independently, because the code changes rarely and the data changes twice a week.

## The container image

Pushing to `main` or a `v*` tag runs `.github/workflows/image.yml`, which builds and pushes `ghcr.io/<repo>:latest` (and `:sha`, and `:<tag>`) for amd64 and arm64.
Nothing else is needed; the image the compose file and docs point at is always the current code.

## The dataset

The dataset is built from a MusicBrainz full export, enriched, and published as a GitHub release.
It is larger than a single release asset, so it is split into parts with a manifest and a whole-file checksum; the server rejoins and verifies them on first boot.

### Automated

Run the `dataset` workflow (`.github/workflows/dataset.yml`) from the Actions tab, or wait for its schedule once enabled.
It downloads the latest export, enriches from Wikidata and Wikipedia, builds, packages, and publishes a release tagged `dataset-<stamp>`.
The biography cache persists between runs, so most builds only fetch the articles that changed.

### By hand

On a machine with the export downloaded to `dumps/` and enough disk for the dataset plus its parts:

```
# 1. Enrich (first run is slow; the cache makes later runs quick).
go run ./cmd/enrich -out enrich/artists.jsonl -contact you@example.com

# 2. Build the dataset.
go run ./cmd/pipeline build \
  dumps/mbdump.tar.bz2 dumps/mbdump-derived.tar.bz2 dataset.db \
  dumps/mbdump-cover-art-archive.tar.bz2 enrich/artists.jsonl

# 3. Split into release-sized parts with a manifest and checksum.
./scripts/package-dataset.sh dataset.db release

# 4. Publish. The tag is what "latest" resolves to for the container.
TAG="dataset-$(date +%Y%m%d)"
gh release create "$TAG" --title "Dataset $TAG" --notes "See docs/DATA_SOURCES.md."
gh release upload "$TAG" release/dataset.db.part* release/dataset.db.parts release/dataset.db.sha256
```

The server is pointed at `.../releases/latest/download/dataset.db` (see `compose.yaml`).
That address has no `dataset.db` asset; the server fetches `dataset.db.parts`, downloads the listed parts, rejoins them, and verifies the result against `dataset.db.sha256` before serving.

## Provenance and licensing

An enriched dataset carries artist biographies from Wikipedia, which are CC BY-SA.
See [DATA_SOURCES.md](DATA_SOURCES.md); a published dataset must keep that attribution.
A dump-only build (omit steps 1 and the enrichment argument) is pure CC0 MusicBrainz data.
