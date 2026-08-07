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
	BaseURL          string
	APIKey           string
	VFSRoot          string
	ManagedFolderMap map[int]string
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
		BaseURL:          stringValue(raw, "base_url"),
		APIKey:           stringValue(raw, "api_key"),
		VFSRoot:          stringValue(raw, "vfs_root"),
		ManagedFolderMap: make(map[int]string),
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

// Validate applies the safety policy shared by the planner and reconciler.
// The output root must not overlap a read-only source root; otherwise a stale
// plugin manifest could accidentally remove source media.
func (c Connection) Validate() error {
	if c.BaseURL == "" {
		return errors.New("shokoanime: base_url is required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("shokoanime: base_url must be an http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("shokoanime: base_url must not contain credentials, query, or fragment")
	}
	if c.APIKey == "" {
		return errors.New("shokoanime: api_key is required")
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
