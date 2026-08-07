// Package vfs renders topology plans as plugin-owned leaf symlinks. It never
// follows a manifest path blindly: every delete is preceded by an ownership
// and symlink-target check, and every target is confined to an allowlisted
// source root.
package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crowquillx/silo-shoko-plugin/internal/topology"
)

const manifestVersion = 1

var (
	ErrUnsafeRoot   = errors.New("vfs: unsafe output root")
	ErrOutsideRoot  = errors.New("vfs: path outside configured root")
	ErrNotOwned     = errors.New("vfs: path is not owned by this manifest")
	ErrPathConflict = errors.New("vfs: logical path conflicts with an existing entry")
)

type Config struct {
	Root               string
	AllowedSourceRoots []string
}

type Manifest struct {
	Version   int                     `json:"version"`
	UpdatedAt time.Time               `json:"updated_at"`
	Entries   map[string]ManifestItem `json:"entries"`
}

type ManifestItem struct {
	LogicalPath string `json:"logical_path"`
	TargetPath  string `json:"target_path"`
}

type Action struct {
	Kind        string `json:"kind"`
	LogicalPath string `json:"logical_path"`
	TargetPath  string `json:"target_path,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type Result struct {
	DryRun  bool     `json:"dry_run"`
	Actions []Action `json:"actions"`
}

type Reconciler struct {
	mu           sync.Mutex
	config       Config
	manifestPath string
	journalPath  string
	statePath    string
	lockPath     string
}

func NewReconciler(cfg Config) (*Reconciler, error) {
	root, err := cleanRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	if len(cfg.AllowedSourceRoots) == 0 {
		return nil, errors.New("vfs: at least one source root is required")
	}
	roots := make([]string, 0, len(cfg.AllowedSourceRoots))
	for _, sourceRoot := range cfg.AllowedSourceRoots {
		cleaned, err := cleanRoot(sourceRoot)
		if err != nil {
			return nil, fmt.Errorf("vfs: source root %q: %w", sourceRoot, err)
		}
		if overlaps(root, cleaned) {
			return nil, fmt.Errorf("vfs: output root %q overlaps source root %q", root, cleaned)
		}
		roots = append(roots, cleaned)
	}
	return &Reconciler{
		config:       Config{Root: root, AllowedSourceRoots: roots},
		manifestPath: filepath.Join(root, ".shokoanime-manifest.json"),
		journalPath:  ChangeJournalPath(root),
		statePath:    CrawlStatePath(root),
		lockPath:     filepath.Join(root, ".shokoanime.lock"),
	}, nil
}

func (r *Reconciler) Reconcile(ctx context.Context, plan topology.Plan, dryRun bool) (Result, error) {
	var result Result
	err := r.WithExclusive(ctx, func(locked *Reconciler) error {
		var err error
		result, err = locked.ReconcileLocked(ctx, plan, dryRun)
		return err
	})
	return result, err
}

// WithExclusive serializes VFS mutations and persistent crawl state across
// plugin processes. The callback must use the locked methods on the supplied
// reconciler; it must not call Reconcile or PollChanges recursively.
func (r *Reconciler) WithExclusive(ctx context.Context, fn func(*Reconciler) error) error {
	if r == nil || fn == nil {
		return errors.New("vfs: exclusive callback is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := r.acquireLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(r)
}

// ReconcileLocked applies a plan while the caller holds WithExclusive.
func (r *Reconciler) ReconcileLocked(ctx context.Context, plan topology.Plan, dryRun bool) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	for _, diagnostic := range plan.Diagnostics {
		if diagnostic.Severity == "error" {
			return Result{}, fmt.Errorf("vfs: plan contains an error for file %d: %s", diagnostic.FileID, diagnostic.Message)
		}
	}
	if err := r.ensureRoot(!dryRun); err != nil {
		return Result{}, err
	}
	previous, err := r.readManifest()
	if err != nil {
		return Result{}, err
	}
	if !dryRun {
		// Validate existing plugin state before mutating links. The journal is
		// part of the reconciliation transaction: a corrupt or operator-owned
		// journal must fail before the manifest and links move forward.
		if _, err := r.readJournal(); err != nil {
			return Result{}, err
		}
	}
	desired, err := r.validatePlan(plan)
	if err != nil {
		return Result{}, err
	}
	result := Result{DryRun: dryRun, Actions: make([]Action, 0)}
	for key, old := range previous.Entries {
		item, keep := desired[key]
		if keep && item.LogicalPath == old.LogicalPath {
			continue
		}
		reason := "no longer present in Shoko snapshot"
		if keep {
			reason = "logical path changed in Shoko snapshot"
		}
		result.Actions = append(result.Actions, Action{
			Kind:        "remove",
			LogicalPath: old.LogicalPath,
			TargetPath:  old.TargetPath,
			Reason:      reason,
		})
	}
	for key, item := range desired {
		old, existed := previous.Entries[key]
		if existed && old.LogicalPath == item.LogicalPath && old.TargetPath == item.TargetPath && r.linkMatches(item.LogicalPath, item.TargetPath) {
			result.Actions = append(result.Actions, Action{Kind: "unchanged", LogicalPath: item.LogicalPath, TargetPath: item.TargetPath})
			continue
		}
		action := "create"
		if existed {
			action = "update"
		}
		result.Actions = append(result.Actions, Action{Kind: action, LogicalPath: item.LogicalPath, TargetPath: item.TargetPath})
	}
	sort.Slice(result.Actions, func(i, j int) bool {
		if result.Actions[i].LogicalPath == result.Actions[j].LogicalPath {
			return result.Actions[i].TargetPath < result.Actions[j].TargetPath
		}
		return result.Actions[i].LogicalPath < result.Actions[j].LogicalPath
	})
	if dryRun {
		return result, nil
	}

	for key, old := range previous.Entries {
		item, keep := desired[key]
		if keep && item.LogicalPath == old.LogicalPath {
			continue
		}
		if _, err := validateTarget(old.TargetPath, r.config.AllowedSourceRoots); err != nil {
			return Result{}, fmt.Errorf("vfs: obsolete manifest entry %q has unsafe target: %w", key, err)
		}
		if err := r.removeOwned(old); err != nil {
			return Result{}, err
		}
	}
	for key, item := range desired {
		old, existed := previous.Entries[key]
		if existed && old.LogicalPath == item.LogicalPath && old.TargetPath == item.TargetPath && r.linkMatches(item.LogicalPath, item.TargetPath) {
			continue
		}
		if existed && old.LogicalPath == item.LogicalPath {
			if _, err := validateTarget(old.TargetPath, r.config.AllowedSourceRoots); err != nil {
				return Result{}, fmt.Errorf("vfs: previous manifest entry %q has unsafe target: %w", key, err)
			}
		}
		if err := r.writeLink(item, previousItem(old, existed && old.LogicalPath == item.LogicalPath)); err != nil {
			return Result{}, err
		}
	}
	manifest := Manifest{Version: manifestVersion, UpdatedAt: time.Now().UTC(), Entries: desired}
	if err := r.writeManifest(manifest); err != nil {
		return Result{}, err
	}
	changedPaths := make([]string, 0, len(result.Actions))
	for _, action := range result.Actions {
		if action.Kind == "unchanged" {
			continue
		}
		linkPath, err := r.logicalPath(action.LogicalPath)
		if err != nil {
			return Result{}, err
		}
		changedPaths = append(changedPaths, linkPath)
	}
	if err := r.appendJournal(changedPaths); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (r *Reconciler) validatePlan(plan topology.Plan) (map[string]ManifestItem, error) {
	desired := make(map[string]ManifestItem, len(plan.Entries))
	logicalPaths := make(map[string]string, len(plan.Entries))
	for _, entry := range plan.Entries {
		if entry.StableKey == "" || entry.LogicalPath == "" {
			return nil, errors.New("vfs: every plan entry requires a stable key and logical path")
		}
		linkPath, err := r.logicalPath(entry.LogicalPath)
		if err != nil {
			return nil, err
		}
		if previousKey, exists := logicalPaths[entry.LogicalPath]; exists && previousKey != entry.StableKey {
			return nil, fmt.Errorf("%w: %q is used by %s and %s", ErrPathConflict, entry.LogicalPath, previousKey, entry.StableKey)
		}
		logicalPaths[entry.LogicalPath] = entry.StableKey
		target, err := validateTarget(entry.TargetPath, r.config.AllowedSourceRoots)
		if err != nil {
			return nil, fmt.Errorf("vfs: %s: %w", entry.StableKey, err)
		}
		if filepath.Clean(linkPath) == filepath.Clean(r.manifestPath) || filepath.Clean(linkPath) == filepath.Clean(r.journalPath) || filepath.Clean(linkPath) == filepath.Clean(r.statePath) || filepath.Clean(linkPath) == filepath.Clean(r.lockPath) {
			return nil, errors.New("vfs: plan cannot own plugin state file")
		}
		item := ManifestItem{LogicalPath: entry.LogicalPath, TargetPath: target}
		if previous, exists := desired[entry.StableKey]; exists && previous != item {
			return nil, fmt.Errorf("vfs: stable key %q appears with multiple bindings", entry.StableKey)
		}
		desired[entry.StableKey] = item
	}
	return desired, nil
}

func (r *Reconciler) logicalPath(logical string) (string, error) {
	logical = filepath.FromSlash(strings.TrimSpace(logical))
	if logical == "" || filepath.IsAbs(logical) {
		return "", fmt.Errorf("vfs: logical path %q must be relative", logical)
	}
	cleaned := filepath.Clean(logical)
	relative, err := filepath.Rel(r.config.Root, filepath.Join(r.config.Root, cleaned))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("%w: logical path %q", ErrOutsideRoot, logical)
	}
	return filepath.Join(r.config.Root, relative), nil
}

func validateTarget(target string, roots []string) (string, error) {
	if target == "" || !filepath.IsAbs(target) {
		return "", ErrOutsideRoot
	}
	target = filepath.Clean(target)
	for _, root := range roots {
		if !strictUnder(target, root) {
			continue
		}
		// Existing targets are checked after resolving symlinks. Missing media is
		// still planned, but its existing parent must resolve inside the root.
		check := target
		if _, err := os.Lstat(check); err != nil {
			check = filepath.Dir(check)
		}
		resolved, err := filepath.EvalSymlinks(check)
		if err == nil && !strictUnder(filepath.Clean(resolved), root) && filepath.Clean(resolved) != filepath.Clean(root) {
			return "", ErrOutsideRoot
		}
		return target, nil
	}
	return "", ErrOutsideRoot
}

func (r *Reconciler) linkMatches(logical, target string) bool {
	linkPath, err := r.logicalPath(logical)
	if err != nil {
		return false
	}
	link, err := os.Readlink(linkPath)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(linkPath), link)
	}
	return filepath.Clean(link) == filepath.Clean(target)
}

func previousItem(item ManifestItem, owned bool) *ManifestItem {
	if !owned {
		return nil
	}
	return &item
}

func (r *Reconciler) writeLink(item ManifestItem, previous *ManifestItem) error {
	linkPath, err := r.logicalPath(item.LogicalPath)
	if err != nil {
		return err
	}
	if err := ensureParents(r.config.Root, filepath.Dir(linkPath)); err != nil {
		return err
	}
	if info, err := os.Lstat(linkPath); err == nil {
		if info.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("%w: %s is not a symlink", ErrPathConflict, item.LogicalPath)
		}
		if previous == nil {
			return fmt.Errorf("%w: %s is not in the previous manifest", ErrNotOwned, item.LogicalPath)
		}
		if !r.linkMatches(item.LogicalPath, previous.TargetPath) {
			return fmt.Errorf("%w: %s target changed outside the manifest", ErrNotOwned, item.LogicalPath)
		}
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("vfs: replace %q: %w", item.LogicalPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("vfs: inspect %q: %w", item.LogicalPath, err)
	}

	digest := sha256.Sum256([]byte(item.LogicalPath + "\x00" + item.TargetPath))
	tmpFile, err := os.CreateTemp(filepath.Dir(linkPath), ".shokoanime-link-"+hex.EncodeToString(digest[:8])+"-*")
	if err != nil {
		return fmt.Errorf("vfs: create temporary link: %w", err)
	}
	tmp := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vfs: close temporary link: %w", err)
	}
	if err := os.Remove(tmp); err != nil {
		return fmt.Errorf("vfs: prepare temporary link: %w", err)
	}
	relativeTarget, err := filepath.Rel(filepath.Dir(linkPath), item.TargetPath)
	if err != nil {
		return fmt.Errorf("vfs: calculate link target: %w", err)
	}
	if err := os.Symlink(relativeTarget, tmp); err != nil {
		return fmt.Errorf("vfs: create temporary link: %w", err)
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vfs: install link %q: %w", item.LogicalPath, err)
	}
	return nil
}

func (r *Reconciler) removeOwned(item ManifestItem) error {
	linkPath, err := r.logicalPath(item.LogicalPath)
	if err != nil {
		return err
	}
	info, err := os.Lstat(linkPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vfs: inspect obsolete link %q: %w", item.LogicalPath, err)
	}
	if info.Mode()&fs.ModeSymlink == 0 || !r.linkMatches(item.LogicalPath, item.TargetPath) {
		return fmt.Errorf("vfs: refusing to remove %q: %w", item.LogicalPath, ErrNotOwned)
	}
	if err := os.Remove(linkPath); err != nil {
		return fmt.Errorf("vfs: remove obsolete link %q: %w", item.LogicalPath, err)
	}
	return nil
}

func (r *Reconciler) ensureRoot(create bool) error {
	info, err := os.Lstat(r.config.Root)
	if os.IsNotExist(err) && create {
		if err := os.Mkdir(r.config.Root, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("vfs: create output root: %w", err)
		}
		info, err = os.Lstat(r.config.Root)
	}
	if os.IsNotExist(err) && !create {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeRoot
	}
	return nil
}

func (r *Reconciler) readManifest() (Manifest, error) {
	if info, err := os.Lstat(r.manifestPath); err == nil {
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Manifest{}, fmt.Errorf("vfs: manifest is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return Manifest{}, fmt.Errorf("vfs: inspect manifest: %w", err)
	}
	data, err := os.ReadFile(r.manifestPath)
	if os.IsNotExist(err) {
		return Manifest{Version: manifestVersion, Entries: make(map[string]ManifestItem)}, nil
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("vfs: read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("vfs: decode manifest: %w", err)
	}
	if manifest.Version != manifestVersion {
		return Manifest{}, fmt.Errorf("vfs: unsupported manifest version %d", manifest.Version)
	}
	if manifest.Entries == nil {
		manifest.Entries = make(map[string]ManifestItem)
	}
	return manifest, nil
}

func (r *Reconciler) writeManifest(manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("vfs: encode manifest: %w", err)
	}
	tmpFile, err := os.CreateTemp(r.config.Root, ".shokoanime-manifest-*")
	if err != nil {
		return fmt.Errorf("vfs: create manifest temp file: %w", err)
	}
	tmp := tmpFile.Name()
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("vfs: protect manifest temp file: %w", err)
	}
	if _, err := tmpFile.Write(append(data, '\n')); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("vfs: write manifest: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vfs: close manifest temp file: %w", err)
	}
	if err := os.Rename(tmp, r.manifestPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vfs: install manifest: %w", err)
	}
	return nil
}

func cleanRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) {
		return "", ErrUnsafeRoot
	}
	cleaned := filepath.Clean(value)
	if cleaned == string(filepath.Separator) {
		return "", ErrUnsafeRoot
	}
	return cleaned, nil
}

func strictUnder(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func overlaps(a, b string) bool {
	return a == b || strictUnder(a, b) || strictUnder(b, a)
}

func ensureParents(root, targetDir string) error {
	if !strictUnder(targetDir, root) && filepath.Clean(targetDir) != filepath.Clean(root) {
		return ErrOutsideRoot
	}
	relative, err := filepath.Rel(root, targetDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return ErrOutsideRoot
	}
	if relative == "." {
		return checkDirectory(root)
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
				return fmt.Errorf("vfs: create parent directory %q: %w", current, err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("vfs: inspect parent %q: %w", current, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("vfs: unsafe parent directory %q", current)
		}
	}
	return nil
}

func checkDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("vfs: unsafe parent directory %q", path)
	}
	return nil
}
