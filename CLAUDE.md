# lidarr-metadata-provider

A drop-in, self-hostable replacement for Lidarr's cloud metadata server (`api.lidarr.audio`).
Built from MusicBrainz CC0 dumps, preprocessed into a compact dataset at build time, served by a small stateless Go binary.
End-user experience: run one Docker container, point Lidarr's `metadataSource` at it, done.

Read `docs/PLAN.md` before doing anything substantial.
It contains the full validated plan, phase gates, and risk table.
Current phase: **Phase 1 (dump → dataset)**.
Phase 0 is done: the DTOs are ported to `internal/skyhook` and the fixture set is complete; `go test ./...` round-trips every fixture through the structs.

## Non-negotiable rules

1. **License and provenance.**
   This project is GPL-3.0.
   `Lidarr/Lidarr` is GPL-3.0: its code MAY be ported/reused (especially the SkyHook resource DTOs).
   `blampe/rreading-glasses` is GPL-3.0: patterns MAY be reused.
   `Lidarr/LidarrAPI.Metadata` has NO license: it is read-only behavioural reference.
   Never copy its code or SQL, not even "adapted".
   All pipeline SQL gets written fresh against the documented MusicBrainz schema.

2. **The fixtures are the contract.**
   `fixtures/v0.4/` holds golden responses captured from the live `api.lidarr.audio/api/v0.4`.
   Our responses must be semantically equal: same keys, same casing, same types.
   Field order may differ; nothing else may.
   Do not "fix" the upstream API's inconsistent casing: `artistname`, `sortname` etc. are lowercase while `Albums`, `Releases`, `OldIds`, `SecondaryTypes` are capitalised.
   That inconsistency is load-bearing; Lidarr's deserializer defines truth.

3. **Build time vs run time.**
   Anything expensive (dump parsing, enrichment, image/overview fetching, index building) belongs in the build pipeline that WE run.
   The runtime server is stateless, read-only, needs no API keys, and must survive total neglect.
   The default configuration touches no third-party API at request time, and that default is not negotiable.

   **Amended 2026-07-22 (Nick's call):** one bounded exception exists, the live MusicBrainz fallback.
   It is off unless the operator passes `-fallback` with a `-contact`, it is only consulted for lookups the dataset misses, and when it fails the server degrades to dataset-only rather than erroring.
   Dump-only remains the supported default so nobody is forced into a runtime dependency.
   Do not widen this: no third-party API may become required, and none may sit on the hot path for data the dataset already has.

4. **Do not become the single point of failure.**
   No hosted public instance, no phone-home, no mandatory central anything.
   The container fetches a versioned dataset artifact from GitHub Releases on first boot; after that it works offline forever.

## The verified contract (from Lidarr's `SkyHookProxy.cs`, develop, 2026-07-22)

Lidarr appends `/{route}` to the configured `MetadataSource` base URL, so all routes are served at root.
The `v0.4` path only exists in Lidarr's default cloud URL; we never need it.

Routes Lidarr actually calls - this is the complete surface:

| Route | Behaviour |
| --- | --- |
| `GET /artist/{mbid}` | Artist with `Albums[]`. No query params - Lidarr sends none (verified in `SkyHookProxy.GetArtistInfo`) and filters albums client-side. See "Client-side album filtering" below. |
| `GET /album/{mbid}` | Release group with `Releases[]` and tracks. |
| `GET /search?type=artist&query=` | List of artist objects. Query arrives lowercased/trimmed. |
| `GET /search?type=album&query=&artist=&includeTracks=1` | List of album objects. |
| `GET /search?type=all&query=` | Mixed entity list (UI search bar). |
| `GET /recent/artist?since=<unix>` | Return `{"since": "<echo as ISO 8601>", "count": 0, "limited": true, "items": []}` always; valid for a static dataset, Lidarr then does its normal full refresh. Keys are lowercase on the wire (verified against live service). Client suppresses HTTP errors here. |
| `GET /recent/album?since=<unix>` | Same. |
| `POST /search/fingerprint` | Stub: `200` with `[]`. |
| `GET /` | Info object with version and `replication_date`. |

Routes that exist on the official server but are NEVER called by Lidarr (do not build): `/search/artist`, `/search/album`, `/chart/*`, `/series/*`, `/spotify/*`, `/*/refresh`, `/invalidate`.

The real `search?type=` routes are captured in `fixtures/v0.4/search-type-*.json`; `search-artist_radiohead.json` is a legacy capture of the never-called `/search/artist` route, kept as shape reference (see `fixtures/v0.4/README.md`).

## Client-side album filtering (drives dataset requirements)

There are no `primTypes`/`secTypes`/`releaseStatuses` query params.
An earlier version of this file claimed there were; that was wrong, disproved twice on 2026-07-22: `SkyHookProxy.GetArtistInfo` builds the request with only the route segment, and the live service returns byte-identical payloads whether or not those params are supplied.
Lidarr applies the user's metadata profile itself, in `SkyHookProxy.FilterAlbums`, keeping a skeletal album only when all three hold:

```csharp
primaryTypes.Contains(album.Type)
&& ((!album.SecondaryTypes.Any() && secondaryTypes.Contains("Studio")) || album.SecondaryTypes.Any(x => secondaryTypes.Contains(x)))
&& album.ReleaseStatuses.Any(x => releaseStatuses.Contains(x))
```

The stock "Standard" profile allows primary `Album`, secondary `Studio`, status `Official` only (`MetadataProfileService.AddDefaultProfile`).

Consequences the pipeline must respect:

- **An album with empty `ReleaseStatuses` is invisible to every profile** (`.Any()` over an empty set is false). Release status lives on `release`, not `release_group`, so the skeletal album entry in the artist payload requires a join up from releases. Get this wrong and artists render with zero albums while the JSON still looks perfect.
- Empty `SecondaryTypes` means "Studio", so it must stay empty for studio albums rather than being filled with a placeholder.
- `Type` must exactly match a Lidarr primary type name; an unrecognised or empty string filters the album out.
- Upstream itself ships albums with empty `ReleaseStatuses` (26 of the Beatles' 1019). Reproduce that faithfully, do not "fix" it.
- Useful acceptance metric, more meaningful than raw payload equality because it is what the user actually sees: under the Standard profile the Beatles keep 18 of 1019 albums, Prince 36 of 637, Bach 4487 of 5668, Utada Hikaru 11 of 105, The La's 4 of 20.

## How users switch Lidarr over

`MetadataSource` is first-class Lidarr config with no UI field.
The switch is one call against Lidarr's own REST API (GET the current object first, PUT it back with `metadataSource` changed; revert by setting `""`):

```
PUT http://lidarr:8686/api/v1/config/metadataprovider
X-Api-Key: <key>
{"id": 1, "metadataSource": "http://host:5001/", ...rest of fetched object}
```

`switch.sh` wraps this and is part of the product, not tooling.

## Live fallback and MusicBrainz access

The fallback is opt-in (`-fallback -contact <email or url>`) and exists to cover the days between a release appearing in MusicBrainz and appearing in a dataset artifact.

Two hard-won rules when touching anything that talks to MusicBrainz:

1. **Every request goes through the one shared `ratelimit.Limiter`.**
   The documented cap is 1 request/second per source IP, and exceeding it drops 100% of requests from that IP, not just the excess.
   The limiter reserves slots rather than checking a rate, so concurrent lookups queue instead of bursting.
   Answering one album takes several calls (release groups and releases both paginate at 100), which is exactly why a per-request limiter would not be enough.

2. **The User-Agent product token must not start with a lowercase `lidarr`.**
   MusicBrainz answers 403 "the application you are using has not identified itself" to any such user agent, case-sensitively and prefix-anchored.
   Verified 2026-07-22: `lidarr/0.1 ( me@example.com )` and `lidarr-anything/0.1 ( ... )` are refused; `Lidarr/0.1`, `mylidarrapp/0.1` and `LidarrMetadataProvider/0.1` are accepted.
   Use `musicbrainz.UserAgent`, which is pinned to `LidarrMetadataProvider/<version> ( <contact> )` and guarded by a test.
   Renaming that token to match the repository name would silently break fallback for every user.

Dataset freshness is also configurable rather than fixed, because dataset downloads are the project's real bandwidth cost; see `docs/PLAN.md` section 9.

## Stack

- Go, single static binary, one Docker container.
- SQLite opened read-only at runtime; FTS5 for search.
- Hot paths (`/artist/`, `/album/`) serve precomputed JSON payloads keyed by MBID; runtime does lookup + filter, never assembly.
- Performance benchmarks: The Beatles, 228 KB, 1019 albums (`fixtures/v0.4/artist_*_beatles.json`); J.S. Bach, 1.2 MB, 5668 albums (`fixtures/v0.4/artist_*_bach.json`). Large artists are a known Lidarr pain point and our headline win.

## Layout

- `docs/PLAN.md` - the validated plan; keep it current when decisions change.
- `fixtures/v0.4/` - golden responses; never regenerate from our own server, only from the live upstream. `README.md` there documents provenance.
- `internal/skyhook/` - the ported SkyHook contract structs, `ContractDiff` (the semantic differ), and fixture round-trip tests (Phase 0).
- `cmd/probe` - dev CLI: query any metadata server (`-base`, live upstream by default), pretty-print or `-save` exact-byte fixtures, and report contract drift against the structs on every response. `go run ./cmd/probe` for usage.
- `cmd/`, rest of `internal/` - Go server (Phase 2+).
- `pipeline/` - dump-to-dataset build (Phase 1); runs on our machines only.

## Testing

Contract tests diff our responses against fixtures: ignore key order, fail on missing keys, casing drift, or type drift.
Search quality gate: top-1 parity with the live service on a fixed query list.
Before any release: one week against a real Lidarr instance (add artist, monitor album, refresh, manual import) with zero corruption.
