#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <version-or-tag> <asset-directory> <output-file>" >&2
  exit 2
fi

version="${1#v}"
asset_dir="$2"
output="$3"
repository="${GITHUB_REPOSITORY:-crowquillx/silo-shoko-plugin}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid plugin version: $version" >&2
  exit 1
fi

for binary in plugin-linux-amd64 plugin-linux-arm64 plugin-darwin-arm64; do
  if [[ ! -f "$asset_dir/$binary" ]]; then
    echo "missing release binary: $asset_dir/$binary" >&2
    exit 1
  fi
done

sha256() {
  sha256sum "$asset_dir/$1" | cut -d ' ' -f 1
}

linux_amd64_sha="$(sha256 plugin-linux-amd64)"
linux_arm64_sha="$(sha256 plugin-linux-arm64)"
darwin_arm64_sha="$(sha256 plugin-darwin-arm64)"
repository_url="https://github.com/$repository"
release_base="$repository_url/releases/download/v$version"

mkdir -p "$(dirname "$output")"
temporary="$output.tmp"
catalog_manifest="$output.manifest.tmp"
trap 'rm -f "$temporary" "$catalog_manifest"' EXIT

(
  cd "$root"
  CGO_ENABLED=0 go run ./cmd/catalog-manifest "$root/manifest.json" "$version"
) > "$catalog_manifest"

jq -n \
  --slurpfile manifest "$catalog_manifest" \
  --arg repository_url "$repository_url" \
  --arg release_base "$release_base" \
  --arg linux_amd64_sha "$linux_amd64_sha" \
  --arg linux_arm64_sha "$linux_arm64_sha" \
  --arg darwin_arm64_sha "$darwin_arm64_sha" \
  '{
    plugins: [
      {
        manifest: $manifest[0],
        repo_url: $repository_url,
        binaries: {
          "linux/amd64": {
            url: ($release_base + "/plugin-linux-amd64"),
            checksum: $linux_amd64_sha
          },
          "linux/arm64": {
            url: ($release_base + "/plugin-linux-arm64"),
            checksum: $linux_arm64_sha
          },
          "darwin/arm64": {
            url: ($release_base + "/plugin-darwin-arm64"),
            checksum: $darwin_arm64_sha
          }
        }
      }
    ]
  }' > "$temporary"

jq -e --arg version "$version" '
  (.plugins | length) == 1 and
  .plugins[0].manifest.version == $version and
  (.plugins[0].binaries | keys | sort) == ["darwin/arm64", "linux/amd64", "linux/arm64"] and
  all(.plugins[0].binaries[]; .url != "" and (.checksum | test("^[0-9a-f]{64}$")))
' "$temporary" >/dev/null

mv "$temporary" "$output"
rm -f "$catalog_manifest"
trap - EXIT
