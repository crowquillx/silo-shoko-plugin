# Silo ShokoAnime VFS

> [!IMPORTANT]
> This is an unofficial, community project. It is not affiliated with, endorsed by or supported by the Shoko or Silo projects. Please do not contact the Shoko or Silo maintainers about it. For bugs, questions or feature requests, [open an issue](https://github.com/crowquillx/silo-shoko-plugin/issues) here, and pull requests are welcome.

Builds a virtual media library from Shoko's groups and series, with metadata and artwork for Silo. Media files are linked into a separate output directory.

Part of [Crowquillx Silo Plugins](https://github.com/crowquillx/crowquillx-silo-plugins#install). Supports Linux amd64, Linux arm64 and macOS arm64, and Shoko Server 5.x and 6.x, stable or daily. The plugin detects which one it is talking to, so upgrading Shoko, whether from 5.3.3 to 6.0 or from a daily build to stable, needs no configuration change.

## Install

1. Add the [Crowquillx catalog](https://github.com/crowquillx/crowquillx-silo-plugins#install) in **Administration → Plugins → Catalog** and install **ShokoAnime VFS**.
2. In **Installed**, open the plugin's **Configure** button or settings gear, then **Global Configuration**. Enter the Shoko server URL, Shoko API key and a writable **VFS output root** separate from your media folders. The VFS holds what the API key's Shoko user can see, so a user with restricted tags leaves out every show with one of them; the **Reconcile Shoko VFS** task output names the user under `shoko_user` and warns when it has restricted tags.
3. Set **Managed-folder path map (JSON)** to map Shoko's numeric managed-folder IDs to media paths visible to both Silo and the plugin. For example:

```json
{"1": "/media/anime"}
```

4. If Shoko is not on a public address, pick an [artwork source](#artwork-source) so Silo can cache Shoko's artwork. To have Silo refresh shows when their metadata or artwork changes in Shoko, also fill in the [Silo refresh settings](#refreshing-silo). Test the connection, which checks the URL, API key, every mapped folder, the S3 bucket if one is set and the Silo settings if they are set, then select **Save config**. Enable the plugin's **Shoko VFS changes** Autoscan source, set the **default poll interval** in Silo's Autoscan settings to 30 to 60 seconds, and bind the **Reconcile Shoko VFS** scheduled task to run every few hours or daily. Silo polls every source at the default interval at most, so a source's own interval can lengthen it but not shorten it.
5. Run **Reconcile Shoko VFS** to generate the library. On a large library the task returns before the crawl ends and the crawl keeps running in the background; run the task again to see its status. When it reports `complete`, create a Silo TV library rooted at the VFS output root. Silo scans a new library straight away, so a library created before the crawl finishes starts empty: scan it once from its menu after the crawl. Keep the Autoscan source enabled to scan future changes. From then on the plugin [follows Shoko's changes live](#live-updates).

The Autoscan source picks up changes from the moment it is added. If it is added after the VFS is built, or its first poll fails, its first successful poll scans the whole library once. The same happens if the VFS output root is deleted or recreated: the source notices that its place in the change journal is gone and starts over, rather than missing changes. One case is still missed: if the recreated root already holds more changes than the source had read before its next poll, the source cannot tell the journal is new and skips the earliest ones. After recreating the root, scan the library once from its menu. Polls never wait for a crawl: a crawl still in progress, including one left unfinished by a restart, continues in the background, and its changes are scanned when it finishes.

Ignored files are left out. A file marked as a variation is left out for any episode another file already covers, since it is another version of that episode. It is kept for an episode with no other file, such as the parts of one AniDB episode that is split across files and marked as variations to clear them from Shoko's release review.

A file Shoko knows in several places, such as hard links or a symbolic link and the file it leads to, gets one link in the VFS. It leads to an accessible copy in a mapped managed folder. On Shoko 6, which groups the places that are the same file, the plugin prefers the file itself to a symbolic link, whose target is a path as Shoko sees it and may not resolve where Silo reads the VFS, then a copy Shoko keeps rather than counts as a duplicate. When no copy is accessible, for example while a network share is down, the link keeps leading to where the file was, and the **Reconcile Shoko VFS** task output warns about it. The task output lists up to 100 such warnings under `diagnostics` and counts all of them by message under `diagnostic_summary`.

## Live updates

The plugin listens to Shoko Server's events, as Shokofin does, so the VFS follows Shoko within seconds instead of waiting for the reconcile task:

- When Shoko matches a new file to episodes, removes a match, moves, renames or deletes a file, or marks it ignored or a variation, the plugin reads that file again from Shoko, with its series and group, and updates only the links that change. A file whose content Shoko already knows, such as one removed and added back, keeps its stored match and gets no match event, so it is read again when Shoko finishes hashing it. A new episode is in the VFS about 10 seconds after Shoko matches it, and in Silo at the Autoscan source's next poll.
- When a series' or episode's metadata changes, for example its episode type or number, the plugin reads that series again and moves its links if their names change.
- On Shoko 6, when a group is renamed, a series moves to another group, or every group is recreated, the plugin renames or moves the show folders affected, or crawls everything after a recreation. Shoko 5 sends no group events, so there a group change reaches the VFS with the next hourly check, described below.
- Events are gathered into one update once Shoko has been quiet for 10 seconds, or after a minute at most, so a large import does not update the VFS file by file.

Shoko does not replay events it sent while the plugin was not listening, and sends none for some changes. So once an hour, and when it connects again after losing the connection, the plugin checks the VFS against Shoko: it reads Shoko's file list and every group with its series, and updates the links of whatever differs. That finds new, moved and deleted files, renamed groups and series, and seasons that were added, removed or reordered, for a fraction of a crawl's cost. A plugin that starts more than 10 minutes after the last update crawls the whole library once in the background instead. The **Reconcile Shoko VFS** task still crawls everything each time it runs, to repair anything else, so it can run every few hours or daily rather than often.

An update that fails, for example because Shoko is busy importing, is tried again after 15 seconds and then at growing intervals up to 5 minutes. After six failures in a row the plugin crawls instead. Shoko 6 can turn its APIv3 off while it runs, and then answers every request with an empty 404. The plugin does not read that as every file, series and group being deleted: updates and crawls fail with an error that names APIv3, and the VFS stays as it is until APIv3 is back. When an update moves links that already existed, as a series joining a group ahead of others renumbers the seasons after it, the plugin asks Silo to refresh that show, if Silo refreshes are set up.

Updates apply to the data of the last full crawl, which the plugin keeps in the VFS output root. An update checks only the links it changes, and syncs theme songs only in the show and season folders they are in, since on a media share where every file lookup is slow, checking all of a large library takes minutes. The reconcile task and the crawl a newly started plugin may run check every link, and remove show and season folders that an earlier update left empty. After upgrading from a version without live updates, or changing the managed-folder map, the plugin crawls once when it starts listening to have that data. A new output root is empty until the reconcile task builds it, as on first install.

To turn live updates off, switch off **Follow Shoko changes live**. The VFS then changes only when the reconcile task runs. A new plugin process waits 30 seconds before it connects, so the short-lived one Silo starts to test a connection never does.

Silo does not show plugin logs among its own, so the plugin reports how live updates are going in `.shokoanime-live-status.json` in the VFS output root, rewritten at most every 15 seconds: whether it is connected to Shoko, the events and batches it has received, how long the last update and the last hourly check took and how many links they changed, any error, and the Silo refreshes done and waiting. The **Reconcile Shoko VFS** task output reports the same under `live`, but running the task also starts a full crawl.

### Refreshing Silo

Silo fetches a show's metadata when it first scans it, and later only when asked. To have the plugin ask Silo to refresh a show when Shoko changes it, set **Silo server URL (for refreshes)** to a URL the plugin reaches Silo at, such as `http://localhost:8080`, and **Silo admin API key (for refreshes)** to an API key of a Silo administrator with no scope limits. Silo gives plugins no other way to start a refresh. The plugin then refreshes a show in Silo:

- when Shoko reports a change to the metadata of any of its series or episodes, from AniDB, TMDB or Shoko itself, or a changed link to TMDB or another provider;
- when an image of one of them is downloaded, removed, or picked as preferred, in Shoko's Web UI or by a plugin such as [Season Posters](https://github.com/crowquillx/shoko-season-posters) setting season posters;
- on Shoko 6, when its group is renamed, gains or loses a series, or has its artwork changed;
- when the show or specials poster chosen for it with Season Posters changes. That plugin sends no event for these, so its list is compared every 5 minutes.

When Shoko moves a series into another group, as it does when it merges groups after AniDB relations arrive, the old group's show folder leaves the VFS and the series' files appear in the show of the group that now holds them. Silo keeps the old show, with its episodes' TMDB, TVDB and IMDb IDs, until it deletes it, which takes at least its missing-file grace period. Silo stores each of those IDs on one episode only, so meanwhile the episodes of the show that now holds the files cannot take them, and keep Silo's `Episode N` placeholders. With the Silo refresh settings set, the plugin therefore clears those three IDs on the old show's episodes when an update applies the change, before Silo scans it. Nothing else changes: the old show, its titles and any watch state stay until Silo removes the show itself, and the edit locks no field. A show only counts as moved when its folder is gone and one of its series is now in another show, so a show whose last file was replaced or deleted is left alone. A change that only a full crawl applies, such as Shoko recreating every group, is not covered. The live status file reports these as `retired_shows` and `released_episodes`.

A Silo show refresh also refreshes its seasons, episodes and artwork. The plugin finds the show in the Silo libraries that scan the VFS output root: by the `[Shoko Group=<id>]` marker in its file names among the library's roots, or, for a show Silo added through an autoscan, which is not listed among the roots until a full library scan, by searching the catalog for its title and checking that its folder is the show's VFS folder. A show Silo has not scanned yet is skipped, since its first scan fetches its metadata.

Shoko reports one update as several events spread over minutes, such as the AniDB metadata first and then each image as it finishes downloading. So a show is refreshed once it has had no new change for a minute, or 10 minutes after its first change if changes keep coming, and at most once every 5 minutes; a change in that time waits for the next refresh rather than being dropped. Refreshes of different shows are 2 seconds apart, so Shoko's nightly AniDB update, which reports only the shows whose data actually changed, does not flood Silo's job queue.

Silo downloads an image again only when its address changes. New artwork in Shoko always has a new image ID, and so a new address: Shoko reuses an ID only to download the same remote file again.

## Library layout

The library follows your Shoko groups. Each group becomes a show and each series in it becomes a season, whatever its type, so a group with many movies or OVAs has many seasons. Seasons are numbered in the order Shoko lists the group's series. Nested groups become their own shows.

Regular episodes go in their series' season, and specials, credits, trailers and other extras go in `Season 00`. Specials keep their AniDB number on the group's first series, and later series and other episode types use higher ranges, such as `S00E1001` or `S00E401`, so that no two share a number.

## Metadata

Shows get their dates, status, genres, studios, cast and AniDB rating from all of their Shoko series, and their title aliases from the group's main series. A show's title is Shoko's name for the group, so Shoko's language settings decide it; the plugin tells Silo it is in the library's language, so Silo shows it instead of an AniDB synonym. The original title is the AniDB main title. Episodes use Shoko's episode titles, except that AniDB placeholders such as `Episode 12` or `TBA` are replaced by the linked TMDB episode's title when it has a real one. Shoko 6 drops these placeholders and makes a name up in the first language of its episode title settings, such as `第12話`; that name is replaced the same way, unless a Shoko user gave the episode its name. Genres are the tags Shoko classifies as genres. The content rating is the strictest US rating of the linked TMDB shows, or `18+` when AniDB marks a series restricted. Seasons get their series' overview and air date, and episodes their air date, still and TMDB rating.

The AniDB rating appears in Silo once an administrator turns on the AniDB rating source the plugin declares.

### Season titles

A season has its Shoko series' name, which usually starts with the show's: under Demon Slayer, `Demon Slayer: Entertainment District Arc`. Silo cuts long names off, so the part that tells seasons apart is often hidden. Switch on **Shorten season titles** to drop the show's title from the front, leaving `Entertainment District Arc`. It is off by default.

- A season keeps its full name when only a year, a number or a few characters would be left, such as `Spy x Family (2023)`, `Overlord II` or `Symphogear G`, and when it is named exactly like the show.
- Case and punctuation do not have to match, so `Armored Trooper Votoms: Pailsen Files` is shortened under `Armored Trooper VOTOMS`.
- When the show's title has a subtitle of its own, as in `Ghost in the Shell: Stand Alone Complex`, seasons that share only the first part are shortened too, so `Ghost in the Shell: Arise` becomes `Arise`.
- When fewer than half of a show's seasons start with its title, as in a group that mixes several franchises, every season keeps its full name.
- Shoko gives several seasons the same name when it has no title for them in its language. They are told apart with the rest of their AniDB main title, which is romanised: three more seasons named `Tokyo Revengers` become `Seiya Kessen Hen`, `Tenjiku Hen` and `Santen Sensou Hen`.

Silo keeps the titles it already has until a show's metadata is refreshed, so refresh the library's metadata after changing the setting.

## Posters

Each season uses its Shoko series' preferred poster. Shoko gives a group only its main series' poster, so on its own a show repeats its first season's poster. With the [Season Posters](https://github.com/crowquillx/shoko-season-posters) Shoko plugin installed, a show uses the show poster chosen there in its own show poster mode, and keeps the group poster otherwise. The plugin reads that list at most every 10 minutes. A changed poster appears once the show's metadata is refreshed in Silo, which the plugin [asks Silo to do](#refreshing-silo) when the Silo refresh settings are set. The specials season uses the specials poster chosen there, from Season Posters 0.1.9, and keeps the group poster otherwise. Shoko's group poster is the main series' poster, so without one the specials season repeats season 1's poster. A group that has sub-groups as well as series of its own is a show of those series here, and Season Posters plans a show poster for it too. Each season also gets its Shoko series' backdrop, from the next metadata refresh on.

Silo's artwork picker shows no Shoko artwork for a show or a season. Silo asks providers for that list by TMDB, TVDB or IMDb ID only, and Shoko's artwork is found by Shoko ID. The plugin answers a season's artwork request, but Silo does not send one it can use yet.

## Artwork source

Silo caches artwork in its own storage, but it only downloads artwork from public addresses. A Shoko server on a private network, such as a LAN or Tailscale address, is refused, so Silo cannot cache its posters. Clients then get Shoko's private URL and show the artwork only when they can reach Shoko themselves.

**Artwork source** in the plugin settings picks where Silo downloads Shoko artwork from:

- **Shoko Server URL** (the default) uses the Shoko Server URL. Pick it when that URL is public, or when every client can reach Shoko.
- **Public image URL** uses another URL that serves Shoko's `/api/v3/Image/<id>` path, for example a reverse proxy in front of Shoko. Shoko serves images without an API key, so let the proxy pass only that path; the rest of Shoko's API should stay private.
- **Copy to S3** copies each image into an S3-compatible bucket, such as Garage, MinIO or AWS, the first time Silo asks for it. Silo then gets a link signed for seven days. The endpoint must be public, and the key needs read and write access to the bucket. Nothing in Shoko needs a public address. Silo's own storage settings are not available to plugins, so the bucket and key are set here.

In the S3 mode, an image that cannot be copied falls back to Shoko's own URL, and the error is logged. The connection test also checks that the bucket exists.

Silo asks the plugin for an image's URL each time it downloads it, so queued downloads use a new artwork source on their next attempt. Downloads that already gave up are retried when an item is refreshed, or for the whole catalog by the **Backfill Metadata Images** task in **Administration → Tasks**.

## Intro and credits markers

Silo's online marker lookups address an episode by its show's TMDB, TVDB or IMDb ID and the episode's season and number. A Shoko VFS episode is numbered by its place in the Shoko group, which often is not the TMDB or TVDB numbering. JoJo's Stardust Crusaders Battle in Egypt is season 3 of its group, but its episodes are S02E25 onwards on TMDB. With the stock TheIntroDB provider, such an episode gets another episode's markers. Silo can also share local detections with TheIntroDB under the same wrong numbers.

The plugin adds its own marker provider, **TheIntroDB (Shoko numbering)**, which looks markers up in TheIntroDB the right way:

- A Shoko VFS episode is looked up by the TMDB episode Shoko links it to. The plugin asks Shoko where that TMDB episode sits in its show (`GET /api/v3/TMDB/Episode/<id>` in TMDB's default order), then asks TheIntroDB for that show, season and episode.
- An episode Shoko has no TMDB link for gets no markers from this provider. Its number would name some other episode.
- An episode from another library is looked up as Silo numbers it, as long as Silo says that numbering is TVDB's or TMDB's. A movie is looked up by its IDs. The provider can therefore replace the stock one for every library.
- Credits and previews are kept only when TheIntroDB timed them on a release within 5 minutes of the file's length. Intros and recaps are kept either way.
- The provider only fetches. It never submits markers to TheIntroDB.

To use it:

1. In Silo's admin settings, open **Providers → Marker providers**. For the stock **TheIntroDB** provider, turn off **Get markers from this provider** and **Allow sharing with this provider**. Silo cannot keep a provider away from one library, so it would otherwise keep looking Shoko episodes up by the group's numbering.
2. Turn on **Get markers from this provider** for **TheIntroDB (Shoko numbering)**, and leave sharing off. The provider does not share.
3. In the **Playback** settings, set the marker mode to online, or to online with local detection as a fallback. Local detection is chosen per library.

Lookups work without an account, but TheIntroDB currently allows 5,000 anonymous lookups a day, so a first sync of a large library takes several days. Silo's **Sync online markers** task resumes where the budget ran out. A key from your theintrodb.org account, entered as **TheIntroDB API key** in the plugin settings, raises the daily lookup budget. When TheIntroDB asks clients to slow down, the plugin tells Silo to pause the provider for that long.

Online markers outrank local detections in Silo. The exception is a local marker taken from the file's own chapters, which an online marker only replaces when TheIntroDB rates it at least as high.

## Theme songs

Theme songs are kept in sync between your media folders and the VFS, in both directions:

- A theme in a media folder appears in the matching VFS show or season folder. The plugin looks for `theme.<ext>`, numbered files such as `theme2.mp3`, and audio inside `theme-music`, `theme-songs`, `themes`, `theme songs` or `theme music` folders.
- A theme written into the VFS, for example by [Theme Songs](https://github.com/crowquillx/silo-theme-songs), is added to the media folder, inside the theme folder it already uses or a new `theme-music` folder.
- A theme you delete from a media folder is removed from the VFS, but only while that media folder is reachable.

Nothing in a media folder is ever overwritten or deleted, and a name clash is reported instead. Silo only plays themes that are real files, so they are hardlinked into the VFS, or copied when the VFS and your media are on different filesystems. Media folders need to be writable for themes to be added to them.

A media folder is only paired with a show or season when every Shoko file in it belongs to that show or series, so shared download folders and managed-folder roots are left alone. Themes sync after each reconcile. Autoscan polls, at most every 5 minutes and in the background, sync the show and season folders that something was written into, which is how a theme Theme Songs downloads reaches the media folder. A theme added in a media folder appears in the VFS after the next reconcile, since reading every media folder on each poll takes minutes on a slow share.

Each link name carries the series' AniDB anime ID as `[AniDB Anime=<id>]`. Theme Songs reads it to find each season's anime, since Shoko shows have no TVDB or TMDB IDs for it to map.

A link that cannot be created is skipped and listed under `failures` in the task output, and the rest of the library is still built.

## Watched status

The plugin can mark an episode watched in Shoko when you finish playing it in Silo, as Shokofin does for Jellyfin, export the episodes you already watched in Silo to Shoko, and import the episodes you watched in Shoko into Silo. All are off until a profile connects the Shoko watch provider, and importing also needs its own switch. Progress and manual watched marks without a play stay in Silo.

1. In Shoko's Web UI, open **Settings → API Keys** and generate a key while signed in as the Shoko user whose watched status you want updated. Each Silo profile can use its own Shoko user.
2. In the Silo profile, open **Settings → Watch Providers → Shoko → Connect**. Enter the Shoko API key, then a Silo server URL the plugin can reach (such as `http://localhost:8080` when the plugin runs on the Silo host), a Silo API key for the account that owns the profile, and the profile's name as Silo shows it. The plugin looks the name up on that account and keeps the profile's ID with the connection; the ID itself also works, and if two profiles share a name the error lists their IDs.

Silo's watch events name only the Silo item, not the Shoko episode, so on each finished play the plugin reads the item from Silo as that profile and takes the Shoko episode from its VFS file name. Items outside the Shoko VFS are skipped. The plugin uses the Shoko Server URL from its global configuration and marks the episode with `POST /api/v3/Episode/<id>/Watched/true`, which also marks the episode's files watched. Each marked episode is logged.

A play counts as finished when Silo reports it completed. Some players stop at position zero after finishing; such a stop counts when Silo has the item marked played for the profile. If Shoko or Silo cannot be reached when a play finishes, the failure is logged and shown on the connection. Silo retries playback stops from Jellyfin-compatible clients, but not from its own apps; the next history export sends such a play.

### Exporting to Shoko

Silo's **Export watched** switch for the connection, on for a new connection, sends the profile's watch history to Shoko. Each sync sends up to 100 plays that Shoko has not received yet, so a long history takes several syncs; failed plays are retried on later syncs. The plugin finds each play's Shoko episode from the item's VFS file name, as for a finished play. Shoko takes no date when it marks an episode, so an exported episode is recorded as watched on the day of the sync; an episode Shoko already has watched is left alone and keeps its date. Plays of items outside the Shoko VFS are counted as not found rather than sent.

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
