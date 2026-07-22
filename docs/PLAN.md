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

- Source: `https://data.metabrainz.org/pub/musicbrainz/data/fullexport/<stamp>/`. Latest stamp at time of writing: `20260718-002132`.
  **Both `mbdump.tar.bz2` (6.9 GB) and `mbdump-derived.tar.bz2` (482 MB) are required**, which an earlier version of this plan got wrong by listing the derived archive as optional enrichment.
  The core archive holds the entities; the derived archive holds the computed tables, and `release_group_meta` there is the only source of an album's first release date.
  A build from the core archive alone produces albums with no dates at all.
  `mbdump-cover-art-archive.tar.bz2` (156 MB) remains genuinely optional, for CAA images.
- Archives must come from the same export. The pipeline compares replication sequences and refuses a mismatch, because joining meta rows against renumbered entity IDs yields payloads carrying other albums' dates and ratings while looking perfectly valid.
- Verify downloads against the export's `SHA256SUMS` before building.
- Install `lbzip2` (or `pbzip2`) on any machine that runs builds. bzip2 stores independent blocks so decompression parallelises across cores, and the single-threaded decoder is the entire bottleneck on a 6.9 GB archive. The pipeline uses one automatically when present and falls back to the standard library when not.
- Own dump reader (open question #4 above).
- **Hard requirement from Lidarr's client-side filter: populate `ReleaseStatuses` on every skeletal album** (needs a release → release_group join). Empty means the album is invisible under every metadata profile. Full rule in CLAUDE.md, "Client-side album filtering".
- Gate for this phase should include the profile-survivor counts in that section (Beatles 18/1019 etc.), not just payload equality - it is the number the user actually sees.
- Prerequisite not yet resolved: enrichment credentials (TheAudioDB / fanart.tv) for images and overviews. Core MB data does not need them; images and overviews do. Decide whether v1 ships thin (README already allows this) or we obtain keys first.
- **Two of the four "missing" fields are not enrichment at all and should be built from the dumps.** The console's field diff against the live service (2026-07-22) shows our payloads carry `genres: []`, `links: []`, `images: []` and `overview: null` where upstream has 18 genres, 57 links, 4 images and a biography.
  `genre`, `tag`, `artist_tag`, `release_group_tag`, `url` and `l_artist_url` are all present in the export, so genres and links are ours to extract and simply have not been wired up.
  Only images and overviews genuinely require third-party enrichment, because MusicBrainz carries neither.
- **Never build a pipeline step on the MusicBrainz web service.** Its rate limit is 1 request/second per source IP, enforced by dropping 100% of requests once you exceed it, and no User-Agent changes that: the UA, IP and global checks are sequential and independent. At 1 req/s a million lookups take ~12 days, so any API-driven bulk step is dead on arrival. Bulk data comes from the dumps (plain HTTPS file downloads from data.metabrainz.org, not rate limited); if we ever need queryable MB at volume, run a local mirror with `metabrainz/musicbrainz-docker` and the Live Data Feed. The web service is for ad-hoc lookups only, at <= 1 req/s with a contactable UA.

**Phase 2 - serve artist/album.**
Gate: byte-semantic equality with fixtures via a differ (ignores key order, catches missing keys/casing/type drift).

Artist path done 2026-07-22, measured against the real 20260718 export:

- Dataset: 2,934,605 artists, 1.55 GB, built in 7m33s on 8 cores. Payloads are compressed JSON keyed by MBID.
- The Beatles (1033 albums, 229 KB) serves in **4.8 ms** against 117-187 ms from the cloud service, roughly 25-40x faster. J.S. Bach (1.35 MB) serves in 23 ms against 200 ms.
- Correctness against the golden fixture: all 1019 fixture albums present, identical 18 survivors under the stock profile, and the 26 albums with empty release statuses match the fixture exactly. Contract differ clean.
- Album payloads are not built yet. They need releases, media, tracks and recordings, roughly 35 M track rows, which will not fit the in-memory join the artist build uses; that needs staging through SQLite. Until then `/album/{mbid}` falls through to the live fallback.

**Phase 3 - search.**
`search?type=artist|album|all` over FTS5.
Gate: top-1 parity with the live service on a fixed query list (`cmd/parity`, `fixtures/search-queries.txt`).
Still the real difficulty; Solr is the incumbent.

Measured 2026-07-22 on the artist dataset: **57.8% top-1, 82.2% top-5** over 45 deliberately awkward queries.
The failures were one pattern rather than noise: bm25 relevance does not know that an exact name is what a user means, so "Yes Yes Yes" outranked "Yes" and "The THE BAND Band" outranked "The Band".
Staged ranking (exact, then all terms, then any term) with normalised names on both sides and album count as a notability tiebreak is in; awaiting re-measurement on the rebuilt dataset.

**Phase 4 - package (built 2026-07-22, gate not yet run).**
Single container + first-boot dataset fetch + `switch.sh` (the REST PUT above, with GET-merge and revert mode).
Gate: one week against a real Lidarr instance - add artist, monitor album, refresh, import, nothing corrupts.

Built: 44.7 MB image carrying both the server and the pipeline, dataset on a volume, checksum-verified first-boot download that leaves the previous dataset in place on a bad transfer, `compose.yaml`, and `switch.sh` which refuses to repoint Lidarr at a server that is not answering.
Automated builds land in `.github/workflows/dataset.yml`, on a cron after each MusicBrainz export, so no artifact is ever produced by hand.
The soak test against a real Lidarr has not happened and is the gate that actually matters before anyone else uses this.

**Phase 5 - ship.**
One r/selfhosted post.
Lead with: no cloud dependency, survives api.lidarr.audio outages, large artists are fast, seconds to enable and to revert.

**Phase 6 - later.**
Enrichment depth, hosted public instance only if ever wanted, track-first ambitions.
Incremental dataset updates move earlier in practice; see section 9.

## 9. Freshness: how new music reaches users

Verified against data.metabrainz.org on 2026-07-22.

**Full exports are twice weekly, Wednesday and Saturday.**
Only the two most recent are kept online (`20260715-002120`, `20260718-002132` at time of writing), so we cannot rely on fetching an old one later.
Each `mbdump.tar.bz2` carries its own provenance at the archive root: `TIMESTAMP` (`2026-07-18 00:21:33+00`), `REPLICATION_SEQUENCE` (`187552`), and `SCHEMA_SEQUENCE` (`31`).
`SCHEMA_SEQUENCE` is the number our dump reader must assert against, so a MusicBrainz schema change fails the build loudly instead of silently mangling columns.
`REPLICATION_SEQUENCE` is the join point for incremental updates.

**The public replication path is dead - do not use it.**
`data.metabrainz.org/pub/musicbrainz/data/replication/` looks like a live hourly feed and is not: every packet there is frozen at May 2015, the newest being `replication-86414.tar.bz2` dated 18-May-2015, against a current sequence of 187552.
The `daily/` and `weekly/` subdirectories are frozen at the same date.
The real Live Data Feed is token-gated at `https://metabrainz.org/api/musicbrainz/replication-<n>.tar.bz2?token=<TOKEN>` and answers HTTP 400 without one.
Tokens are free from a MetaBrainz account; getting one is a prerequisite for any incremental work, so do it early rather than discovering the gate mid-Phase-6.

**We can never be fresher than MusicBrainz itself.**
An album released two days ago is only reachable if a MusicBrainz editor has already added it, which for anything outside major releases is often not the case.
That is a hard floor no architecture choice moves, and it applies to the official cloud service too.

**The staleness budget, worst case, is the sum of three lags:** MusicBrainz editor lag, plus our rebuild cadence, plus the user's update interval.
Rebuilding on every full export puts our own contribution at 3-4 days; a monthly rebuild puts it at 30 and would make the project feel broken for anyone chasing new releases.
Weekly is the sane steady state, twice-weekly is available if the pipeline is cheap enough to run that often.

**Design consequence for Phase 1, not Phase 6:** the artifact format must be delta-friendly from the first version.
Users cannot re-download a multi-GB artifact twice a week, so updates have to ship as patches of changed rows keyed by MBID.
Retrofitting that into a format designed as a monolith is painful, so the schema decision has to be made now even if delta publishing lands later.

**Decided 2026-07-22: opt-in live fallback, dump-only stays the default.**
Rule 3 is amended in CLAUDE.md rather than quietly bent: the exception is bounded to a lookup miss, off unless the operator passes `-fallback -contact`, and degrades to dataset-only when MusicBrainz is unreachable.
Users who want nothing leaving their machine keep exactly that, and the two modes differ by one flag.

Implemented in `internal/musicbrainz` (client and mapping), `internal/ratelimit` (the shared 1 req/s gate) and `internal/source` (the chain that tries the dataset first).
No MusicBrainz API key exists or is needed for this: the web service is open, and asks only for a contactable User-Agent, which is what `-contact` supplies.
An access token is required only for the Live Data Feed replication packets, which is a build-pipeline concern rather than a user-facing one.

Known thinness of fallback results, all deliberate:

- Search hits carry no albums and no releases, because expanding them would cost one request per hit. Lidarr fetches the full entity once the user picks one, so the add flow still works.
- No images and no overviews, because MusicBrainz carries neither. Those come from build-time enrichment.
- A cold artist lookup costs 2+ requests and roughly a second each, so a large artist is slow. That is the correct trade: the dataset is the answer for large artists.

### 9.1 Who builds the dataset, and why not the user's container

Raised 2026-07-22: shipping a prebuilt artifact makes this project's maintainer a dependency, which sits awkwardly beside rule 4.
It is a fair objection and the answer is three separate things, not one.

**The build is automated, not hand-cranked.**
GitHub Actions on a cron, triggered after each twice-weekly MusicBrainz export.
Verified this fits the free tier: standard runners are free and unlimited for public repositories, `ubuntu-latest` starts with roughly 21-29 GB free (extendable to ~55 GB with a cleanup action), and the job ceiling is 6 hours.
Our input is 7.4 GB compressed and the reader streams, so the ~40 GB expanded form never touches disk; a full-archive pass costs single-digit minutes of decompression.
There is no scenario where a person is manually producing images every three days.

**The user's container deliberately does not build its own dataset.**
Three reasons, in order of weight:
1. It would break the product. Processing needs CPU, RAM and disk that a Synology NAS or a Raspberry Pi does not have, and first boot would go from seconds to the better part of an hour, or fail outright.
2. It would be inconsiderate to MetaBrainz. They are a donation-funded nonprofit; thousands of installs each pulling a 7.4 GB export twice weekly is exactly the load pattern their rate limiting and mirror documentation exists to discourage. Building once centrally and distributing the result is the polite design, not the lazy one.
3. It wastes the work. The same deterministic transform would be recomputed by every user independently.

**Self-building stays a first-class supported path.**
The pipeline ships inside the same image, so anyone who wants zero dependence on this project can point it at a MusicBrainz export and produce their own dataset with one command.
That is what removes the maintainer as a *hard* dependency rather than merely arguing about it.

**The failure mode is soft.**
If this project goes dark, existing installs keep serving forever, offline, from the dataset they already have; they stop getting fresher data, which is a slow degradation rather than an outage.
That is categorically different from `api.lidarr.audio` going down, which breaks Lidarr immediately.
Artifacts are checksummed and content-addressed, so mirroring or seeding them elsewhere needs no cooperation from us.

**Dataset update cadence is configurable, not baked in.**
The artifact download is the project's real bandwidth cost, both for the user and for whoever hosts the artifacts, so the operator chooses.
The intended surface for Phase 4, to be implemented with the updater rather than stubbed now:

| Setting | Default | Meaning |
| --- | --- | --- |
| `-update` | `weekly` | `never`, `daily`, `weekly`, or a duration. `never` means the dataset only changes when the operator replaces it. |
| `-update-window` | `random` | Spread checks across the day. MusicBrainz explicitly asks clients not to wake at a fixed hour, and the same courtesy applies to whoever serves our artifacts. |
| `-update-mode` | `delta` | `delta` pulls only changed rows; `full` re-downloads the artifact. Delta is why the format has to support row-level patches from version one. |

A user on metered bandwidth sets `-update never` and pulls a new artifact by hand.
A user who wants freshness sets `-update daily` and pays a small delta per day.

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
