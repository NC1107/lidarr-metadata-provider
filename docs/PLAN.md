# lidarr-metadata-provider - validated project plan

Status: plan validated against Lidarr source (`Lidarr/Lidarr` develop branch) and the live `api.lidarr.audio` service on 2026-07-22.
Name: **lidarr-metadata-provider** (working name, matches Lidarr's own terminology - the config section is literally called `metadataprovider`).
Community precedent says a "lidarr-" prefixed descriptive name is fine (lidmeta, lidatube, docker-lidarr-extended all exist); a cuter brand can come later without breaking anything.

## 0. Validation results (what changed from the draft plan)

Verified by reading Lidarr's actual client code (`SkyHookProxy.cs`, `MetadataRequestBuilder.cs`, `ConfigService.cs`, `LidarrCloudRequestBuilder.cs`):

1. **The switch mechanism is better than assumed - no SQLite hack needed.**
   `MetadataSource` is a first-class Lidarr config value, exposed through Lidarr's own REST API.
   Switching is one authenticated call, live, no restart, instantly reversible:

   ```
   curl -X PUT "http://lidarr:8686/api/v1/config/metadataprovider" \
     -H "X-Api-Key: <key>" -H "Content-Type: application/json" \
     -d '{"id": 1, "metadataSource": "http://our-host:5001/", "writeAudioTags": "no", "scrubAudioTags": false, "embedCoverArt": false}'
   ```

   (Fetch current settings with GET first and PUT back the full object with `metadataSource` changed; revert by setting it to `""`.)
   The UI has no field for it (`MetadataProvider.js` renders only tag-writing settings), so our switch script IS the UX.

2. **We own the entire URL, version path included.**
   `MetadataRequestBuilder.cs`: when `MetadataSource` is set, Lidarr builds requests as `<MetadataSource>/{route}`.
   The default is `https://api.lidarr.audio/api/v0.4/{route}` - so `v0.4` lives only in the default.
   We serve routes at the root and the user enters `http://host:5001/`. Open question #1 from the draft: answered.

3. **The search contract in the draft was wrong.**
   Lidarr does NOT call `/search/artist` or `/search/album`.
   It calls the unified `search` route with query params (verbatim from `SkyHookProxy.cs`):
   - `GET search?type=artist&query=<lowercased trimmed>` → JSON list of artist objects
   - `GET search?type=album&query=<q>&artist=<lowercased or empty>&includeTracks=1` → JSON list of album objects
   - `GET search?type=all&query=<q>` → JSON list of mixed entity objects

4. **`recent/artist` and `recent/album` are NOT deferrable - but they have a trivial correct answer.**
   Lidarr calls `GET recent/artist?since=<unix>` and `GET recent/album?since=<unix>` from its scheduled refresh, with `SuppressHttpError = true` (failures tolerated).
   The response has a `Limited` flag; when set, Lidarr treats the enumeration as unavailable and falls back to its normal full-refresh behaviour.
   A static-dataset server can always return `{"Limited": true, "Items": []}` - correct, honest, and ~5 lines. Open question #2: answered.

5. **`search/fingerprint` gets a stub.** Return an empty 200 list; fingerprint search degrades gracefully.

6. **Licensing landmine found: `Lidarr/LidarrAPI.Metadata` has NO license (GitHub reports `license: null`).**
   No license = all rights reserved. We may read it to understand behaviour (interoperability), but we must NOT copy its code or SQL.
   `Lidarr/Lidarr` itself is **GPL-3.0** - its code IS reusable if we are GPL-3.0.
   `rreading-glasses` is **GPL-3.0** - patterns and code reusable on the same terms.
   **Decision: our project is GPL-3.0.** It unlocks both reuse sources and matches ecosystem norms.

## 1. What we are building (unchanged, sharpened)

A drop-in, self-hostable replacement for Lidarr's metadata server.
End-user experience, exactly as targeted:

```
docker run -d -p 5001:5001 -v lidarr-metadata:/data nc1107/lidarr-metadata-provider
./switch.sh --lidarr http://lidarr:8686 --api-key XXX --to http://host:5001/
```

Boom, it works.
On first boot the container downloads the current versioned dataset artifact from GitHub Releases into the volume (or the user pre-seeds it); then it serves, read-only, stateless.
All heavy work (dump processing, enrichment) happens in OUR build pipeline, not on the user's box and not at request time.
We host no service; neglect is a supported operating mode.

## 2. What we reuse, and from where

| Source | License | What we take |
| --- | --- | --- |
| `Lidarr/Lidarr` (GPL-3.0) | reusable | The SkyHook resource DTOs (`ArtistResource`, `AlbumResource`, `ReleaseResource`, `TrackResource`, `MediumResource`, `RecentUpdatesResource` in `src/NzbDrone.Core/MetadataSource/SkyHook/Resource/`). These define the exact JSON contract Lidarr deserializes - port them to Go structs field-for-field. This kills all response-shape guessing. |
| Live `api.lidarr.audio` | n/a (facts) | Golden fixtures (already captured: Beatles artist 228 KB/1019 albums, album, search). Ground truth for the differ. |
| `Lidarr/LidarrAPI.Metadata` | **none - read-only** | Behavioural reference only (endpoint semantics, filter behaviour). No code, no SQL copied. |
| MusicBrainz dumps | CC0 | All core data. |
| `metabrainz/musicbrainz-docker` | official | Optional: local mirror path for contributors; not required by users. |
| `blampe/rreading-glasses` (GPL-3.0) | reusable | Go project patterns: Docker packaging, README framing ("seconds to enable or disable"), large-author handling strategy. |
| TheAudioDB / fanart.tv / Wikidata | API ToS | Build-time enrichment only (images, overviews), keys held by us, never by users. |

## 3. Confirmed stack

- **Go.** Single static binary, tiny image, proven by rreading-glasses on this exact problem class, low contribution barrier. Confirmed.
- **SQLite, read-only at runtime**, plus precomputed JSON payloads for `artist/{mbid}` and `album/{mbid}` hot paths; SQLite FTS5 for search. Confirmed.
- **Single Docker container.** Serving container only; the dataset is a versioned artifact it fetches on first boot. The build pipeline is a separate repo/workflow that we run, not something users ever touch. Confirmed - this satisfies the one-container goal without baking 20+ GB into an image.
- **GPL-3.0.** New decision, see validation #6.

## 4. The verified contract (definitive MVP surface)

Routes Lidarr actually calls, from `SkyHookProxy.cs` - serve all of these at root:

| Route | Response | Notes |
| --- | --- | --- |
| `GET /artist/{mbid}` | artist object with `Albums[]` | Casing quirk is real: lowercase keys except `Albums`/`Releases`. Match fixtures exactly. |
| `GET /album/{mbid}` | album object with `Releases[]` (+ tracks) | The heavy one for imports. |
| `GET /search?type=artist&query=` | list of artist objects | |
| `GET /search?type=album&query=&artist=&includeTracks=1` | list of album objects | `includeTracks` matters for manual import. |
| `GET /search?type=all&query=` | mixed list | Powers the UI top search bar. |
| `GET /recent/artist?since=` | `{"since": "<echo of since as ISO 8601>", "count": 0, "limited": true, "items": []}` | Static-dataset answer, always valid. Live service emits lowercase keys (verified 2026-07-22); the DTO's `Limited`/`Items` casing never appears on the wire. |
| `GET /recent/album?since=` | same | same |
| `POST /search/fingerprint` | `[]` | Stub. |
| `GET /` | version + replication_date info | Health/vintage. |

Explicitly out (exist server-side, never called by the client): `/chart/*`, `/series/*`, `/spotify/*`, `/artist/{id}/refresh`, `/album/{id}/refresh`, `/search/artist`, `/search/album`, `/invalidate`.
That is a much smaller MVP than the draft assumed.

## 5. Phases (updated)

**Phase 0 - fixtures (DONE 2026-07-22).**
Extended the goldens to the full nasty sample: huge artist (Beatles 1019 albums, Bach 5668 albums/1.2 MB - the new large-artist stress case), single-album artist (The La's), alias-heavy (Prince), various-artists compilation (Pulp Fiction OST), classical (Goldberg Variations, composer + performer credits), non-Latin script (宇多田ヒカル), brand-new 2026 release (Olivia Rodrigo, 2026-06-12).
Captured the real `search?type=artist|album|all` routes, `recent/*`, and `/` - see `fixtures/v0.4/README.md` for provenance.
Ported the SkyHook resource DTOs (all ten files) to Go structs in `internal/skyhook`; `go test` round-trips every fixture through them with unknown-key rejection and key/casing/type tree-diff, so the contract is pinned from both sides.
Contract facts established by the port (see `internal/skyhook/resources.go` doc comment):
- Upstream never emits DTO fields `AristUrl` (sic), `ImageResource.Height`/`Width`, `TrackResource.Explicit`; we must not emit them either.
- Upstream emits `sortname`, `aliases`, `remoteUrl`, which the DTOs do not declare; the contract keeps them.
- `recent/*` and `search?type=all` wire keys are lowercase despite capitalized DTO properties.
- Same entity, different shape per context: album-under-artist is skeletal with capitalized keys; artist-under-album has no `Albums` key; empty collections are always `[]`, never null or absent.
- Nullable on the wire: artist `overview`/`type`, album `overview`, all `ReleaseDate`s, `rating.Value`, `DurationMs`. Everything else observed non-null across 16k+ objects.
- Open question #3 (empty-vs-absent) and #5 (which fields Lidarr reads) are now answered mechanically.
Dev tooling: `cmd/probe` queries any metadata server base (live upstream by default, ours later via `-base`), prints responses, saves exact-byte fixtures, and reports contract drift via `skyhook.ContractDiff` - the same differ the fixture tests use, which Phase 2's gate will reuse against our server.

**Phase 1 - dump → dataset.**
MusicBrainz dump ingest → precomputed artist/album JSON payloads + FTS index → single versioned artifact (target: well under 30 GB; drop every field the DTOs prove Lidarr never reads - open question #5 is now answerable mechanically from the DTO port).

Inputs settled during the Phase 0 wrap-up (2026-07-22):

- Source: `https://data.metabrainz.org/pub/musicbrainz/data/fullexport/<stamp>/` - `mbdump.tar.bz2` (6.9 GB) plus `mbdump-derived.tar.bz2` (482 MB) for tags/ratings, and `mbdump-cover-art-archive.tar.bz2` (156 MB) if we take CAA images. Latest stamp at time of writing: `20260718-002132`.
- Own dump reader (open question #4 above).
- **Hard requirement from Lidarr's client-side filter: populate `ReleaseStatuses` on every skeletal album** (needs a release → release_group join). Empty means the album is invisible under every metadata profile. Full rule in CLAUDE.md, "Client-side album filtering".
- Gate for this phase should include the profile-survivor counts in that section (Beatles 18/1019 etc.), not just payload equality - it is the number the user actually sees.
- Prerequisite not yet resolved: enrichment credentials (TheAudioDB / fanart.tv) for images and overviews. Core MB data does not need them; images and overviews do. Decide whether v1 ships thin (README already allows this) or we obtain keys first.
- **Never build a pipeline step on the MusicBrainz web service.** Its rate limit is 1 request/second per source IP, enforced by dropping 100% of requests once you exceed it, and no User-Agent changes that: the UA, IP and global checks are sequential and independent. At 1 req/s a million lookups take ~12 days, so any API-driven bulk step is dead on arrival. Bulk data comes from the dumps (plain HTTPS file downloads from data.metabrainz.org, not rate limited); if we ever need queryable MB at volume, run a local mirror with `metabrainz/musicbrainz-docker` and the Live Data Feed. The web service is for ad-hoc lookups only, at <= 1 req/s with a contactable UA.

**Phase 2 - serve artist/album.**
Gate: byte-semantic equality with fixtures via a differ (ignores key order, catches missing keys/casing/type drift).

**Phase 3 - search.**
`search?type=artist|album|all` over FTS5.
Gate: top-1 parity with the live service on a fixed query list.
Still the real difficulty; Solr is the incumbent.

**Phase 4 - package.**
Single container + first-boot dataset fetch + `switch.sh` (the REST PUT above, with GET-merge and revert mode).
Gate: one week against a real Lidarr instance - add artist, monitor album, refresh, import, nothing corrupts.

**Phase 5 - ship.**
One r/selfhosted post.
Lead with: no cloud dependency, survives api.lidarr.audio outages, large artists are fast, seconds to enable and to revert.

**Phase 6 - later.**
Enrichment depth, incremental dataset updates (make `recent/*` real using MB replication sequence numbers), hosted public instance only if ever wanted, track-first ambitions.

## 6. Risks (updated)

| Risk | Mitigation |
| --- | --- |
| New official metadata server ships | Our pitch is self-hostability + no single point of failure, not "theirs is down". Unchanged by their beta. |
| Contract drift | Fixtures + ported DTOs pin it. Lidarr's client code moves slowly (v0.4 has been pinned for years). |
| Search quality vs Solr | Own phase, explicit parity gate, FTS5 with trigram fallback if needed. |
| Large artists slow/huge | Precomputed payloads; Beatles fixture is the benchmark. |
| Thin images/overviews at launch | Acceptable; be explicit in README about coverage. |
| Copying unlicensed code by accident | Hard rule: LidarrAPI.Metadata is read-only reference. Our pipeline SQL is written fresh against documented MB schema. |
| Dataset hosting cost/size | GitHub Releases allows large artifacts; split per-table if needed; torrent as fallback. |

## 7. Remaining open questions

1. ~~v0.4 path negotiable?~~ Answered: we own the full base URL.
2. ~~recent/* semantics?~~ Answered: `Limited: true` is a valid permanent answer for a static dataset.
3. ~~Empty-vs-absent field behaviour?~~ Answered by the Phase 0 port: collections always `[]`, nullables enumerated in `internal/skyhook/resources.go`.
4. ~~Existing Go MusicBrainz dump parser worth reusing?~~ Answered 2026-07-22: `michiwend/gomusicbrainz` is a WS2 **API** client (MIT, self-described work-in-progress, last pushed 2023, 63 stars) - it does not touch dumps and is not a fit. We write our own dump reader; the dumps are tab-separated Postgres COPY format, straightforward in Go.
5. ~~Which fields does Lidarr read?~~ Answered: exactly the fields of the ported structs (see Phase 0 notes for the never-emitted DTO leftovers).

## 8. Next concrete step

Phase 1: dump → dataset.
Answer open question #4 (write our own MusicBrainz dump reader vs reuse) and build the pipeline that produces precomputed artist/album payloads matching `internal/skyhook` shapes, validated by the same fixture differ.
