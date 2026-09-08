# lidarr-metadata-provider

A self-hosted replacement for lidarr's cloud metadata server.
I've run this at home for a while, and after a friend saw my setup and asked for it so I cleaned it up and put it here in case anyone else finds it useful.
It ships the dataset as prebuilt github assets on purpose, so nobody has to process the musicbrainz dumps themselves and pile bandwidth onto musicbrainz, which is donation funded.

Lidarr doesn't store artist and album metadata itself, it asks api.lidarr.audio for it.
So adding an artist, refreshing a library, or importing a folder all depend on someone else's server being up.
This is that server, except you run it.
It answers every route lidarr calls off a dataset built from the musicbrainz CC0 dumps (about 2.9 million artists, 4.4 million albums, 57 million tracks, plus images, biographies, ratings and cover art), and the responses are checked against captures from the live service so imports behave the same. Big artists that make the cloud choke load fine.

## Quick start

You run one container and point lidarr at it. It downloads the dataset on first boot, checks it against its checksum, and serves it. No dump to download, no import step, no database.

```
git clone https://github.com/NC1107/lidarr-metadata-provider
cd lidarr-metadata-provider
docker compose up -d
```

Then point lidarr at it. `metadataSource` has no field in lidarr's ui, so `switch.sh` sets it through lidarr's rest api, live and with no restart:

```
./switch.sh --lidarr http://localhost:8686 --api-key <key> --to http://localhost:5001/
```

Your api key is in lidarr under Settings > General > Security. Run the same thing with `--revert` to go back to the cloud service. After first boot it works offline.

## What it needs

Measured on a running instance, not estimated:

| | |
|---|---|
| Disk | 9GB for the dataset, and room for a second copy (~18GB) while an update downloads, since the old one keeps serving until the new one is verified |
| RAM, idle | ~14MB |
| RAM, normal searches | ~15MB |
| RAM, ten concurrent worst-case artist pages | ~22MB |

So 128MB is comfortable and 256MB has room to spare.
It's a single go binary reading sqlite, there's no database server, no cache layer and no background workers, and the memory stays flat because responses stream off disk rather than being held.
CPU is idle except while answering.

Building your own dataset is a different story: about 10GB of disk for the dumps, roughly 8GB of scratch beside the output, and a peak working set near 13GB, so give it a 16GB machine.
That gap is exactly why the prebuilt datasets exist.
[docs/BUILDING.md](docs/BUILDING.md) has the full numbers.

## How it works

The musicbrainz dumps are about 7gb and slow to process, so that happens ahead of time and what ships is the compact dataset it produces, you never touch the dumps. The server is a single go binary with sqlite opened read only, responses are precomputed json keyed by mbid, and search runs on FTS5.

New datasets are built on github actions twice a week, right after musicbrainz publishes. By default your container keeps the one it first downloaded, a stable snapshot that never changes under you. If you want it to stay current, set a refresh interval (`-dataset-refresh`, see Flags) and it checks for a newer dataset on that schedule, verifies it, and swaps it in without dropping a request, leaving you on the version that already worked if a download goes bad. There's no hosted instance and no phone home, on purpose, so if lidarr, musicbrainz and github all went down your container keeps serving what it has.

You can build the dataset yourself if you'd rather not use my images, the pipeline is the same code I run, see [docs/BUILDING.md](docs/BUILDING.md).

## The gap between dumps

Dumps come out twice a week, so there's a window where a new album is in musicbrainz but not your dataset yet. Two ways to cover it, and you can use both: keep the dataset current with `-dataset-refresh` (see Flags), or turn on the live fallback (uncomment the `command` block in `compose.yaml`, set a contact) so anything the dataset misses gets looked up from musicbrainz directly:

```
-fallback -contact you@example.com
```

No api key, the contact is just so musicbrainz can reach you if your instance misbehaves. Fallback lookups are slower (their limit is one request a second) and thinner (no images or bios), and if musicbrainz is down you get whatever the dataset has.

## Comparing it against the official service

There's a side-by-side console if you want to see how the data stacks up:

```
go run ./cmd/lidarr-metadata-provider -fallback -contact you@example.com -web
```

Open http://localhost:5001/ui and type a query, it runs against this server, the official cloud service and musicbrainz at once and lines the three up side by side.
Each column says whether its response is shaped the way lidarr expects, how long it took and how big it was, and each result shows how many albums lidarr would actually keep after your metadata profile filters them, which is usually a lot fewer than the raw count.
The musicbrainz column only fills in when the server runs with `-fallback -contact`, since that is what builds the musicbrainz client.
Dataset counts and live request history sit either side of it.

## Flags

The compose file sets sensible defaults, so most people never touch these. If you run the binary directly or tweak the `command` block:

Every flag can also be set from the environment as `LMP_` plus the flag name in upper case with dashes as underscores (`LMP_DATASET_URL`, `LMP_FALLBACK`, ...). A flag on the command line wins. A value that doesn't parse (`LMP_DATASET_REFRESH=3d`, say) refuses to start rather than silently running with the default.

- `-addr` (default `:5001`) - the address it listens on. Change the port if 5001 is taken.
- `-dataset` - path to the dataset file it serves. In the container that's `/data/dataset.db`.
- `-dataset-url` - where to grab the dataset if the file isn't there yet. Compose points it at the latest github release, so a fresh setup just works. Use https; a plain http url gets a warning, since the checksum only catches a truncated download, not a tampered one.
- `-dataset-refresh` (e.g. `72h`) - off by default. When set, it checks `dataset-url` that often and swaps in a newer dataset when one is published, live, no restart. Opt-in because that's the ~8gb download landing on your schedule. Leave it off and you keep your first snapshot forever, which is fine if you mostly listen to older music. A downloaded dataset is verified and test-opened before it replaces the one you have, so a bad download leaves you on the one that works. Note that it also replaces a dataset you built yourself if the published one differs, so leave it off for a self-built dataset.
- `-web` - turns on the `/ui` side-by-side console. Handy for poking around, not needed for lidarr. The console is unauthenticated and can make the server query the cloud service and musicbrainz on a visitor's behalf, so keep it off on a port other people can reach.
- `-fallback` - when the dataset misses something, look it up live from musicbrainz. Off by default, and it's the only thing that touches the network while serving. Needs `-contact`. Also what fills in the musicbrainz column in the console.
- `-contact` - an email or url so musicbrainz can reach you if your instance misbehaves, which they require of anyone querying them. Required when `-fallback` is on. No api key, that's the whole ask.
- `-fallback-interval` (default ~1s) - minimum gap between musicbrainz requests. Don't drop below a second, that's their limit and going under gets you blocked. Lookups are queued at most a minute deep; past that they get a 503 with a Retry-After, which lidarr treats as transient, rather than letting a burst push everyone's wait out indefinitely.
- `-fallback-max-pages` - caps how far one live lookup will page, so a giant artist can't make a single request hang forever.

`GET /healthz` runs a real lookup against the served dataset and answers 200 or 503, which is what the container's healthcheck and any orchestrator probe should use. `GET /` is lidarr's own info route and always answers while the process is up.

## License

GPL-3.0. The structs in `internal/skyhook` are ported from [lidarr](https://github.com/Lidarr/Lidarr) (GPL-3.0). Metadata comes from [musicbrainz](https://musicbrainz.org) under CC0. `Lidarr/LidarrAPI.Metadata` has no license, so it is read-only behavioural reference and none of its code is reused.
