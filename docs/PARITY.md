# Parity with the official metadata server

This is the evidence behind the claim that a Lidarr pointed at this server gets metadata that is on par with, or better than, `api.lidarr.audio`.
It is measured, not asserted.
Reproduce it with `go run ./cmd/parity -deep` against a running server; the harness lives in `cmd/parity/compare.go`.

The sample is eight artists chosen to span the cases parity has to survive: a mainstream band (Radiohead), a huge catalogue (The Beatles), classical with performer credits (Bach), non-Latin script (Utada Hikaru), a single-album act (The La's), a pop artist with many compilations (Rihanna), a heavy collaborator (Drake), and a deep back catalogue (Pink Floyd).
For each, the harness compares every artist field, then samples shared albums and compares their fields track by track.

## What a working import depends on, and where we stand

These are the fields Lidarr actually reads to add and match music.
Parity here is the whole point; a gap here corrupts a library.

| Signal | Result |
| --- | --- |
| Track agreement (name, number, recording id, duration) | 337 of 337 tracks across 24 albums, 100% |
| Release-count agreement per album | 24 of 24 albums match |
| Album coverage | Superset of official on every artist tested |
| Real release-date coverage | 88% of albums, versus official's 38% |
| Artist name, sortname, type, status, disambiguation | Full parity |
| Genres, links, rating (artist) | Full parity |
| Album cover art | Parity, plus one album official does not have art for |

Album coverage is a superset: across the eight artists we carry every release group the official service lists except one on Radiohead and eight on Bach, out of thousands, while carrying hundreds it omits (Drake +103, Rihanna +29, Bach +413).
The handful we lack are release groups newer than the dump's replication date, which a dataset refresh picks up, and the live fallback serves in the meantime.

Release dates deserve a note because a naive comparison gets them backwards.
Where the date is genuinely unknown, the official server emits `0001-01-01`, the .NET minimum-date sentinel, and we emit `null`.
Both mean "no date"; Lidarr already receives `null` in the artist album list from the real server and handles it, so the wire difference is benign.
The number that matters is how many albums have a real date at all, and there we resolve 88% against the official server's 38%, because we compute the earliest date across a release group's releases rather than leaving it blank.

## Genuine gaps, all cosmetic

None of these block an import or corrupt a library; they are the polish an operator sees in the Lidarr UI.
Every one of them needs data the MusicBrainz dumps do not carry, so closing them means a build-time enrichment pass, never a runtime dependency.

| Field | Official | Us | Closing it |
| --- | --- | --- | --- |
| Artist images | Present | Absent | fanart.tv (needs a key) or Wikidata to Wikimedia Commons (keyless) |
| Artist overview / biography | Present | Absent | Wikidata to Wikipedia extract (keyless) |
| Album rating | Sometimes present | Absent | `release_group_meta` rating columns, already read by the pipeline, not yet emitted |
| Album external links | Sometimes present | Absent | `l_release_group_url`, a subset we do not yet map |

Artist images are the most visible of these: a Lidarr library backed by this server currently shows no artist photos.
That is the first gap worth closing before calling the UI experience equal.

## How to re-run

```
# against the local soak server
go run ./cmd/parity -deep -base http://localhost:5001 -albums-per 3

# search-ranking parity (a separate gate)
go run ./cmd/parity -queries fixtures/search-queries.txt
```

The harness talks to `api.lidarr.audio` directly and paces itself, so it is safe to run but not to hammer.
