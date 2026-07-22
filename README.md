# lidarr-metadata-provider

A self hosted replacement for lidarr's metadata server.

Lidarr doesn't keep artist and album metadata itself, it asks a cloud service at api.lidarr.audio for it.
So adding an artist, refreshing a library, or importing a folder all depend on someone else's server being up.
This is that server, except you run it.

## Status

Early. The server runs and answers every route lidarr calls, but there's no dataset behind it yet, so right now it only works in fallback mode against musicbrainz.

The contract is pinned, meaning the response shapes lidarr expects are ported to go structs and checked against golden responses captured from the live service.
Next up is the pipeline that turns musicbrainz dumps into the dataset.
`docs/PLAN.md` has the actual phase list if you want the detail.

## How it's meant to work

You run one docker container, point lidarr at it, and that's pretty much the whole thing.

The data comes from the musicbrainz CC0 dumps.
Those get chewed into a compact dataset ahead of time, so the container you run never parses a dump and never calls a third party api at request time.
On first boot it pulls a versioned dataset artifact from github releases into a volume.
After that it works offline, and it keeps working if this repo goes quiet for a year.

The server is a single go binary with sqlite opened read only.
Artist and album responses are precomputed json keyed by mbid, so a request is a lookup and a filter rather than assembly.
Search runs on sqlite FTS5.

There's no hosted public instance and no phone home, on purpose.
The point is not being a single point of failure, so putting one in the middle would sort of defeat it.

## New releases, and the gap

The dumps come out twice a week, so there's a window where an album exists in musicbrainz but isn't in your dataset yet.
Two ways to deal with that, and you pick.

Dump only is the default. Nothing leaves your machine, and you get new music whenever you take a dataset update. How often that happens is up to you, since the download is the part that actually costs bandwidth.

Or you turn on the fallback, and anything the dataset doesn't have gets looked up live from musicbrainz:

```
lidarr-metadata-provider -fallback -contact you@example.com
```

There's no api key involved, musicbrainz just wants a way to reach you if your instance misbehaves, which is what the contact is for.
Requests get queued a second apart because that's their rate limit, and going over it gets your ip blocked rather than just slowed.
So fallback lookups are noticeably slower than dataset ones, and if musicbrainz is down you just get whatever the dataset has.

Worth knowing: fallback results are thinner. No images or overviews, since musicbrainz doesn't have those, and search results don't come with albums attached because that would be a request per result.

## Switching lidarr over

Lidarr has a `metadataSource` setting with no field in the ui, but it is exposed through lidarr's own rest api.
You fetch the current config, put it back with `metadataSource` changed, and it takes effect live with no restart.
Setting it back to `""` reverts to the cloud service.

```
PUT http://lidarr:8686/api/v1/config/metadataprovider
X-Api-Key: <key>
{"id": 1, "metadataSource": "http://host:5001/", ...rest of the fetched object}
```

A `switch.sh` that wraps this is planned, since that call is the actual install step.

## Development

Requires go 1.24 or newer.

```
go test ./...                      # round trip every fixture through the contract structs
go run ./cmd/probe root            # ask the live service for its version and vintage
go run ./cmd/probe artist <mbid>   # any route, with a contract check on the response
```

`cmd/probe` talks to api.lidarr.audio by default and takes `-base` to point at any other server, including ours.
It also saves exact response bytes with `-save`, which is how the fixtures in `fixtures/v0.4` were captured.

There's a local console for trying searches without going through lidarr:

```
go run ./cmd/lidarr-metadata-provider -fallback -contact you@example.com -web
```

Then open http://localhost:5001/ui.
You type a query, it runs it against us and against the live cloud service side by side, and shows both.
Each result says how many albums lidarr would actually display after it applies your metadata profile, which is usually a much smaller number than the album count and is the thing that tends to surprise people.
It also checks both responses against the ported contract and shows the queue state, so you can watch the rate limiter pace things out.

## License

GPL-3.0.

The resource structs in `internal/skyhook` are ported from [Lidarr](https://github.com/Lidarr/Lidarr), which is GPL-3.0, so this project matches it.
Metadata comes from [musicbrainz](https://musicbrainz.org) under CC0.
`Lidarr/LidarrAPI.Metadata` has no license attached, so it is treated as read only reference for behaviour and none of its code or sql is reused here.
