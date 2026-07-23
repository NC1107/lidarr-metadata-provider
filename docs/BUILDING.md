# Building your own dataset

You do not need this.
The container downloads a prebuilt dataset on first start and that is the supported path for basically everyone.

This is here for the case where you would rather not depend on my github releases at all.
It is the same pipeline I run, not a cut down version.

## Status

Working end to end. You can build a dataset and serve from it.

Not in the dataset yet: images and overviews. MusicBrainz carries neither, so they come from separate enrichment that has not been built. Artists and albums will have empty `images` and a null `overview`, which Lidarr tolerates.

## What you need

- Go 1.24 or newer.
- About 10gb of free disk. The dumps are 7.4gb compressed and the pipeline streams them, so the roughly 40gb uncompressed form never lands on your disk.
- `lbzip2` or `pbzip2`, optional but worth it. bzip2 stores independent blocks so decompression parallelises across cores, and it is the slowest part by a wide margin. Without it a full pass is around ten minutes, with it closer to two.

## Getting the dumps

Pick the latest export from https://data.metabrainz.org/pub/musicbrainz/data/fullexport/ and grab three files.
Only the two most recent exports stay online, so do not plan on fetching an old one later.

```
STAMP=20260718-002132   # whatever LATEST says
BASE=https://data.metabrainz.org/pub/musicbrainz/data/fullexport/$STAMP

curl -O $BASE/mbdump.tar.bz2           # 6.9gb, the entities
curl -O $BASE/mbdump-derived.tar.bz2   # 0.5gb, ratings and release dates
curl -O $BASE/SHA256SUMS
```

Both archives are required.
The derived one holds `release_group_meta`, which is where an album's release date comes from, so a build without it gives you albums with no dates.

Please be considerate here.
This is a donation funded nonprofit serving a 7gb file, so pull it once and keep it rather than re-downloading while you experiment.

## Verify before you build

```
go run ./cmd/pipeline verify SHA256SUMS mbdump.tar.bz2 mbdump-derived.tar.bz2
```

A truncated download does not announce itself.
It produces a dataset that is quietly missing rows, which is worse than a build that fails outright, so check first.

## Look at what you have

```
go run ./cmd/pipeline inspect mbdump.tar.bz2
```

Prints when the export was taken, its schema version, and its replication sequence.
The schema version matters: the pipeline refuses an export whose schema it has not been checked against, because a column moving upstream would otherwise produce data that is wrong while still looking like valid json.

To see a table's actual columns before writing anything against them:

```
go run ./cmd/pipeline inspect mbdump.tar.bz2 release_group
```

## Enrich with images and biographies (optional)

MusicBrainz does not carry artist photos or biographies.
This step gathers them from Wikidata and Wikipedia, both open data, and writes a file the build folds in.
It is optional: without it, artists simply have no image or overview, exactly as a dump-only build always did.

```
go run ./cmd/enrich -out enrich/artists.jsonl -contact you@example.com
```

The harvest of image and article links is a handful of quick queries.
The biographies are one fetch per article, so the first run takes a while, but the file is a cache: run it again before your next dump and it keeps every biography whose Wikipedia article has not changed, fetching only what is new.
Pass `-images-only` to skip the biography fetch and get just the photos.

## Build the dataset

```
go run ./cmd/pipeline build mbdump.tar.bz2 mbdump-derived.tar.bz2 dataset.db \
  mbdump-cover-art-archive.tar.bz2 enrich/artists.jsonl
```

The last two arguments are optional.
The cover art archive gives albums their artwork; the enrichment file gives artists their photo and biography.
Pass `""` for the cover art slot if you want enrichment but not artwork.

This reads each archive once and writes every artist and album. Expect it to take a while and to want several gigabytes of scratch space next to the output, which is removed when it finishes.

Then serve it:

```
go run ./cmd/lidarr-metadata-provider -dataset dataset.db -web
```

Open http://localhost:5001/ui to check what you built, then point Lidarr at it:

```
./switch.sh --lidarr http://localhost:8686 --api-key <key> --to http://localhost:5001/
```

## Build a single artist, for checking

Faster than a full build when you want to see whether the mapping is right.

```
go run ./cmd/pipeline build-artist mbdump.tar.bz2 mbdump-derived.tar.bz2 \
  b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d
```

Give it as many MusicBrainz ids as you like.
It reads each archive exactly once no matter how many you ask for, so ask for everything you want in one go rather than running it repeatedly.

It prints the same json the server would serve, plus a line telling you how many albums survive Lidarr's default metadata profile.
That second number is the useful one, it is what a real Lidarr install would actually display.

## What it costs

Measured on 8 cores against the 20260718 export:

| | |
| --- | --- |
| Archives to download | 7.5 GB |
| Scratch space during the build | roughly 8 GB beside the output, removed afterwards |
| Peak memory | around 13 GB (cap it with `LMP_MEMORY_LIMIT_GB`) |
| Output | a single file of roughly 9 GB |
| Wall clock | around 35 minutes on 8 fast cores |

Give the build a machine with 16 GB or more; the peak working set is about 13 GB and the default memory cap is 13 GB, which you can lower with `LMP_MEMORY_LIMIT_GB` on a smaller box at the cost of harder garbage collection.
The scratch space holds tracks and recordings, roughly 57 million and 40 million rows, which are the only tables too large to keep in memory.

## If a build refuses to start

Two failures are deliberate and both mean stop rather than continue.

**Unsupported schema sequence.** MusicBrainz changed its database layout and the pipeline has not been checked against it.
Building anyway risks reading shifted columns and producing plausible looking nonsense.

**Archives are from different exports.** The two files came from different dumps, so their internal ids do not line up.
Continuing would attach one album's release date to a different album.
Re-download both from the same stamp.
