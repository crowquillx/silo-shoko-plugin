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

func TestDecodePublicImageURL(t *testing.T) {
	decode := func(imageURL string) (Connection, error) {
		value, err := structpb.NewStruct(map[string]any{
			"base_url":           "http://shoko:8111",
			"image_mode":         "public_url",
			"image_base_url":     imageURL,
			"api_key":            "secret",
			"vfs_root":           "/srv/silo/shoko-vfs",
			"managed_folder_map": `{"1":"/media/anime"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		return Decode([]*pluginv1.ConfigEntry{{Key: connectionKey, Value: value}})
	}

	if _, err := decode(""); err == nil {
		t.Fatal("the public URL mode was accepted without a URL")
	}
	cfg, err := decode(" https://shoko-images.example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageURLBase() != "https://shoko-images.example.com" {
		t.Fatalf("images use %q", cfg.ImageURLBase())
	}
	for _, bad := range []string{"shoko-images.example.com", "ftp://shoko", "https://user:pass@shoko", "https://shoko?x=1"} {
		if _, err := decode(bad); err == nil {
			t.Fatalf("image_base_url %q was accepted", bad)
		}
	}
}

func TestDecodeImageModes(t *testing.T) {
	decode := func(extra map[string]any) (Connection, error) {
		raw := map[string]any{
			"base_url":           "http://shoko:8111",
			"api_key":            "secret",
			"vfs_root":           "/srv/silo/shoko-vfs",
			"managed_folder_map": `{"1":"/media/anime"}`,
		}
		for key, value := range extra {
			raw[key] = value
		}
		value, err := structpb.NewStruct(raw)
		if err != nil {
			t.Fatal(err)
		}
		return Decode([]*pluginv1.ConfigEntry{{Key: connectionKey, Value: value}})
	}

	cfg, err := decode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageMode != ImageDirect || cfg.ImageURLBase() != "http://shoko:8111" {
		t.Fatalf("default mode = %q, images use %q", cfg.ImageMode, cfg.ImageURLBase())
	}
	s3 := map[string]any{
		"image_mode":           "s3",
		"s3_endpoint":          "https://s3.example.com",
		"s3_region":            "garage",
		"s3_bucket":            "shoko-images",
		"s3_access_key_id":     "key",
		"s3_secret_access_key": "secret",
	}
	cfg, err = decode(s3)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageMode != ImageS3 || cfg.ImageS3.Bucket != "shoko-images" || cfg.ImageS3.Region != "garage" {
		t.Fatalf("S3 config = %#v", cfg.ImageS3)
	}
	for name, change := range map[string]map[string]any{
		"missing bucket":  {"s3_bucket": ""},
		"missing secret":  {"s3_secret_access_key": ""},
		"endpoint path":   {"s3_endpoint": "https://s3.example.com/shoko-images"},
		"endpoint scheme": {"s3_endpoint": "s3.example.com"},
		"unknown mode":    {"image_mode": "ftp"},
	} {
		broken := map[string]any{}
		for key, value := range s3 {
			broken[key] = value
		}
		for key, value := range change {
			broken[key] = value
		}
		if _, err := decode(broken); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
