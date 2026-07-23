# Project audit - 2026-07-23

Five independent audits ran against the repo, the real 8.73 GB dataset (`full-enriched-v2.db`), the live soak server, the golden fixtures, and the actual MusicBrainz dumps: an adversarial CI audit, a build performance audit, an adversarial data-correctness audit, a code quality review, and a dataset size audit.
Everything below was verified against real code, real data, or a working prototype - findings the auditors could not substantiate were dropped, and two audits independently confirmed the same `DictBuilder` bug.
This file is the backlog: each item has evidence, a concrete fix, and an effort tag (S/M/L).

## Resolved 2026-07-23

The first pass fixed every P0 and P1, plus the safe size and robustness wins.
Items 1-10, 14, 15, 16, 19, 20, 25, and the linkType half of 26 are done and shipped, each with a regression test where one applied:
the fallback safeguard, the biography-fetch data race, the DictBuilder failure handling, genre casing and link typing, the dead maps, BetterCompression, atomic build output, the resilient download, and the server and CI hardening.

Still open, in rough priority: the schema-changing size wins (12 FTS external content, 18 mbid blobs) and the larger memory and speed work (13 stage releases, 17 parallel emit, 11 transport compression) which want their own focused changes and a search-parity run; the smaller correctness items (22 search-hint aliases, 23 tar-order guards, 24 statusMask logging); the metrics wiring (21); the design work (28-30); and the doc pass (27).
Item 12 was deliberately deferred because it changes the schema, which would make an updated server refuse an existing on-disk dataset; it needs a re-download-on-mismatch migration path first.

Headline numbers if the backlog lands:

- Download size: roughly -2.4 to -2.6 GB (about 28%) from transport compression, FTS restructuring, and compression tuning combined (the naive per-item sum is larger; transport compression overlaps with the FTS savings).
- Build peak memory: roughly -2.6 to -2.8 GB (about 20%) from staging releases in SQLite and deleting dead maps.
- Two CRITICAL automation bugs and one CRITICAL correctness gap that would each have caused real failures unattended.

## P0 - critical, fix before relying on the affected path

### 1. The live MusicBrainz fallback never runs the featured-artist safeguard
The dataset path repairs track-artist references at build time (`internal/pipeline/album.go` `completeAlbumArtists`) and again at serve time (`internal/dataset/reader.go`), because a track crediting an artist missing from the album's `Artists[]` makes Lidarr throw a KeyNotFoundException and discard the whole album.
The fallback path has neither: `internal/musicbrainz/entities.go` builds `Artists` only from the release-group credit and `toTrack` sets `track.ArtistID` independently, with no repair anywhere between the handler and the client (verified layer by layer).
New releases with a featured guest are exactly what the fallback exists to serve, and exactly the releases most likely to have per-track guests.
Fix: port the `reader.go` repair to `internal/musicbrainz` (or centralize it in `internal/source` so every source gets it), plus a test feeding a hand-built API response with a track-level-only guest through `toAlbum`.
No test anywhere enforces this invariant on any of the three paths today.
Effort: S.

### 2. The dataset cron never publishes
`.github/workflows/dataset.yml` gates the Publish step on `if: inputs.publish != false`.
On a `schedule` trigger, `inputs.publish` is null, and GitHub's expression rules coerce both null and false to 0, so the condition is false and publishing is silently skipped on every automatic run - the job still shows green.
Fix: `if: github.event_name != 'workflow_dispatch' || inputs.publish != false`.
Effort: S.

### 3. CI disk arithmetic does not close
Peak simultaneous disk during the build step: dumps 7.5 GB (not freed until a later step) + staging 7.9 GB + output 8.8 GB is about 24.2 GB, and the `VACUUM` inside `Writer.Finish` then needs up to 2x the database size in scratch while the dumps are still on disk, pushing the second peak to roughly 26-30 GB.
The workflow's ad hoc `rm -rf` reclaim step yields roughly 21-29 GB free, so the low end fails with certainty.
Fix: use a maintained disk-reclaim action (for example `jlumbroso/free-disk-space`, which reaches about 50 GB), and delete the dumps before `Finish`/`VACUUM` runs rather than in a separate later step.
Effort: S-M.

### 4. CI memory limit is below the measured peak
`cmd/pipeline/main.go` defaults `debug.SetMemoryLimit` to 10 GB and the workflow never overrides it, but the measured build peak is 12.6 GB.
A soft limit below the live working set makes the GC run continually against a target it cannot reach (documented livelock risk) while competing with the encoder pool for the runner's 4 cores.
Fix: set `LMP_MEMORY_LIMIT_GB=13` in the workflow build step; revisit after item 12 lands and the real peak drops.
Effort: S.

## P1 - high: real bugs and wrong data being served

### 5. Data race in the biography fetcher
`internal/enrich/wikipedia.go` `FetchBios`: workers write `a.Overview` directly while the periodic checkpoint (which serializes the same structs to the cache file) reads them holding only its own mutex.
Reproduced under `-race` with an isolated repro; worst case is corrupted biography text written into the shipped cache.
Fix: route results through a single collector goroutine that owns all artist mutation and calls the checkpoint itself (the pattern `internal/dataset/writer.go` already uses), or guard the field writes with the same mutex.
Add a test that exercises the pool; today nothing does, which is why `-race` never caught it.
Effort: S-M.

### 6. `DictBuilder` mishandles mid-flush failure (found independently by two audits)
`internal/dataset/dictbuild.go`: `train()` never sets `b.err` and never closes the just-created `Parallel` on a flush error, so a later `Close()` silently retrains, creates a second `Parallel` over a writer documented as not concurrency-safe, resends the entire buffered batch (double-write risk), and leaks the first pool's goroutines forever - then reports nil.
Reachable in production via any transient SQLite error during the 40k-payload flush; currently masked only because `cmd/pipeline/main.go` discards `par.Close()`'s error on the failure path and `Finish()` never runs, so the broken file is refused at open.
Fix: set `b.err` on every `train()` failure and close the old `Parallel` before creating another; stop discarding `Close()`'s error in `cmd/pipeline`.
Effort: S.

### 7. Genre casing is wrong for hyphenated and ampersand genres
`internal/pipeline/enrich.go` `titleCase` splits only on whitespace, so we serve `R&b`, `J-pop`, `Dance-pop`, `Singer-songwriter` where upstream serves `R&B`, `J-Pop`, `Dance-Pop`, `Singer-Songwriter` (verified against the Utada Hikaru golden fixture and our live server side by side).
Measured against the shipped dataset: 103 distinct genre strings affected, with thousands of occurrences each (`Singer-songwriter` 3,150, `Lo-fi` 2,626, `R&b` 1,851, `J-pop` 1,236, `K-pop` 726, and 98 more in the top-300k artist sample alone).
Fix: treat `-` and `&` as word boundaries in `titleCase`.
Effort: S.

### 8. Link typing diverges from upstream for `gov.<cc>` and `co.kr` domains
`internal/pipeline/enrich.go` `isPublicSuffix` includes `gov`, `com`, `org`, `net`, `ac`, but the fixtures prove upstream only compound-strips `co.uk`/`co.jp`-style domains: `nla.gov.au` is typed `gov` upstream (that National Library of Australia link recurs across most well-documented artists in the fixture set) while our code returns `nla`, and `music.bugs.co.kr` is typed `co` upstream while our code returns `bugs`.
Fix: narrow the list to what the fixtures verify (`co` alone reproduces every confirmed case) and add a fixture-driven test for `linkType` so additions are checked before shipping.
Effort: S.

### 9. Wikidata harvest has no retry
`internal/enrich/wikidata.go` `Harvest` aborts the entire run on the first transient WDQS error across its 16 sequential queries, while the Wikipedia fetch alongside it retries with backoff.
One 429 kills an unattended multi-hour build until the next cron days later.
Fix: wrap `harvestPrefix` in the same retry+backoff pattern `fetchExtracts` uses.
Effort: S.

### 10. CI shell hygiene batch
The build step's trailing `|| true` is attached to the whole pipeline under pipefail, so a crash or OOM of `pipeline build` itself reports success (fix: scope it to the grep: `| (grep -Ev "^\s" || true)`).
No `curl` call in the workflow has retry or stall protection (fix: `--retry 3 --retry-delay 5 --speed-limit 1024 --speed-time 60` everywhere).
`gh release view` treats any API error as "no release yet", causing spurious rebuilds.
A re-published same-day export never rebuilds automatically, and a manual rebuild leaves stale release title/notes (fix: `gh release edit` unconditionally on republish).
The smoke test checks one artist and a key name; add a floor check on total counts via `pipeline stats`.
Raise `timeout-minutes` toward 360-420: the cold-cache 4-vCPU worst case estimate spans 140-300+ minutes against the current 300.
Effort: S each.

## P2 - size and speed: the measured big wins

### 11. Ship the artifact whole-file zstd-compressed for transport (~1.7 GB, ~20% of the download)
Measured on real page samples from the actual artifact: the whole file compresses 1.24x blended at zstd -19 (indexes and FTS at 2-2.5x, even the already-compressed payload tables give up 12-23%), taking the 8.73 GB artifact to an estimated 7.55 GB.
Cost: about 8 min multi-threaded at build time, about 10 s of streaming decompression on first boot, zero runtime cost.
Fix: compress after `VACUUM`, split the compressed file into parts, checksum the compressed artifact, and wrap the fetch path's part-join in a streaming `zstd.Decoder` so nothing extra lands on disk.
Note: savings overlap with item 12; landing both yields roughly 2.1-2.2 GB, not the 2.5 GB naive sum.
Effort: M.

### 12. FTS5 external content + `detail=none` (~770 MB, permanent, on disk and download)
The FTS tables duplicate every indexed string in `_content` shadow tables: 627 MB measured exactly via `dbstat` (`album_fts_content` 442 MB + `artist_fts_content` 185 MB).
External-content mode eliminates the shadow tables entirely - verified with a working 200k-row prototype including a correct `MATCH`/`rank` query.
`detail=none` shrinks `*_fts_data` to about 42% (another ~140 MB) and produced byte-identical bm25 ordering in the prototype; safe because `ftsQuery` never issues phrase queries.
Two required safety details: give `artist`/`album` an explicit `id INTEGER PRIMARY KEY` so `VACUUM` cannot renumber rowids out from under `content_rowid`, and rename the album FTS column `title` to `norm` so name-based column resolution lines up.
Run the search parity gate before shipping.
Effort: M.

### 13. Stage releases and media in SQLite instead of RAM (~2.3-2.5 GB off peak memory)
The code's own comments claim releases+media cost "a few hundred megabytes"; the real numbers (5.64 M releases, 6.20 M media, 13.2 M release_country rows, 4.8 M release_label rows, struct sizes measured with `unsafe.Sizeof`) put it at 2.3-2.4 GB held for the entire build.
Fix: extend the existing `trackStream` staging pattern (already trusted for 96 M rows) to releases and media; the added staging volume is about +12%.
Effort: M.

### 14. Free per-group release data during album emission
`emitAll` deletes each artist after emitting (with a comment explaining why); `emitAlbums` walks the identical pattern but never frees `releasesByRG`/`releaseByID`, and none of the release-level maps are read after album emission (verified by grep).
Fix: delete per-group entries in the loop and nil the release-only maps before artist emission.
Effort: S.

### 15. Delete dead and stale maps (~280-330 MB)
`groupIDs` (4.4 M entries, ~132 MB) is written once per release group and never read anywhere - three-line deletion.
`neededURLs`/`neededTags` (~150-200 MB) are only used during the archive scan but stay alive for the rest of the build; nil them after both `ReadTables` calls in `scan()`.
Effort: S, zero risk (confirmed unread).

### 16. Compression tuning (~200-300 MB download)
The zstd dictionary trains on the first 40k payloads, which are the oldest release groups; measured 22% worse compression on the newest content, which is exactly what every future rebuild adds.
Fix: stride-sample across the corpus instead of buffering the front, and bump `DictionarySize` from 112 KB to about 224 KB (+3-4% measured).
Separately, `SpeedBetterCompression` gives 1.5-1.7% smaller payloads for 1.4x encode time (measured; `SpeedBestCompression` is 28x slower and stays rejected).
A second, artist-shaped dictionary is a bounded ~45-90 MB extra if ever wanted.
Effort: S.

### 17. Profile, then parallelize album emission (potentially minutes off the build)
Album emission is about 18 of the 21.5 build minutes and the assembly side (`emitAlbums` + `fullAlbum` + one SQL cursor) is single-threaded; benchmark evidence implies compression is not the whole cost.
First step is a CPU profile bracketing `emitAlbums`; if assembly dominates, shard the sorted album-id space into per-core ranges with `rg BETWEEN` cursors feeding the existing concurrent-safe `Parallel.jobs`.
Must land together with the stride dictionary sampling (item 16) since parallel emission breaks "first 40k" determinism.
Effort: L, profile first.

### 18. mbid TEXT to BLOB(16) (~300 MB, later)
36-byte hyphenated hex mbids across both tables, both alias tables, and their indexes are inherently ~2.25x oversized; the measured 2.1x zstd ratio on `idx_album_mbid` confirms the ceiling.
Evaluated `WITHOUT ROWID` keying as an alternative: nets only ~90 MB after secondary-index growth, not worth it.
Do this only after items 11-12; it costs debugging ergonomics (opaque blobs in the sqlite3 CLI).
Effort: L.

## P3 - robustness and hygiene

### 19. Atomic build output
`dataset.Create` deletes the previous artifact before a multi-hour build begins, so a failure at hour two destroys the last good dataset - the opposite of the verify-then-install pattern `internal/checksum` documents and `fetch.go` follows.
Fix: build to `path+".building"` and rename only after `Finish()` succeeds.
Effort: S.

### 20. Dataset download stall handling
All fetch-path requests use bare `http.DefaultClient` (no timeout); a trickling connection on first boot hangs for the caller's full 6-hour deadline, and any single part error fails the whole multi-GB transfer with no per-part retry.
Fix: dedicated client with `ResponseHeaderTimeout`/`IdleConnTimeout` plus stall detection (mirroring `enrich.DefaultClient`), and per-part retry with backoff.
Effort: M.

### 21. `Metrics.ObserveFallback` is dead code
The counter documented as "the one worth watching" (it tells an operator the dataset is going stale) is never called; `source.Chain` never reports which source answered.
Fix: return or record the answering source in `Chain` and wire the counter, or delete it.
Effort: S-M.

### 22. Filter "Search hint" aliases
`readArtistAlias` ignores `ArtistAliasTypeID`, so MusicBrainz-internal search-hint aliases (including a genuine mojibake artifact on Utada Hikaru) leak into `ArtistAliases[]`; the fixture and a live MusicBrainz query confirm upstream excludes that alias class.
Fix: read the type column, load the `alias_type` lookup (existing `readTypeTable` pattern), exclude "Search hint".
Effort: S-M.

### 23. Generalize tar-ordering guards
`urlPhase`/`tagPhase` protect 2 of the 8+ order-dependent joins; `release_group_secondary_type_join`, the medium-to-release buffer, and both gid-redirect reads would all silently drop data dataset-wide if a future export reordered tables.
Fix: assert expected table order upfront via `Archive.Tables()`, or extend the phase-flag pattern.
Effort: S-M.

### 24. `statusMask` ceiling should log
Only 7 release statuses exist today (verified), far under the uint32 ceiling of 31, but an ID above it is silently dropped, which would make matching albums invisible with zero signal.
Fix: log once when an out-of-range status ID is seen.
Effort: S.

### 25. Server hardening batch
`http.Server` sets only `ReadHeaderTimeout`; add `WriteTimeout` (~30 s) and `IdleTimeout` (~120 s).
`ui.go`'s comparison proxy reads the upstream body with no `io.LimitReader` (the only unbounded external read in the repo).
`writeLookupError` sends raw internal error text to clients; return a generic message and keep detail in the log.
`dataset.Reader`: add `SetMaxOpenConns` and prepare the two hottest statements once in `Open()`.
Effort: S each.

### 26. Error-handling batch
`Parallel`/`DictBuilder` panic (send on closed channel) on Add-after-Close instead of returning an error.
Swallowed errors: `ui.go` `summarize()` unmarshals, `writer.go` encoder `Close()` twice, `cmd/parity/compare.go` request/read errors, `cmd/pipeline` discarding `par.Close()` on the failure path (ties into item 6).
`cmd/probe`'s User-Agent starts with lowercase `lidarr`, violating the repo's own test-guarded MusicBrainz rule; capitalize it.
`cmd/parity`/`cmd/probe` build a fresh `http.Client` per call inside loops; hoist one client.
`linkType` mis-parses userinfo URLs, trailing-dot FQDNs, and bare IPs (theoretical exposure; fix while touching item 8).
Serve-time `completeAlbumArtists` decodes a full artist payload per missing artist with no cache; dormant today (a full 4.4 M album scan found zero repairs needed) but add a small LRU if the invariant ever regresses.
Effort: S each.

## P4 - documentation accuracy

### 27. BUILDING.md understates the build by 2x
The docs claim "peak memory around 6 GB"; the measured peak is 12.6 GB, and someone self-building on an 8 GB machine (a first-class supported path) would OOM.
Row counts are also stale: "roughly 35 million tracks and 30 million recordings" versus the real 56.9 M and 39.5 M, and the "few hundred megabytes" claim corrected by item 13.
Update after items 13-15 land so the numbers are written once, correctly.
Effort: S.

## Design work queued behind the backlog

### 28. Incremental dataset updates: diff two full builds
Do not make the pipeline incremental (the artist/album cross-references make change tracking a correctness minefield); instead keep building full datasets and add `pipeline diff old.db new.db` producing a compact mbid-keyed patch via `ATTACH` + `EXCEPT`, published beside the full artifact.
Client-side: apply the patch to a copy, update FTS via normal DML, verify, atomically swap via the existing `checksum.Install` path.
The diff of two correct full builds is structurally correct no matter what changed upstream.
Test: N sequential patches must byte-match a from-scratch build.
Effort: S for the diff tool, M for client apply, M-L end to end.

### 29. Zero-album artist analysis
62.85% of artists (1,844,467) have zero albums, an upper bound of ~600 MB across all structures, but many exist solely so track credits can resolve, and pruning a referenced one would silently mislabel credits.
Prerequisite: a build-time pass computing which zero-album artists are never referenced by any track or album credit; only those are candidates.
High risk if done naively (classical and compilation libraries), so analysis first.
Effort: M analysis, L implementation.

### 30. Close the test-infrastructure gap that let items 7, 8, and 22 happen
`ContractDiff`/`deepCompare` check presence and shape but never exact content for genres, links, and aliases, which is the shared root cause of every "wrong cosmetic data" finding.
Add fixture-driven exact-content assertions for those fields, a test exercising the `FetchBios` worker pool, and tests for the featured-artist invariant on all three serving paths.
Effort: M.

## Verified sound (attacked and survived)

- Full-dataset scans (all 4.4 M albums, not samples): zero track-artist violations, zero TrackCount mismatches, zero empty album types, and the build-time featured-artist repair is 100% effective on the dataset path.
- The 5.34% of albums with empty `ReleaseStatuses` faithfully reproduce upstream's own quirk (which the contract requires).
- Contract shape and casing: 5 random mid-size artists plus the fixture round-trips all match; CJK exact-match search works.
- `ratelimit.Limiter` correctness under concurrency, MusicBrainz client body handling, `source.Chain`, the shared zstd decoder's thread safety, graceful shutdown, and the checksum verify-then-install path all check out.
- Release-asset part sizing (1900 MiB vs the 2 GiB cap), the enrich cache atomic save, and the check/build job output wiring are correct.
- `go build`, `go vet`, `go test -race ./...` all clean.

## Suggested execution order

1. P0 items 1-4 plus the P1 one-liners (2, 9, 10 partially) - one short session, mostly S effort, removes every known unattended-failure mode.
2. Items 7, 8, 22 (wrong-data fixes) plus item 30's exact-content tests so they cannot regress - then rebuild and republish the dataset once.
3. Items 5, 6, 19, and the item 26 batch - correctness of the build machinery.
4. Items 12, 15, 16 in one schema bump, then item 11 (transport compression), then item 13/14 - each independently testable, one rebuild each or batched.
5. Item 17 (profile first), items 18, 28, 29 as capacity allows.

## Second-pass gaps (adversarial sweep, 2026-07-23)

A follow-up sweep for algorithmically-derived fields that must match upstream turned up a clear theme: the opt-in live-fallback path (`internal/musicbrainz/entities.go`) is a second, thinner implementation of resource assembly that never received three fixes made to the dataset pipeline (`internal/pipeline`).
Each item below was cross-checked against the golden fixtures and, where the fixture was ambiguous, against live `musicbrainz.org`.
Only the default dataset path ships by default, so the fallback items affect operators who pass `-fallback` only; the dataset-path items affect the shipped artifact but are cosmetic (order/duplication/empty fields Lidarr does not filter on).

Priority order:

31. **Fallback album `Type` never defaults to "Other"** (functional, fallback-only). `entities.go:225` `primaryType` returns `""` for an untyped release group; the pipeline defaults it to `"Other"` (`artist.go:707-720`) precisely because an empty `Type` matches no metadata profile and makes the album unmonitorable. Untyped release groups are real (the pipeline's own fixtures model them). Fix: factor the "Other" default into a shared helper both paths call. Effort S.
32. **Fallback link `Type` uses MusicBrainz's relation vocabulary, not the domain-derived label** (cosmetic, 100% wrong on fallback). `entities.go:156` emits `r.Type` ("official homepage", "social network", "IMDb") where upstream emits the second-level domain ("discogs", "imdb", "tiktok"). Fix: extract `linkType` from `enrich.go` into a shared package and call it in `mapLinks`. Effort S.
33. **Fallback genres are never title-cased** (cosmetic, fallback-only). `entities.go:130` passes MusicBrainz's raw lowercase names through; the pipeline applies `titleCase`. Fix: call the shared `titleCase` in `mapGenres`. Effort S.
    31-33 share one remedy: extract `primaryType`-with-"Other", `linkType`, and `titleCase` into a package both `pipeline` and `musicbrainz` import, so the two paths cannot drift again.
34. **Dataset sorts and name-dedupes `ArtistAliases`/`OldIDs`/`Links` against upstream order** (cosmetic, wide blast radius, fix uncertain). Fixtures show Bach's `oldids` are not alphabetical and his `artistaliases` keep literal duplicates; the Beatles' `links` are in neither alphabetical nor type order. `sortedUnique` (`artist.go:764`) and the `linksFor` sort (`enrich.go:230`) impose an order upstream does not use, and `artist_test.go:265` actively pins the wrong (deduped) behavior. Investigate whether natural table-read order reproduces upstream before changing; the dumps are static so read-order is already deterministic, making the "for determinism" sort unnecessary. Effort M (investigation first).
35. **Genre tie-break (equal vote count) is alphabetical; upstream's is not** (cosmetic, fix unknown). Prince's fixtures show count-tied genres ordered non-alphabetically (`Neo-Psychedelia, Soul, R&B` at count 4). The descending-count primary sort is correct; only the tie-break (`enrich.go:190`) is wrong, and the true key (likely tag-row insertion order) needs to be confirmed available through the scan. Effort M (investigation first).
36. **`Release.Label` dedupes same-name labels upstream keeps** (cosmetic, narrow). The Pulp Fiction OST fixture keeps `["MCA Records", "MCA Records"]`; `album.go`'s `seen[name]` drops the duplicate. Fix: dedupe by label id, or not at all. Effort S.
37. **Album `Aliases` is never populated** (cosmetic content gap, all albums). Both paths hardcode `[]string{}`; no handler reads `release_group_alias`, though the Pulp Fiction OST fixture carries `"aliases": ["Pulp Fiction"]`. Fix: add a `release_group_alias` handler and wire it into album emit. Effort M.

Suggested handling: ship the current dataset rebuild (it fixes the widespread tiktok `/@handle` link regression and is validated), then take 31-33+36 as one shared-helper change, 37 as a small feature, and 34-35 as investigations that must establish upstream's real order before any code changes.
