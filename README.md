# Silo ShokoAnime VFS plugin

Shoko remains the source of truth while Silo sees deterministic, group-aware
logical paths backed by plugin-owned leaf symlinks. The plugin provides typed
Shoko metadata, artwork resolution, durable reconciliation, and targeted Silo
scan notifications. It does not create `.strm` files, expose remote Shoko
streams, or write to Shoko.

## Install

Add the shared Crowquillx catalog to Silo:

```text
https://raw.githubusercontent.com/crowquillx/crowquillx-silo-plugins/main/repository.json
```

Install **ShokoAnime VFS**, configure the Shoko URL, API key, VFS output root,
and numeric managed-folder path map, then create a Silo TV library rooted at
the generated VFS.

## Development

The module targets Go 1.26 and `silo-plugin-sdk` v0.12.0. From a Go-enabled
environment:

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -o plugin .
./plugin manifest | jq
```

Tagged `v*` releases run the full test and vet suite, cross-compile Linux
amd64, Linux arm64, and Apple silicon binaries, publish checksums, and attach
the `repository.json` consumed by the shared catalog.

The Shoko API key is sent only in the `apikey` request header. VFS mutation is
confined to the configured output root and is driven by a plugin-owned manifest.

The binary advertises metadata, image-resolver, scheduled-reconcile, and
`scan_source.v1` capabilities. A real reconcile is a durable crawl: its cursor
is stored in
`.shokoanime-crawl-state.json` under the configured output root, so a Silo task
that reaches its short host deadline can resume on the next task or in the
longer `scan_source.v1` polling call. The scheduled task uses a bounded step;
`PollChanges` completes any pending crawl before replaying committed paths.
Successful reconciles append changed logical VFS paths to
`.shokoanime-change-journal.jsonl`; Silo polls that journal and queues targeted
scans. Dry runs never create crawl state, links, or journal entries. On the
first poll Silo starts at the current journal tail, while the opaque marker
returned by later polls supports replay after a plugin restart.
Grouped TV series use their direct Shoko group as the Silo series root. Member
TV series are ordered by complete AniDB air date and rendered as seasons;
special/non-TV members use season zero, while movies retain the Movies layout.
Metadata identity is scope-specific (`shoko_group`, `shoko_series`,
`shoko_episode`, and `shoko_file`) so numerically overlapping Shoko IDs cannot
merge unrelated catalog rows. Artwork is stored as opaque `shoko://image/...`
paths and resolved to Shoko's read-only image endpoint without exposing the API
key.

The managed-folder map is keyed by Shoko's numeric managed-folder ID, not by a
path suffix. Both the plugin and Silo must see each mapped source root at the
same path, and the VFS output root must be a separate mount/path from every
source root.

The group-aware identity layout is a clean cutover from earlier development
builds. Reconcile it into a new VFS root and scan a fresh Silo library; an
existing catalog can retain provider associations and artwork paths created by
the old series-per-root layout.

The scheduled task starts or advances a full authoritative reconcile. Silo's
autoscan source should be enabled for the installed Shoko VFS capability; its
poller invokes `scan_source.v1` with the capability ID and opaque marker,
finishes any pending crawl, and scans only the changed VFS paths. A periodic
full library scan remains a useful recovery operation.

For a first production crawl, the binary also has a one-shot bootstrap path.
The JSON file contains the same four connection values as the Silo form (the
API key is read only for Shoko GET requests):

```json
{
  "base_url": "http://shoko:8111",
  "api_key": "REDACTED",
  "vfs_root": "/srv/silo/shoko-vfs",
  "managed_folder_map": {"1": "/srv/media/anime"}
}
```

Run it from the host namespace shared by Shoko media and Silo:

```sh
./silo-plugin-shokoanime bootstrap --config shokoanime-connection.json
```

Bootstrap performs no Shoko write operation. It writes only the configured VFS
root and can be interrupted and rerun; the same crawl state and journal rules
apply.
