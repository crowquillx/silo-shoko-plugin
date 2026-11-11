# Silo ShokoAnime VFS

Builds a virtual media library from Shoko's groups and series, with metadata and artwork for Silo. Media files are linked into a separate output directory.

Part of [Crowquillx Silo Plugins](https://github.com/crowquillx/crowquillx-silo-plugins#install). Supports Linux amd64, Linux arm64 and macOS arm64, and both stable (5.x) and daily (6.x) Shoko Server. The plugin detects which one it is talking to, so upgrading Shoko needs no configuration change.

## Install

1. Add the [Crowquillx catalog](https://github.com/crowquillx/crowquillx-silo-plugins#install) in **Administration → Plugins → Catalog** and install **ShokoAnime VFS**.
2. In **Installed**, open the plugin's **Configure** button or settings gear, then **Global Configuration**. Enter the Shoko server URL, Shoko API key and a writable **VFS output root** separate from your media folders.
3. Set **Managed-folder path map (JSON)** to map Shoko's numeric managed-folder IDs to media paths visible to both Silo and the plugin. For example:

```json
{"1": "/media/anime"}
```

4. If Shoko is not on a public address, pick an [artwork source](#artwork-source) so Silo can cache Shoko's artwork. Test the connection, which checks the URL, API key, every mapped folder and the S3 bucket if one is set, then select **Save config**. Enable the plugin's **Shoko VFS changes** Autoscan source and bind the **Reconcile Shoko VFS** scheduled task.
5. Run **Reconcile Shoko VFS** to generate the library. On a large library the task returns before the crawl ends and the crawl keeps running in the background; run the task again to see its status. When it reports `complete`, create a Silo TV library rooted at the VFS output root. Silo scans a new library straight away, so a library created before the crawl finishes starts empty: scan it once from its menu after the crawl. Keep the Autoscan source enabled to scan future changes.

The Autoscan source picks up changes from the moment it is added. If it is added after the VFS is built, or its first poll fails, its first successful poll scans the whole library once. The same happens if the VFS output root is deleted or recreated: the source notices that its place in the change journal is gone and starts over, rather than missing changes. One case is still missed: if the recreated root already holds more changes than the source had read before its next poll, the source cannot tell the journal is new and skips the earliest ones. After recreating the root, scan the library once from its menu. Polls never wait for a crawl: a crawl still in progress, including one left unfinished by a restart, continues in the background, and its changes are scanned when it finishes.

## Library layout

The library follows your Shoko groups. Each group becomes a show and each series in it becomes a season, whatever its type, so a group with many movies or OVAs has many seasons. Seasons are numbered in the order Shoko lists the group's series. Nested groups become their own shows.

Regular episodes go in their series' season, and specials, credits, trailers and other extras go in `Season 00`. Specials keep their AniDB number on the group's first series, and later series and other episode types use higher ranges, such as `S00E1001` or `S00E401`, so that no two share a number.

## Metadata

Shows get their dates, status, genres, studios, cast and AniDB rating from all of their Shoko series, and their title aliases from the group's main series. A show's title is Shoko's name for the group, so Shoko's language settings decide it; the plugin tells Silo it is in the library's language, so Silo shows it instead of an AniDB synonym. The original title is the AniDB main title. Episodes use Shoko's episode titles, except that AniDB placeholders such as `Episode 12` or `TBA` are replaced by the linked TMDB episode's title when it has a real one. Genres are the tags Shoko classifies as genres. The content rating is the strictest US rating of the linked TMDB shows, or `18+` when AniDB marks a series restricted. Seasons get their series' overview and air date, and episodes their air date, still and TMDB rating.

The AniDB rating appears in Silo once an administrator turns on the AniDB rating source the plugin declares.

## Posters

Each season uses its Shoko series' preferred poster. Shoko gives a group only its main series' poster, so on its own a show repeats its first season's poster. With the [Season Posters](https://github.com/crowquillx/shoko-season-posters) Shoko plugin installed, a show uses the show poster chosen there in its own show poster mode, and keeps the group poster otherwise. The plugin reads that list at most every 10 minutes. A changed poster appears once the show's metadata is refreshed in Silo. Specials keep the group poster. Each season also gets its Shoko series' backdrop, from the next metadata refresh on.

Silo's **Choose Image** list shows no Shoko artwork for a show or a season. Silo asks providers for that list by TMDB, TVDB or IMDb ID only, and Shoko's artwork is found by Shoko ID. The plugin answers a season's artwork request, but Silo does not send one it can use yet.

## Artwork source

Silo caches artwork in its own storage, but it only downloads artwork from public addresses. A Shoko server on a private network, such as a LAN or Tailscale address, is refused, so Silo cannot cache its posters. Clients then get Shoko's private URL and show the artwork only when they can reach Shoko themselves.

**Artwork source** in the plugin settings picks where Silo downloads Shoko artwork from:

- **Shoko Server URL** (the default) uses the Shoko Server URL. Pick it when that URL is public, or when every client can reach Shoko.
- **Public image URL** uses another URL that serves Shoko's `/api/v3/Image/<id>` path, for example a reverse proxy in front of Shoko. Shoko serves images without an API key, so let the proxy pass only that path; the rest of Shoko's API should stay private.
- **Copy to S3** copies each image into an S3-compatible bucket, such as Garage, MinIO or AWS, the first time Silo asks for it. Silo then gets a link signed for seven days. The endpoint must be public, and the key needs read and write access to the bucket. Nothing in Shoko needs a public address. Silo's own storage settings are not available to plugins, so the bucket and key are set here.

In the S3 mode, an image that cannot be copied falls back to Shoko's own URL, and the error is logged. The connection test also checks that the bucket exists.

Silo asks the plugin for an image's URL each time it downloads it, so queued downloads use a new artwork source on their next attempt. Downloads that already gave up are retried when an item is refreshed, or for the whole catalog by the **Backfill Metadata Images** task in **Administration → Tasks**.

## Theme songs

Theme songs are kept in sync between your media folders and the VFS, in both directions:

- A theme in a media folder appears in the matching VFS show or season folder. The plugin looks for `theme.<ext>`, numbered files such as `theme2.mp3`, and audio inside `theme-music`, `theme-songs`, `themes`, `theme songs` or `theme music` folders.
- A theme written into the VFS, for example by [Theme Songs](https://github.com/crowquillx/silo-theme-songs), is added to the media folder, inside the theme folder it already uses or a new `theme-music` folder.
- A theme you delete from a media folder is removed from the VFS, but only while that media folder is reachable.

Nothing in a media folder is ever overwritten or deleted, and a name clash is reported instead. Silo only plays themes that are real files, so they are hardlinked into the VFS, or copied when the VFS and your media are on different filesystems. Media folders need to be writable for themes to be added to them.

A media folder is only paired with a show or season when every Shoko file in it belongs to that show or series, so shared download folders and managed-folder roots are left alone. Themes sync after each reconcile and on every Autoscan poll.

Each link name carries the series' AniDB anime ID as `[AniDB Anime=<id>]`. Theme Songs reads it to find each season's anime, since Shoko shows have no TVDB or TMDB IDs for it to map.

A link that cannot be created is skipped and listed under `failures` in the task output, and the rest of the library is still built.

## Build

Requires Go 1.26 or newer.

```sh
make test
make build
```

The binary is `plugin`.

## Acknowledgments

- [Shoko](https://github.com/ShokoAnime/ShokoServer) for media organization, metadata and artwork.
- [Silo plugin SDK](https://github.com/Silo-Server/silo-plugin-sdk) for Silo integration.

[MIT license](LICENSE).
