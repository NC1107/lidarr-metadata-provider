# lidarr-metadata-provider

A self-hosted replacement for lidarr's cloud metadata server.
I've run this at home for a while, and after a friend saw my setup and asked for it (his metadata kept giving him trouble) I cleaned it up and put it here in case anyone else finds it useful.
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

There's a crude side-by-side ui if you want to see how the data stacks up:

```
go run ./cmd/lidarr-metadata-provider -fallback -contact you@example.com -web
```

Open http://localhost:5001/ui and type a query, it runs against this and the live cloud service at once. Each result also shows how many albums lidarr would actually keep after your metadata profile filters them, which is usually a lot fewer than the raw count.

## Flags

The compose file sets sensible defaults, so most people never touch these. If you run the binary directly or tweak the `command` block:

- `-addr` (default `:5001`) - the address it listens on. Change the port if 5001 is taken.
- `-dataset` - path to the dataset file it serves. In the container that's `/data/dataset.db`.
- `-dataset-url` (or the `LMP_DATASET_URL` env var) - where to grab the dataset if the file isn't there yet. Compose points it at the latest github release, so a fresh setup just works.
- `-dataset-refresh` (or `LMP_DATASET_REFRESH`, e.g. `72h`) - off by default. When set, it checks `dataset-url` that often and swaps in a newer dataset when one is published, live, no restart. Opt-in because that's the ~8gb download landing on your schedule. Leave it off and you keep your first snapshot forever, which is fine if you mostly listen to older music.
- `-web` - turns on the `/ui` side-by-side console. Handy for poking around, not needed for lidarr.
- `-fallback` - when the dataset misses something, look it up live from musicbrainz. Off by default, and it's the only thing that touches the network while serving. Needs `-contact`.
- `-contact you@example.com` - an email or url so musicbrainz can reach you if your instance misbehaves. Required when `-fallback` is on. No api key, that's the whole ask.
- `-fallback-interval` (default ~1s) - minimum gap between musicbrainz requests. Don't drop below a second, that's their limit and going under gets you blocked.
- `-fallback-max-pages` - caps how far one live lookup will page, so a giant artist can't make a single request hang forever.

## License

GPL-3.0. The structs in `internal/skyhook` are ported from [lidarr](https://github.com/Lidarr/Lidarr) (GPL-3.0). Metadata comes from [musicbrainz](https://musicbrainz.org) under CC0. `Lidarr/LidarrAPI.Metadata` has no license, so it is read-only behavioural reference and none of its code is reused.
