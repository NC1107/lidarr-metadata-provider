# Building your own dataset

You do not need this.
The container downloads a prebuilt dataset on first start and that is the supported path for basically everyone.

This is here for the case where you would rather not depend on my github releases at all.
It is the same pipeline I run, not a cut down version.

## Status

Partly working, and honest about which parts.

What runs today: reading a MusicBrainz export, verifying it, and building artist payloads out of it.
What does not exist yet: packaging those payloads into a dataset file the server can load, so there is currently nothing to point the server at.
That is the piece being built now, tracked as Phase 1 in [PLAN.md](PLAN.md).

So right now you can verify the pipeline works and inspect what it produces, but you cannot yet build a dataset and serve from it.

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

## Build artist payloads

```
go run ./cmd/pipeline build-artist mbdump.tar.bz2 mbdump-derived.tar.bz2 \
  b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d
```

Give it as many MusicBrainz ids as you like.
It reads each archive exactly once no matter how many you ask for, so ask for everything you want in one go rather than running it repeatedly.

It prints the same json the server would serve, plus a line telling you how many albums survive Lidarr's default metadata profile.
That second number is the useful one, it is what a real Lidarr install would actually display.

## If a build refuses to start

Two failures are deliberate and both mean stop rather than continue.

**Unsupported schema sequence.** MusicBrainz changed its database layout and the pipeline has not been checked against it.
Building anyway risks reading shifted columns and producing plausible looking nonsense.

**Archives are from different exports.** The two files came from different dumps, so their internal ids do not line up.
Continuing would attach one album's release date to a different album.
Re-download both from the same stamp.
