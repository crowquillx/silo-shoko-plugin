// Package config decodes the single global connection entry used by the
// plugin. Silo sends every configured entry as a protobuf Struct, so this
// package owns validation and path-policy checks before any filesystem write.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const connectionKey = "connection"

// Connection is the runtime configuration for one Shoko VFS installation.
// APIKey is never included in diagnostics or serialized back to the host.
type Connection struct {
	BaseURL string
	// ImageMode decides where Silo downloads Shoko images from. Silo only
	// downloads artwork from public addresses, so a Shoko server on a private
	// network needs ImagePublicURL or ImageS3 for Silo to cache its artwork.
	ImageMode        ImageMode
	ImageBaseURL     string
	ImageS3          S3
	APIKey           string
	VFSRoot          string
	ManagedFolderMap map[int]string
	// LiveUpdates listens to Shoko's events to update the VFS as soon as
	// Shoko changes, instead of only when the reconcile task runs.
	LiveUpdates bool
	// SiloBaseURL and SiloAPIKey, both optional, let the plugin ask Silo to
	// refresh a show when its Shoko metadata or artwork changes. The key must
	// belong to an administrator.
	SiloBaseURL string
	SiloAPIKey  string
}

// Decode reads the plugin's global config entry. Unknown entries are ignored
// so a host can retain future settings while this binary is being upgraded.
func Decode(entries []*pluginv1.ConfigEntry) (Connection, error) {
	var raw map[string]any
	for _, entry := range entries {
		if entry == nil || entry.GetKey() != connectionKey || entry.GetValue() == nil {
			continue
		}
		raw = entry.GetValue().AsMap()
		break
	}
	if raw == nil {
		return Connection{}, errors.New("shokoanime: connection config is required")
	}
	return decodeRaw(raw)
}

// DecodeJSON reads the flat connection object used by the bootstrap command.
// For convenience it also accepts {"connection":{...}}, which makes it easy
// to export the same values from a Silo config backup without including the
// protobuf wrapper.
func DecodeJSON(data []byte) (Connection, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return Connection{}, fmt.Errorf("shokoanime: decode JSON config: %w", err)
	}
	if nested, ok := raw[connectionKey].(map[string]any); ok {
		raw = nested
	}
	return decodeRaw(raw)
}

func decodeRaw(raw map[string]any) (Connection, error) {
	if raw == nil {
		return Connection{}, errors.New("shokoanime: connection config is required")
	}
	cfg := Connection{
		BaseURL:      stringValue(raw, "base_url"),
		ImageMode:    ImageMode(stringValue(raw, "image_mode")),
		ImageBaseURL: stringValue(raw, "image_base_url"),
		ImageS3: S3{
			Endpoint:        stringValue(raw, "s3_endpoint"),
			Region:          stringValue(raw, "s3_region"),
			Bucket:          stringValue(raw, "s3_bucket"),
			Prefix:          stringValue(raw, "s3_prefix"),
			AccessKeyID:     stringValue(raw, "s3_access_key_id"),
			SecretAccessKey: stringValue(raw, "s3_secret_access_key"),
		},
		APIKey:           stringValue(raw, "api_key"),
		VFSRoot:          stringValue(raw, "vfs_root"),
		ManagedFolderMap: make(map[int]string),
		LiveUpdates:      boolValue(raw, "live_updates", true),
		SiloBaseURL:      strings.TrimRight(stringValue(raw, "silo_base_url"), "/"),
		SiloAPIKey:       stringValue(raw, "silo_api_key"),
	}
	if cfg.ImageMode == "" {
		cfg.ImageMode = ImageDirect
	}
	if err := parseManagedFolderMap(raw["managed_folder_map"], cfg.ManagedFolderMap); err != nil {
		return Connection{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Connection{}, err
	}
	return cfg, nil
}

func stringValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func boolValue(values map[string]any, key string, fallback bool) bool {
	switch value := values[key].(type) {
	case bool:
		return value
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return fallback
}

func parseManagedFolderMap(value any, out map[int]string) error {
	if value == nil {
		return errors.New("shokoanime: managed_folder_map is required")
	}

	var raw map[string]any
	switch typed := value.(type) {
	case string:
		if err := json.Unmarshal([]byte(typed), &raw); err != nil {
			return fmt.Errorf("shokoanime: decode managed_folder_map: %w", err)
		}
	case map[string]any:
		raw = typed
	default:
		return fmt.Errorf("shokoanime: managed_folder_map must be JSON text or an object")
	}

	if len(raw) == 0 {
		return errors.New("shokoanime: managed_folder_map must contain at least one folder")
	}
	for rawID, rawRoot := range raw {
		id, err := strconv.Atoi(strings.TrimSpace(rawID))
		if err != nil || id < 1 {
			return fmt.Errorf("shokoanime: managed_folder_map key %q is not a positive folder ID", rawID)
		}
		root, ok := rawRoot.(string)
		if !ok || strings.TrimSpace(root) == "" {
			return fmt.Errorf("shokoanime: managed_folder_map[%q] must be a non-empty path", rawID)
		}
		out[id] = strings.TrimSpace(root)
	}
	return nil
}

// ImageMode is where Silo downloads Shoko images from.
type ImageMode string

const (
	// ImageDirect gives Silo Shoko's own image URLs.
	ImageDirect ImageMode = "direct"
	// ImagePublicURL gives Silo image URLs on a public address that serves
	// Shoko's /api/v3/Image/ path.
	ImagePublicURL ImageMode = "public_url"
	// ImageS3 copies each image into an S3 bucket and gives Silo a presigned
	// link to the copy.
	ImageS3 ImageMode = "s3"
)

// S3 names the bucket for ImageS3.
type S3 struct {
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
}

func (s S3) validate() error {
	if s.Endpoint == "" || s.Bucket == "" || s.AccessKeyID == "" || s.SecretAccessKey == "" {
		return errors.New("shokoanime: the S3 image mode needs s3_endpoint, s3_bucket, s3_access_key_id and s3_secret_access_key")
	}
	if err := validateServerURL("s3_endpoint", s.Endpoint); err != nil {
		return err
	}
	if u, _ := url.Parse(s.Endpoint); u.Path != "" && u.Path != "/" {
		return errors.New("shokoanime: s3_endpoint must not have a path; put the bucket in s3_bucket")
	}
	return nil
}

// ImageURLBase returns the base URL for image URLs given to Silo in the direct
// and public URL modes.
func (c Connection) ImageURLBase() string {
	if c.ImageMode == ImagePublicURL {
		return c.ImageBaseURL
	}
	return c.BaseURL
}

func validateServerURL(key, value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("shokoanime: %s must be an http or https URL", key)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("shokoanime: %s must not contain credentials, query, or fragment", key)
	}
	return nil
}

// Validate applies the safety policy shared by the planner and reconciler.
// The output root must not overlap a read-only source root; otherwise a stale
// plugin manifest could accidentally remove source media.
func (c Connection) Validate() error {
	if c.BaseURL == "" {
		return errors.New("shokoanime: base_url is required")
	}
	if err := validateServerURL("base_url", c.BaseURL); err != nil {
		return err
	}
	switch c.ImageMode {
	case ImageDirect:
	case ImagePublicURL:
		if c.ImageBaseURL == "" {
			return errors.New("shokoanime: image_base_url is required for the public image URL mode")
		}
		if err := validateServerURL("image_base_url", c.ImageBaseURL); err != nil {
			return err
		}
	case ImageS3:
		if err := c.ImageS3.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("shokoanime: unknown image_mode %q", c.ImageMode)
	}
	if c.APIKey == "" {
		return errors.New("shokoanime: api_key is required")
	}
	if (c.SiloBaseURL == "") != (c.SiloAPIKey == "") {
		return errors.New("shokoanime: silo_base_url and silo_api_key are needed together to refresh Silo")
	}
	if c.SiloBaseURL != "" {
		if err := validateServerURL("silo_base_url", c.SiloBaseURL); err != nil {
			return err
		}
	}
	if c.VFSRoot == "" || !filepath.IsAbs(c.VFSRoot) {
		return errors.New("shokoanime: vfs_root must be an absolute path")
	}
	cRoot := filepath.Clean(c.VFSRoot)
	if cRoot == string(filepath.Separator) || cRoot == "." {
		return errors.New("shokoanime: refusing filesystem root as vfs_root")
	}
	if len(c.ManagedFolderMap) == 0 {
		return errors.New("shokoanime: at least one managed-folder mapping is required")
	}
	for id, root := range c.ManagedFolderMap {
		if id < 1 || root == "" || !filepath.IsAbs(root) {
			return fmt.Errorf("shokoanime: managed-folder %d root must be absolute", id)
		}
		root = filepath.Clean(root)
		if root == string(filepath.Separator) {
			return fmt.Errorf("shokoanime: refusing filesystem root as managed-folder %d root", id)
		}
		if pathsOverlap(cRoot, root) {
			return fmt.Errorf("shokoanime: vfs_root %q overlaps managed-folder %d root %q", cRoot, id, root)
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	under := func(child, parent string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	return under(a, b) || under(b, a)
}
