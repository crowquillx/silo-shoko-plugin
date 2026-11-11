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

4. If Shoko is not on a public address, pick an [artwork source](#artwork-source) so Silo can cache Shoko's artwork. To have Silo refresh shows when their metadata or artwork changes in Shoko, also fill in the [Silo refresh settings](#refreshing-silo). Test the connection, which checks the URL, API key, every mapped folder, the S3 bucket if one is set and the Silo settings if they are set, then select **Save config**. Enable the plugin's **Shoko VFS changes** Autoscan source, set the **default poll interval** in Silo's Autoscan settings to 30 to 60 seconds, and bind the **Reconcile Shoko VFS** scheduled task to run every few hours or daily. Silo polls every source at the default interval at most, so a source's own interval can lengthen it but not shorten it.
5. Run **Reconcile Shoko VFS** to generate the library. On a large library the task returns before the crawl ends and the crawl keeps running in the background; run the task again to see its status. When it reports `complete`, create a Silo TV library rooted at the VFS output root. Silo scans a new library straight away, so a library created before the crawl finishes starts empty: scan it once from its menu after the crawl. Keep the Autoscan source enabled to scan future changes. From then on the plugin [follows Shoko's changes live](#live-updates).

The Autoscan source picks up changes from the moment it is added. If it is added after the VFS is built, or its first poll fails, its first successful poll scans the whole library once. The same happens if the VFS output root is deleted or recreated: the source notices that its place in the change journal is gone and starts over, rather than missing changes. One case is still missed: if the recreated root already holds more changes than the source had read before its next poll, the source cannot tell the journal is new and skips the earliest ones. After recreating the root, scan the library once from its menu. Polls never wait for a crawl: a crawl still in progress, including one left unfinished by a restart, continues in the background, and its changes are scanned when it finishes.

## Live updates

The plugin listens to Shoko Server's events, as Shokofin does, so the VFS follows Shoko within seconds instead of waiting for the reconcile task:

- When Shoko matches a new file to episodes, removes a match, moves, renames or deletes a file, or marks it ignored or a variation, the plugin reads that file again from Shoko, with its series and group, and updates only the links that change. A new episode is in the VFS about 10 seconds after Shoko matches it, and in Silo at the Autoscan source's next poll.
- When a series' or episode's metadata changes, for example its episode type or number, the plugin reads that series again and moves its links if their names change.
- On daily Shoko, when a group is renamed, a series moves to another group, or every group is recreated, the plugin renames or moves the show folders affected, or crawls everything after a recreation. Stable Shoko sends no group events, so there a group change reaches the VFS with the next reconcile.
- Events are gathered into one update once Shoko has been quiet for 10 seconds, or after a minute at most, so a large import does not update the VFS file by file.

Shoko does not replay events it sent while the plugin was not listening. So when the plugin connects again after losing the connection, or starts more than 10 minutes after the last update, it crawls the whole library once in the background. The **Reconcile Shoko VFS** task still crawls everything each time it runs, to repair anything an event missed, so it can run every few hours or daily rather than often.

Updates apply to the data of the last full crawl, which the plugin keeps in the VFS output root. An update checks only the links it changes, and syncs theme songs only in the show and season folders they are in, since on a media share where every file lookup is slow, checking all of a large library takes minutes. The reconcile task and the crawls after a reconnect check every link. After upgrading from a version without live updates, or changing the managed-folder map, the plugin crawls once when it starts listening to have that data. A new output root is empty until the reconcile task builds it, as on first install.

To turn live updates off, switch off **Follow Shoko changes live**. The VFS then changes only when the reconcile task runs. A new plugin process waits 30 seconds before it connects, so the short-lived one Silo starts to test a connection never does.

Silo does not show plugin logs among its own, so the **Reconcile Shoko VFS** task output reports live updates under `live`: whether the plugin is connected to Shoko, the events and batches it has received, how long the last update took and how many links it changed, any error, and the Silo refreshes done and waiting.

### Refreshing Silo

Silo fetches a show's metadata when it first scans it, and later only when asked. To have the plugin ask Silo to refresh a show when Shoko changes it, set **Silo server URL (for refreshes)** to a URL the plugin reaches Silo at, such as `http://localhost:8080`, and **Silo admin API key (for refreshes)** to an API key of a Silo administrator with no scope limits. Silo gives plugins no other way to start a refresh. The plugin then refreshes a show in Silo:

- when Shoko reports a change to the metadata of any of its series or episodes, from AniDB, TMDB or Shoko itself, or a changed link to TMDB or another provider;
- when an image of one of them is downloaded, removed, or picked as preferred, in Shoko's Web UI or by a plugin such as [Season Posters](https://github.com/crowquillx/shoko-season-posters) setting season posters;
- on daily Shoko, when its group is renamed, gains or loses a series, or has its artwork changed;
- when the show poster chosen for it with Season Posters changes. That plugin sends no event for show posters, so its list is compared every 5 minutes.

A Silo show refresh also refreshes its seasons, episodes and artwork. The plugin finds the show in the Silo libraries that scan the VFS output root: by the `[Shoko Group=<id>]` marker in its file names among the library's roots, or, for a show Silo added through an autoscan, which is not listed among the roots until a full library scan, by searching the catalog for its title and checking that its folder is the show's VFS folder. A show Silo has not scanned yet is skipped, since its first scan fetches its metadata.

Shoko reports one update as several events spread over minutes, such as the AniDB metadata first and then each image as it finishes downloading. So a show is refreshed once it has had no new change for a minute, or 10 minutes after its first change if changes keep coming, and at most once every 5 minutes; a change in that time waits for the next refresh rather than being dropped. Refreshes of different shows are 2 seconds apart, so Shoko's nightly AniDB update, which reports only the shows whose data actually changed, does not flood Silo's job queue.

Silo downloads an image again only when its address changes. New artwork in Shoko always has a new image ID, and so a new address: Shoko reuses an ID only to download the same remote file again.

## Library layout

The library follows your Shoko groups. Each group becomes a show and each series in it becomes a season, whatever its type, so a group with many movies or OVAs has many seasons. Seasons are numbered in the order Shoko lists the group's series. Nested groups become their own shows.

Regular episodes go in their series' season, and specials, credits, trailers and other extras go in `Season 00`. Specials keep their AniDB number on the group's first series, and later series and other episode types use higher ranges, such as `S00E1001` or `S00E401`, so that no two share a number.

## Metadata

Shows get their dates, status, genres, studios, cast and AniDB rating from all of their Shoko series, and their title aliases from the group's main series. A show's title is Shoko's name for the group, so Shoko's language settings decide it; the plugin tells Silo it is in the library's language, so Silo shows it instead of an AniDB synonym. The original title is the AniDB main title. Episodes use Shoko's episode titles, except that AniDB placeholders such as `Episode 12` or `TBA` are replaced by the linked TMDB episode's title when it has a real one. Genres are the tags Shoko classifies as genres. The content rating is the strictest US rating of the linked TMDB shows, or `18+` when AniDB marks a series restricted. Seasons get their series' overview and air date, and episodes their air date, still and TMDB rating.

The AniDB rating appears in Silo once an administrator turns on the AniDB rating source the plugin declares.

## Posters

Each season uses its Shoko series' preferred poster. Shoko gives a group only its main series' poster, so on its own a show repeats its first season's poster. With the [Season Posters](https://github.com/crowquillx/shoko-season-posters) Shoko plugin installed, a show uses the show poster chosen there in its own show poster mode, and keeps the group poster otherwise. The plugin reads that list at most every 10 minutes. A changed poster appears once the show's metadata is refreshed in Silo, which the plugin [asks Silo to do](#refreshing-silo) when the Silo refresh settings are set. Specials keep the group poster. Each season also gets its Shoko series' backdrop, from the next metadata refresh on.

Silo's artwork picker shows no Shoko artwork for a show or a season. Silo asks providers for that list by TMDB, TVDB or IMDb ID only, and Shoko's artwork is found by Shoko ID. The plugin answers a season's artwork request, but Silo does not send one it can use yet.

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

A media folder is only paired with a show or season when every Shoko file in it belongs to that show or series, so shared download folders and managed-folder roots are left alone. Themes sync after each reconcile, and on Autoscan polls at most every 5 minutes.

Each link name carries the series' AniDB anime ID as `[AniDB Anime=<id>]`. Theme Songs reads it to find each season's anime, since Shoko shows have no TVDB or TMDB IDs for it to map.

A link that cannot be created is skipped and listed under `failures` in the task output, and the rest of the library is still built.

## Watched status

The plugin can mark an episode watched in Shoko when you finish playing it in Silo, as Shokofin does for Jellyfin, and can import the episodes you watched in Shoko into Silo. Both are off until a profile connects the Shoko watch provider, and importing also needs its own switch. Only new completions are sent to Shoko: plays from before the connection, manual watched marks and progress stay in Silo.

1. In Shoko's Web UI, open **Settings → API Keys** and generate a key while signed in as the Shoko user whose watched status you want updated. Each Silo profile can use its own Shoko user.
2. In the Silo profile, open **Settings → Watch Providers → Shoko → Connect**. Enter the Shoko API key, then a Silo server URL the plugin can reach (such as `http://localhost:8080` when the plugin runs on the Silo host), a Silo API key for the account that owns the profile, and the profile's ID from `GET /api/v1/profiles`.

Silo's watch events name only the Silo item, not the Shoko episode, so on each finished play the plugin reads the item from Silo as that profile and takes the Shoko episode from its VFS file name. Items outside the Shoko VFS are skipped. The plugin uses the Shoko Server URL from its global configuration and marks the episode with `POST /api/v3/Episode/<id>/Watched/true`, which also marks the episode's files watched. Each marked episode is logged.

A play counts as finished when Silo reports it completed. Some players stop at position zero after finishing; such a stop counts when Silo has the item marked played for the profile. If Shoko or Silo cannot be reached when a play finishes, the failure is logged and shown on the connection. Silo retries playback stops from Jellyfin-compatible clients, but not from its own apps, and the plugin does not export watched history, so such a play may need marking in Shoko by hand.

### Importing from Shoko

To bring watched status the other way, turn on **Import watched episodes from Shoko** in the connection's sync options. It is off by default. Silo's own **Import watched** switch for the connection must also be on, which it is for a new connection. On each sync the plugin lists every episode the connection's Shoko user has watched, and Silo marks the matching episodes watched in the profile, using Shoko's last watched date. Importing only adds watches: unwatching an episode in Shoko does not unwatch it in Silo.

Silo matches imported episodes by their TVDB, TMDB or IMDb episode ID. These are the IDs the plugin's metadata gives each Silo episode from Shoko's links, so an episode Shoko has not linked to TVDB, TMDB or IMDb cannot be imported, and the sync reports how many were skipped. An ID that belongs to more than one Silo episode is ambiguous and is skipped as well. This happens when Shoko links two episodes to the same TMDB episode, or when the same show is also in another Silo library the profile can see. Episodes you finished in Silo come back from Shoko on the next sync; Silo recognises them as the plays it sent and does not record them twice.

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
