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

4. Test the connection, which checks the URL, API key and every mapped folder, then select **Save config**. Enable the plugin's **Shoko VFS changes** Autoscan source and bind the **Reconcile Shoko VFS** scheduled task.
5. Run **Reconcile Shoko VFS** to generate the library. On a large library the task returns before the crawl ends and the crawl keeps running in the background; run the task again to see its status. When it reports `complete`, create a Silo TV library rooted at the VFS output root. Silo scans a new library straight away, so a library created before the crawl finishes starts empty: scan it once from its menu after the crawl. Keep the Autoscan source enabled to scan future changes.

The Autoscan source picks up changes from the moment it is added. If it is added after the VFS is built, or its first poll fails, its first successful poll scans the whole library once. Polls never wait for a crawl: a crawl still in progress, including one left unfinished by a restart, continues in the background, and its changes are scanned when it finishes.

## Library layout

The library follows your Shoko groups. Each group becomes a show and each series in it becomes a season, whatever its type, so a group with many movies or OVAs has many seasons. Seasons are numbered in the order Shoko lists the group's series. Nested groups become their own shows.

Regular episodes go in their series' season, and specials, credits, trailers and other extras go in `Season 00`. Specials keep their AniDB number on the group's first series, and later series and other episode types use higher ranges, such as `S00E1001` or `S00E401`, so that no two share a number.

## Metadata

Shows get their dates, status, genres, studios, cast and AniDB rating from all of their Shoko series, and their title aliases from the group's main series. Genres are the tags Shoko classifies as genres. The content rating is the strictest US rating of the linked TMDB shows, or `18+` when AniDB marks a series restricted. Seasons get their series' overview and air date, and episodes their air date, still and TMDB rating.

The AniDB rating appears in Silo once an administrator turns on the AniDB rating source the plugin declares.

## Posters

Each season uses its Shoko series' preferred poster. Shoko gives a group only its main series' poster, so on its own a show repeats its first season's poster. With the [Season Posters](https://github.com/crowquillx/shoko-season-posters) Shoko plugin installed, a show uses the show poster chosen there in its own show poster mode, and keeps the group poster otherwise. The plugin reads that list at most every 10 minutes. A changed poster appears once the show's metadata is refreshed in Silo. Specials keep the group poster.

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
