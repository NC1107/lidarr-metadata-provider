# Data sources and attribution

The dataset this project builds is an aggregate of open data.
Each source keeps its own license; this file records what comes from where so a published dataset carries the attribution those licenses require.

## Sources

| Data | Source | License |
| --- | --- | --- |
| Artists, release groups, releases, tracks, recordings, dates, ratings, genres, external links | [MusicBrainz](https://musicbrainz.org) full data export | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) (public domain) |
| Album cover art URLs | [Cover Art Archive](https://coverartarchive.org) | image URLs are referenced, not redistributed |
| Artist image links, English Wikipedia article titles | [Wikidata](https://www.wikidata.org) (property P434) | [CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/) |
| Artist images | [Wikimedia Commons](https://commons.wikimedia.org) | referenced by URL, not redistributed; each file keeps its own license |
| Artist biographies | [English Wikipedia](https://en.wikipedia.org) article summaries | [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/) |

## What this means for a published dataset

Most of the dataset is MusicBrainz and Wikidata, both CC0, which may be redistributed without restriction.

Images are never copied into the dataset.
Only their URLs are stored, pointing at Wikimedia Commons and the Cover Art Archive, exactly as the official service points at its own image host.
The image bytes are fetched by the client at display time and keep whatever license their uploader gave them.

Artist biography text is the one component under a share-alike license.
It is the lead summary of the artist's English Wikipedia article, licensed CC BY-SA 4.0.
The dataset attributes it in two ways: every enriched artist carries a link to its Wikidata item and, through it, the Wikipedia article the text is drawn from, and this file records the source and license for the dataset as a whole.
A dataset that redistributes these summaries is a CC BY-SA work with respect to that text and must be shared under the same terms with this attribution intact.

## Building without the enrichment

The enrichment is optional.
A dump-only build carries no images or biographies and is therefore pure CC0 MusicBrainz data.
See [BUILDING.md](BUILDING.md); omit the enrichment file and the `enrich` step, and the dataset contains nothing but the MusicBrainz export.
