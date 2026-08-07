package main

import (
	"encoding/json"
	"fmt"
	"os"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: catalog-manifest <manifest-file> <version>")
		os.Exit(2)
	}
	encoded, err := encodeCatalogManifest(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func encodeCatalogManifest(path, version string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plugin manifest: %w", err)
	}
	manifest, err := publicmanifest.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("load plugin manifest: %w", err)
	}
	manifest.Version = version
	manifest.Checksum = ""

	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode catalog manifest: %w", err)
	}
	encoded = append(encoded, '\n')

	var roundTrip pluginv1.PluginManifest
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		return nil, fmt.Errorf("verify catalog manifest encoding: %w", err)
	}
	if roundTrip.GetPluginId() != manifest.GetPluginId() || roundTrip.GetVersion() != version {
		return nil, fmt.Errorf("catalog manifest did not preserve plugin identity")
	}
	return encoded, nil
}
