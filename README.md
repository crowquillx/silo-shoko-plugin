# Silo ShokoAnime VFS

Builds a virtual media library from Shoko's groups and series, with metadata and artwork for Silo. Media files are linked into a separate output directory.

Part of [Crowquillx Silo Plugins](https://github.com/crowquillx/crowquillx-silo-plugins#install). Supports Linux amd64, Linux arm64 and macOS arm64.

## Install

1. Add the [Crowquillx catalog](https://github.com/crowquillx/crowquillx-silo-plugins#install) in **Administration → Plugins → Catalog** and install **ShokoAnime VFS**.
2. In **Installed**, open the plugin's **Configure** button or settings gear, then **Global Configuration**. Enter the Shoko server URL, Shoko API key and a writable **VFS output root** separate from your media folders.
3. Set **Managed-folder path map (JSON)** to map Shoko's numeric managed-folder IDs to media paths visible to both Silo and the plugin. For example:

```json
{"1": "/media/anime"}
```

4. Select **Save config**. Enable the plugin's **Shoko VFS changes** Autoscan source and bind the **Reconcile Shoko VFS** scheduled task.
5. Run **Reconcile Shoko VFS** to generate the library, then create a Silo TV library rooted at the VFS output root. Keep the Autoscan source enabled to scan future changes.

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
