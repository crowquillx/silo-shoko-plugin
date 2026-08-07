# ShokoAnime integration for Silo Server: viability assessment

[Open the formatted HTML report](viability-report.html).

## Verdict

A useful Shoko plugin for Silo is viable, but **a metadata-only plugin is not sufficient** for libraries whose physical layout does not already match Silo's TV/movie naming rules. Layout support is the deciding architectural constraint.

Recommended direction:

1. **Near term: build a Shokofin-style symlink VFS plus a normal Silo metadata plugin.** This can be implemented without changing Silo and is the fastest route to broad layout support. Silo's scanner already follows file symlinks while preserving their logical VFS paths.
2. **Long term: add a declarative `catalog_source.v1`/`library_source.v1` capability to Silo.** The Shoko plugin would submit logical hierarchy and file bindings; Silo would validate paths and remain the sole catalog writer. This removes the VFS operational burden.
3. Build the Shoko-to-library **topology planner independently of either renderer**. The same tested plan should render to a VFS now and to a native Silo source later.

The near-term VFS route is a real implementation option, not merely a workaround. It is also not currently a first-class SDK contract: the plugin binary would write ordinary filesystem symlinks using the permissions of the Silo process. That needs explicit configuration, path confinement, and careful reconciliation.

## Why metadata alone fails the layout requirement

Silo currently owns discovery and structure:

- Its scanner walks host-visible filesystem roots.
- Naming determines movie/show grouping and file-to-episode coordinates before metadata enrichment.
- TV episode matching is centered on `SxxEyy` or air-date filenames.
- A metadata provider can enrich an existing series, season, or episode, but cannot create a different hierarchy, re-parent files, or bind one file to arbitrary logical episodes.

The SDK confirms that boundary:

- [`metadata_provider.proto`](research/sources/silo-plugin-sdk/proto/silo/plugin/v1/metadata_provider.proto) gives `GetMetadata` a `file_path`, but `SearchMetadataRequest` has no path and the season/episode requests contain provider IDs and numeric coordinates only.
- [`scan_source.proto`](research/sources/silo-plugin-sdk/proto/silo/plugin/v1/scan_source.proto) only reports changed source paths; Silo rewrites, validates, and scans those paths itself.
- [`runtime_host.proto`](research/sources/silo-plugin-sdk/proto/silo/plugin/v1/runtime_host.proto) exposes a public-safe, path-free catalog view and no catalog mutation API.

Silo's parser currently recognizes `SxxEyy` and only TMDB/TVDB/IMDb filename tags ([`filename.go`](research/sources/silo-server/internal/naming/filename.go)). It has no AniDB/Shoko grouping token and no absolute-number episode linker in this path.

Consequently, metadata-only operation is acceptable as an optional **normalized-layout mode**, but cannot be the primary design when arbitrary Shoko layouts are a requirement.

## What Shokofin's VFS actually provides

Shokofin is not merely renaming folders for Jellyfin. Its VFS materializes a selectable logical library from Shoko's graph:

- `AniDB_Anime`: an AniDB/Shoko series-oriented view.
- `Shoko_Groups`: related Shoko series can become one show with logical seasons.
- `TMDB_SeriesAndMovies`: TMDB-oriented shows, seasons, episodes, and movies.
- Configurable season ordering: Shoko/default, release date, chronological, and simplified chronological.
- Configurable specials placement and optional season merging, including forward/backward/main-story and explicit merge groups.
- Movie/show filtering, extras categories, external subtitles/audio, trickplay sidecars, versions, split files, and files linked to multiple episodes.
- Stable Shoko IDs encoded in generated paths so its resolver can recover exact identity.
- Full reconciliation plus SignalR-driven file/metadata updates.

Primary references are [`SeriesStructureType.cs`](research/sources/Shokofin/Shokofin/Configuration/Enums/SeriesStructureType.cs), [`Ordering.cs`](research/sources/Shokofin/Shokofin/Utils/Ordering.cs), [`SeasonMergingBehavior.cs`](research/sources/Shokofin/Shokofin/Configuration/Enums/SeasonMergingBehavior.cs), and [`VirtualFileSystemService.cs`](research/sources/Shokofin/Shokofin/Resolvers/VirtualFileSystemService.cs).

The essential, media-server-independent behavior is:

1. Normalize arbitrary physical locations into a predictable logical hierarchy.
2. Compile Shoko groups/relations and ordering policy into show/season/episode coordinates.
3. Materialize multiple logical bindings for a physical file when necessary.
4. Put sidecars and extras beside the logical media item.
5. carry stable source identity independently of display titles.

Jellyfin-specific resolver hooks, action filters, creation-time workarounds, and library APIs should not be ported unless Silo proves to need equivalent behavior.

## Silo behavior that makes a VFS viable today

The scanner's [`walkLogicalTree`](research/sources/silo-server/internal/scanner/scanner.go) follows symlinks and appends the **logical link path**, not the resolved target, for leaf files. Its physical-directory visited set prevents directory loops, but does not deduplicate leaf symlinks by target. This means several VFS entries can point to one physical file and still be scanned as distinct logical paths.

There is also a symlink-root test in [`scanner_test.go`](research/sources/silo-server/internal/scanner/scanner_test.go) (`TestCollectLogicalFilePaths_PreservesLogicalSymlinkRootPaths`). The implementation was reviewed, but the test suite could not be executed in this environment because the Go toolchain is unavailable.

Other relevant behavior:

- `SxxEyy-Eyy` is detected as `presentation_kind=multi_episode` ([`variants.go`](research/sources/silo-server/internal/naming/variants.go)).
- Each `media_files` row still has one nullable `episode_id` ([`001_schema.sql`](research/sources/silo-server/migrations/sql/001_schema.sql)); range metadata does not create a many-to-many relationship.
- Multiple files for one episode are a normal Silo use case and fit versions/parts well.
- Sidecar subtitles are found by logical basename, so linked sidecars should work.
- Extras use conventional directory and suffix classification.
- Silo permanently excludes `.strm` and equivalent remote stream shortcuts. Shoko streams must not be used to evade that rule; playback must resolve to operator-controlled, host-visible files ([`non-goals.md`](research/sources/silo-server/docs/non-goals.md)).

### Important VFS caveats

- **Leaf links, not shared directory aliases:** multiple directory symlinks to the same physical directory are deliberately deduplicated. Generate one symlink for each logical media/sidecar leaf.
- **Multi-episode files:** generate one logical leaf for every required episode binding. Do not rely only on Silo's range annotation. All such entries will currently play the same underlying file; segment-offset playback is not represented and needs a dedicated test with Shoko percentage xrefs.
- **Path identity:** changing generated paths can look like remove/add churn. Keep names deterministic and update individual links atomically.
- **Containers:** Shoko paths, VFS paths, and link targets may use three different namespaces. Map Shoko managed-folder IDs plus relative paths to Silo-visible roots; do not rely on suffix matching alone.
- **Permissions:** the plugin process is launched as a normal child binary and no SDK filesystem-write capability exists. Link creation succeeds only where the Silo OS/container identity can write and resolve targets.
- **No-symlink environments:** copies are unacceptable and hard links are not portable across filesystems. A native catalog source is the required fallback.

## Shoko API suitability

The local Shoko server exposes a large authenticated V3 API and the required source data is present:

- Managed folders and paginated files: `/ManagedFolder`, `/ManagedFolder/{folderID}/File`
- Files, locations, and path lookup: `/File`, `/File/{fileID}`, `/File/{fileID}/Location`, `/File/PathEndsWith`
- Explicit file-to-episode links: `/File/{fileID}/Episode`, plus link APIs
- Series hierarchy: `/Series`, `/Series/{seriesID}/Episode`, relations, groups, images, cast, and TMDB cross-references
- Groups and nested group/series membership: `/Group` and related routes
- Episode/file/series/group user data and watched/scrobble routes
- ED2K/CRC32/MD5/SHA1 lookup where path identity is insufficient

The API uses an `apikey` header. The checked local instance reported `6.0.0-dev.390`; integration code should use capability detection and tolerate API evolution rather than depending on that development build.

SignalR is not described by the OpenAPI document, but Shokofin demonstrates the aggregate hub at `/signalr/aggregate?feeds=shoko,metadata,file,release` and handles metadata, file deletion/relocation, and release events in [`SignalRConnectionManager.cs`](research/sources/Shokofin/Shokofin/SignalR/SignalRConnectionManager.cs). Incremental events should be an optimization over periodic authoritative reconciliation, not the only source of truth.

Local API artifacts are documented in [`research/README.md`](research/README.md). No authenticated library data was retrieved during this assessment.

## Options compared

| Option | Arbitrary physical layouts | Shoko groups/order | Multi-binding files | Operational fit | Verdict |
|---|---:|---:|---:|---:|---|
| Metadata provider over raw files | No | No | No | Excellent | Optional normalized-layout mode only |
| Symlink VFS + metadata provider | Yes | Yes | Yes, by duplicate logical leaves | Moderate | **Recommended MVP** |
| Sidecar using Silo admin/catalog APIs | Potentially | Potentially | Limited by schema | Poor | Reject |
| New native catalog/library source | Yes | Yes | Requires schema/API work | Best long-term | **Recommended target** |

### Why not use the catalog-seed importer?

Silo has admin-only catalog export/import routes under `/api/v1/admin/catalog` and a versioned gzip bundle implementation in [`internal/catalogseed`](research/sources/silo-server/internal/catalogseed). It is designed for migration/restore, not continuous ownership by an external source. It has no durable source ownership, delta cursor, safe prune contract, or plugin-scoped authorization, and its payload follows internal catalog schema closely. Using it would require admin credentials and couple the integration to migrations. It is useful as evidence that host-side transactional imports are possible, not as the integration protocol.

## Layout support matrix

| Requirement | Metadata only | VFS MVP | Native source target |
|---|---:|---:|---:|
| Flat, hashed, or otherwise arbitrary source folders | No | Yes | Yes |
| AniDB anime as independent shows | Only if already Silo-shaped | Yes | Yes |
| Shoko group as show; member anime as seasons | No | Yes | Yes |
| TMDB show/season/episode layout | Partial | Yes | Yes |
| Anime movies separated from shows | Partial | Yes | Yes |
| Mixed movie/show output | Heuristic only | Yes | Yes |
| Relation-based season ordering | No | Yes | Yes |
| Season merge policies | No | Yes | Yes |
| Specials placement and alternate episodes | Filename-dependent | Yes | Yes |
| Multiple releases/versions for one episode | Yes when grouped correctly | Yes | Yes |
| Split episode across several files | Partial/native parts | Yes | Yes |
| One physical file linked to several episodes/series | No | Yes, duplicate links | Needs many-to-many binding |
| External subtitles/audio and extras | Only if physically adjacent | Yes, linked sidecars | Needs explicit bindings or host discovery |
| Multiple Shoko locations and path failover | Manual | Planner chooses mapped reachable path | Native binding selection |
| Environment without symlink support | N/A | No | Yes |

## Recommended architecture

### 1. A renderer-independent topology planner

Treat layout as a first-class compiler, not scattered filename logic.

**Input model**

- Managed folders and mapped locations
- Shoko files and every file↔episode xref
- Shoko series, episodes, groups, relations, and relevant TMDB crossrefs
- Sidecars, release/version attributes, user-selected filters

**Policy**

- Structure mode: AniDB, Shoko Groups, or TMDB
- Movie handling: show specials, separate movie library, or mixed output
- Season ordering and merging rules
- Specials/alternate episode placement
- Preferred file-location mapping and release/version naming
- Included Shoko filters/managed folders

**Output `LibraryPlan`**

- Stable source keys for shows, seasons, episodes, movies, extras, and files
- Display metadata and provider IDs
- Explicit parent relationships and S/E coordinates
- Repeated file bindings, including one file bound to several logical episodes
- Sidecar/extra bindings
- Deterministic desired logical paths for the VFS renderer

The planner should be pure and fixture-testable. Layout policy changes then produce a previewable plan diff. A VFS renderer can consume it now; a future native Silo source adapter can consume the same plan without redesigning layout semantics.

### 2. Plugin capabilities for the VFS MVP

- **`metadata_provider.v1`**: resolve encoded Shoko IDs; return series/movie/season/episode metadata, provider crossrefs, images, cast, and aliases.
- **`image_resolver.v1`**: resolve authenticated or plugin-owned Shoko image URLs if direct URLs are unsuitable.
- **`scheduled_task.v1`**: full plan/VFS reconciliation, diagnostics, and cache refresh.
- **`scan_source.v1`**: report changed **VFS logical paths** after reconciliation so Silo performs targeted scans. A periodic full Silo scan remains a recovery path.
- **SignalR client inside the plugin process**: coalesce upstream events, update the plan/VFS, then expose changed logical paths through `scan_source`.
- **`watch_sync_provider.v1` later**: Silo already has host-owned API-key credentials, outbox, retry, and reconciliation semantics. Shoko user mapping and multi-episode progress semantics need separate design.
- **`http_routes.v1`**: connection test, path-map validation, plan preview, unresolved path/xref diagnostics, and manual reconcile.

### 3. VFS filesystem rules

- Use a stable configured root, one subroot per Silo library/output mode.
- Create only relative symlinks where practical; validate every resolved target against configured allowed source roots.
- Build a desired manifest, diff it against a plugin-owned manifest, create replacement links under temporary names, then atomically rename each leaf.
- Delete only paths listed in the prior plugin manifest. Never recursively clean an arbitrary operator-selected directory.
- Write the new manifest only after successful link operations; retain retryable failures.
- Encode stable IDs in a deterministic token and sanitize all display-name components.
- Link subtitles/external audio/extras with collision-resistant deterministic names.
- Debounce SignalR bursts and perform periodic full reconciliation to repair missed events.
- Offer a dry-run/preview before the first generation and after policy changes.

### 4. Identity handoff is an MVP gate

Shokofin filenames carry Shoko IDs, but Silo strips only known TMDB/TVDB/IMDb tags. A plugin-only prototype can deliberately preserve a Shoko token in the parsed query, extract it in `Search`, return an exact tagged search title, then return the clean title from `GetMetadata`. This needs an end-to-end match test before it is trusted.

The cleaner small Silo addition would be one of:

- a general provider-tag parser that puts `{shoko-123}`/`{anidb-456}` into `SearchMetadataRequest.provider_ids`, or
- a stable source hint/logical path on `SearchMetadataRequest`.

That is much smaller than a native import capability and would make the VFS integration robust without making metadata responsible for hierarchy.

## Recommended native Silo addition

A future `catalog_source.v1` (name subject to Silo conventions) should follow Silo's existing pattern: **plugin translates; host validates and writes**.

The plugin should be able to page an authoritative snapshot or ordered deltas containing:

- source namespace and stable source item/file keys;
- item kind, parent source key, order, season/episode/absolute numbers;
- provider IDs and basic metadata;
- host-namespace paths after host-owned rewrite rules;
- repeated item bindings per physical file;
- version/part attributes and optional sidecar/extra descriptors;
- a cursor/snapshot token and explicit finalization.

The host must own:

- library/root authorization and path rewrite/confinement;
- stat/probe and playback-path validation;
- transactions, idempotent upserts, and source-scoped deletion only after a complete snapshot;
- content-ID selection, provider-ID promotion, merge/conflict policy, and watch-state preservation;
- missing-root protection and observability.

Required data-model work:

1. Replace or supplement `media_files.episode_id` with a many-to-many file↔logical-item binding table. Preserve a primary binding if current APIs need one.
2. Record source ownership/provenance independently of provider metadata.
3. Decide whether AniDB/Shoko may become deterministic `content_id` anchors. This is a migration-sensitive policy choice; crosswalk to existing TVDB/TMDB anchors where available in the meantime.
4. Add source-aware collection/group import only after core hierarchy is reliable. Current RuntimeHost APIs cannot create collections or playlists.

A native source should still consume on-disk files. It must not call Shoko's stream endpoint as a `.strm` equivalent.

## Suggested implementation phases and gates

### Phase 0 — local vertical spike

1. Implement a read-only Shoko V3 client and normalized graph fixtures.
2. Implement one planner slice for each structure mode.
3. Render a small VFS with leaf symlinks and stable ID tokens.
4. Scan a synthetic Silo library and verify:
   - logical symlink paths survive probing/playback;
   - ID-token matching works end to end;
   - one target linked as two episodes produces two usable catalog entries;
   - subtitles, extras, versions, multipart files, rename, and deletion reconcile correctly.
5. Measure a representative full plan/reconcile and an event burst.

Do not commit to the full plugin until the ID-token and duplicate-link tests pass.

### Phase 1 — layout-first MVP

- AniDB, Shoko Group, and TMDB plan modes
- movie/show filtering
- path maps keyed by Shoko managed-folder ID
- metadata/images
- VFS preview, manifest diff, scheduled full reconcile
- targeted Silo scans via `scan_source`
- clear diagnostics for unmapped/unreachable files

### Phase 2 — operational parity

- SignalR incremental updates with full-reconcile fallback
- sidecars/extras and release/version naming
- watch-state synchronization after user/profile semantics are settled
- migration tests across Shoko V3 and Silo SDK versions

### Phase 3 — native source prototype

Add the SDK/server capability locally, retain the planner, replace only the renderer, and test migration from VFS identity to native bindings without losing watch history.

## Open questions and risks

- Exact Silo playback/progress behavior for several logical episodes backed by one complete file, especially when Shoko xrefs describe time percentages.
- Whether Silo maintainers want AniDB/Shoko in the frozen deterministic identity scheme; no contact was made as requested.
- Shoko SignalR compatibility/stability outside the Shokofin compatibility path.
- Silo's desired trust model for plugin filesystem writes. The current launcher starts a normal child process without establishing a filesystem sandbox, but that is not an explicit VFS contract.
- Silo profile ↔ Shoko user mapping and whether one Shoko API token can represent all desired users.
- Scale characteristics on the target library and the cost of generating/linking every sidecar.
- Silo is early WIP and the checked local Shoko server is a development build, so pinned compatibility tests are essential.

## Overall recommendation

Proceed with a **layout-first local spike**. Use a planner/VFS architecture rather than a metadata-only plugin, because it is the only current path that satisfies the layout requirement across AniDB, Shoko Group, and TMDB views. Treat the VFS as the delivery mechanism, not the domain model.

In parallel, specify a host-owned declarative catalog-source capability and many-to-many file binding for Silo. If that lands later, the topology planner and Shoko client remain valid and only the output renderer changes.

This assessment made no changes to either upstream project and opened no issues or pull requests.
