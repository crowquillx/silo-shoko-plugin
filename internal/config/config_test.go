package config

import (
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestDecodeConnection(t *testing.T) {
	value, err := structpb.NewStruct(map[string]any{
		"base_url":           "http://shoko:8111",
		"api_key":            "secret",
		"vfs_root":           "/srv/silo/shoko-vfs",
		"managed_folder_map": `{"1":"/media/anime"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Decode([]*pluginv1.ConfigEntry{{Key: connectionKey, Value: value}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ManagedFolderMap[1] != "/media/anime" {
		t.Fatalf("managed folder map = %#v", cfg.ManagedFolderMap)
	}
}

func TestDecodeRejectsOverlappingRoots(t *testing.T) {
	value, err := structpb.NewStruct(map[string]any{
		"base_url":           "http://shoko:8111",
		"api_key":            "secret",
		"vfs_root":           "/media/anime/vfs",
		"managed_folder_map": `{"1":"/media/anime"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode([]*pluginv1.ConfigEntry{{Key: connectionKey, Value: value}}); err == nil {
		t.Fatal("overlapping roots were accepted")
	}
}
