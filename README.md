# lidarr-metadata-provider

A self hosted replacement for lidarr's metadata server.

Lidarr doesn't keep artist and album metadata itself, it asks a cloud service at api.lidarr.audio for it.
So adding an artist, refreshing a library, or importing a folder all depend on someone else's server being up.
This is that server, except you run it.

## Status

Early, and nothing serves requests yet.

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

`cmd/probe` talks to api.lidarr.audio by default and takes `-base` to point at any other server, which is how our own responses get compared once there are any.
It also saves exact response bytes with `-save`, which is how the fixtures in `fixtures/v0.4` were captured.

## License

GPL-3.0.

The resource structs in `internal/skyhook` are ported from [Lidarr](https://github.com/Lidarr/Lidarr), which is GPL-3.0, so this project matches it.
Metadata comes from [musicbrainz](https://musicbrainz.org) under CC0.
`Lidarr/LidarrAPI.Metadata` has no license attached, so it is treated as read only reference for behaviour and none of its code or sql is reused here.
