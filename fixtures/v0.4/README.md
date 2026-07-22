# Golden fixtures - api.lidarr.audio/api/v0.4

These files are raw, byte-for-byte responses captured from the live `https://api.lidarr.audio/api/v0.4` service.
They are the contract: our server's responses must be semantically equal (same keys, same casing, same types; only key order may differ).
Never regenerate them from our own server; re-capture only from the live upstream.
The tests in `internal/skyhook` round-trip every file here through the ported Go structs.

## Nasty-sample coverage

| Category | Fixture |
| --- | --- |
| Huge artist (1019 albums, 228 KB) | `artist_b10bbbfc..._beatles.json` |
| Huger artist (5668 albums, 1.2 MB) | `artist_24f1766e..._bach.json` |
| Single-album artist | `artist_ff3e88b3..._the-las.json` |
| Alias-heavy artist | `artist_070d193a..._prince.json` (10 aliases), Bach (22) |
| Non-Latin script | `artist_b539e453..._utada-hikaru.json` (宇多田ヒカル) |
| Classical (composer + performer credits) | `album_31048ac9..._goldberg-state-of-wonder.json` |
| Various-artists compilation | `album_1703cd63..._pulp-fiction-ost.json` |
| Brand-new 2026 release | `album_4c4eee61..._olivia-rodrigo-2026.json` (released 2026-06-12) |

## Capturing more fixtures

Use the probe CLI; it fetches with Lidarr's exact query semantics, saves the exact response bytes, and contract-checks the response in one step:

```
go run ./cmd/probe -save fixtures/v0.4/artist_<mbid>_<slug>.json artist <mbid>
go run ./cmd/probe -save fixtures/v0.4/search-type-album_<slug>.json search-album "<query>" "<artist>"
```

Then add a provenance row below.

## Provenance

All captures on 2026-07-22 unless noted.
Search queries are sent the way Lidarr sends them: lowercased and trimmed, `artist=` present (possibly empty) for `type=album`, `includeTracks=1`.

| File | Route |
| --- | --- |
| `root.json` | `GET /` |
| `artist_<mbid>_<slug>.json` | `GET /artist/{mbid}` (no query filters) |
| `album_<mbid>_<slug>.json` | `GET /album/{mbid}` |
| `search-type-artist_<slug>.json` | `GET /search?type=artist&query=<slug>` |
| `search-type-album_pulp-fiction.json` | `GET /search?type=album&query=pulp fiction&artist=&includeTracks=1` |
| `search-type-album_goldberg-variations.json` | `GET /search?type=album&query=goldberg variations&artist=glenn gould&includeTracks=1` |
| `search-type-album_olivia-rodrigo-2026.json` | `GET /search?type=album&query=you seem pretty sad for a girl so in love&artist=olivia rodrigo&includeTracks=1` |
| `search-type-all_beatles.json` | `GET /search?type=all&query=beatles` |
| `recent-artist_since-1752000000.json` | `GET /recent/artist?since=1752000000` |
| `recent-album_since-1752000000.json` | `GET /recent/album?since=1752000000` |
| `search-artist_radiohead.json` | **Legacy**: `GET /search/artist?query=radiohead` (pre-plan capture, ~2026-07). Route is never called by Lidarr; kept because the object shape matches `search?type=artist` and it is upstream truth. |
| `artist_b10bbbfc..._beatles.json`, `album_ac7bcd07....json` | Pre-plan captures (~2026-07) of the normal artist/album routes. |

## Behavioural notes observed during capture

- `search?type=album` returned a transient HTTP 500 (`{"error":"Internal server error"}`) once; the identical request succeeded on retry. Lidarr treats search failures as empty results, but our server should still never do this.
- `recent/*` on the live service returns lowercase keys (`since`, `count`, `limited`, `items`) with `limited: true` and `count: 10000` - it caps enumeration and Lidarr falls back to full refresh. The DTO's capitalized `Limited`/`Items` casing does NOT appear on the wire.
- `search?type=all` items are `{"score": n, "artist": {...}|null, "album": {...}|null}` - lowercase, unlike the C# `EntityResource` property names.
- Sizes: artist and album payloads for big entities reach 1.2 MB (Bach, Pulp Fiction OST with `Releases[]`).
