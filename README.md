# lidarr-metadata-provider

A self-hosted replacement for lidarr's cloud metadata server.

Lidarr doesn't store artist and album metadata itself, it asks api.lidarr.audio for it.
So adding an artist, refreshing a library, or importing a folder all depend on someone else's server being up.
This is that server, except you run it.

It answers every route lidarr calls, off a dataset built from the musicbrainz CC0 dumps: about 2.9 million artists, 4.4 million albums and 57 million tracks, plus artist images and biographies, album ratings, and cover art.
The response shapes are ported from lidarr's own code and checked against golden captures from the live service, so on the things that make an import work, tracks, releases, album coverage, it measures on par with or ahead of the official service.

## Quick start

You run one container and point lidarr at it.
The container downloads the finished dataset on first boot, checks it against its checksum, and serves it.
No dump to download, no import step, no database to set up.

```
git clone https://github.com/NC1107/lidarr-metadata-provider
cd lidarr-metadata-provider
docker compose up -d
```

Then point lidarr at it.
`metadataSource` has no field in lidarr's ui, so `switch.sh` sets it through lidarr's own rest api, live and with no restart:

```
./switch.sh --lidarr http://localhost:8686 --api-key <key> --to http://localhost:5001/
```

Your api key is in lidarr under Settings > General > Security.
To go back to the cloud service, run the same thing with `--revert`.

That's the whole install.
After first boot it works offline, and it keeps working if this repo goes quiet for a year.

## How it works

The data comes from the musicbrainz dumps, but you never touch those.
They're about 7gb and slow to chew through, so that happens ahead of time and what ships is the compact dataset it produces.
The server is a single go binary with sqlite opened read only.
Artist and album responses are precomputed json keyed by mbid, so a request is a lookup and a filter rather than assembly, and search runs on sqlite FTS5.

Updates work like first boot: a new dataset gets pulled and verified before it replaces the old one, so a bad download leaves you on the version that was already working.
The builds run on github actions twice a week, right after musicbrainz publishes, so nobody makes them by hand.

There's no hosted public instance and no phone home, on purpose.
The point is not being a single point of failure, so putting one in the middle would defeat it.

## On me being a single point of failure

Yes, i see the irony.
The whole pitch is not depending on someone else's server, and then the dataset comes from my github releases.

The difference is what happens when it breaks.
If api.lidarr.audio goes down, your lidarr stops right then, mid import.
If i get bored and wander off, your container keeps serving the dataset it already has, offline, basically forever.
You just stop getting new music until you do something about it, which is a slower kind of broken.

And you can build the dataset yourself if you'd rather not use my images.
The pipeline ships in the same container, it's the same code i run, see [docs/BUILDING.md](docs/BUILDING.md).

## New releases, and the gap

The dumps come out twice a week, so there's a window where an album exists in musicbrainz but isn't in your dataset yet.
Two ways to handle it, and you pick.

Dump only is the default.
Nothing leaves your machine, and you get new music whenever you take a dataset update, which is as often as you like since the download is the part that costs bandwidth.

Or you turn on the fallback, and anything the dataset misses gets looked up live from musicbrainz.
Uncomment the `command` block in `compose.yaml` and set a contact:

```
-fallback -contact you@example.com
```

There's no api key, musicbrainz just wants a way to reach you if your instance misbehaves.
Requests go a second apart because that's their rate limit, so fallback lookups are slower than dataset ones, and if musicbrainz is down you get whatever the dataset has.
Fallback results are also thinner, no images or overviews, since musicbrainz doesn't carry those.

## Development

Requires go 1.24 or newer.

```
go test ./...                      # round trip every fixture through the contract structs
go run ./cmd/probe artist <mbid>   # ask any route, with a contract check on the response
```

`cmd/probe` talks to api.lidarr.audio by default and takes `-base` to point at any other server, including yours.
`-save` writes exact response bytes, which is how the fixtures in `fixtures/v0.4` were captured.

There's a local console for trying searches without going through lidarr:

```
go run ./cmd/lidarr-metadata-provider -fallback -contact you@example.com -web
```

Open http://localhost:5001/ui, type a query, and it runs against us and the live cloud service side by side.
Each result shows how many albums lidarr would actually display after your metadata profile filters them, which is usually a much smaller number than the album count and the thing that tends to surprise people.

## License

GPL-3.0.

The resource structs in `internal/skyhook` are ported from [lidarr](https://github.com/Lidarr/Lidarr), which is GPL-3.0, so this project matches it.
Metadata comes from [musicbrainz](https://musicbrainz.org) under CC0.
`Lidarr/LidarrAPI.Metadata` has no license attached, so it's treated as read-only reference for behaviour and none of its code or sql is reused here.
