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

// Failure is one link the reconcile could not create or remove. Other links are
// unaffected, and the link is retried by the next reconcile.
type Failure struct {
	LogicalPath string `json:"logical_path"`
	TargetPath  string `json:"target_path,omitempty"`
	Reason      string `json:"reason"`
}

type Result struct {
	DryRun   bool      `json:"dry_run"`
	Actions  []Action  `json:"actions"`
	Failures []Failure `json:"failures,omitempty"`
	// Themes is the theme-song sync that followed the reconcile, if any.
	Themes *ThemeResult `json:"themes,omitempty"`
}

type Reconciler struct {
	exclusive    chan struct{}
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
		exclusive:    make(chan struct{}, 1),
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
	select {
	case r.exclusive <- struct{}{}:
		defer func() { <-r.exclusive }()
	case <-ctx.Done():
		return ctx.Err()
	}
	unlock, err := r.acquireLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(r)
}

// ReconcileLocked applies a plan while the caller holds WithExclusive.
// ReconcileOptions tune a reconcile.
type ReconcileOptions struct {
	// TrustUnchanged skips checking links the manifest already has with the
	// same path and target: their targets are not validated again and the
	// links on disk are not read. On a media share where every file lookup
	// costs milliseconds, checking every link makes a small change take
	// minutes. A full reconcile, without this option, still checks them all.
	TrustUnchanged bool
}

func (r *Reconciler) ReconcileLocked(ctx context.Context, plan topology.Plan, dryRun bool) (Result, error) {
	return r.ReconcileLockedWith(ctx, plan, dryRun, ReconcileOptions{})
}

// ReconcileLockedWith is ReconcileLocked with options.
func (r *Reconciler) ReconcileLockedWith(ctx context.Context, plan topology.Plan, dryRun bool, options ReconcileOptions) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
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
	trusted := Manifest{}
	if options.TrustUnchanged {
		trusted = previous
	}
	desired, failures, err := r.validatePlan(plan, trusted)
	if err != nil {
		return Result{}, err
	}
	result := Result{DryRun: dryRun, Actions: make([]Action, 0), Failures: failures}
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
		if existed && old.LogicalPath == item.LogicalPath && old.TargetPath == item.TargetPath && (options.TrustUnchanged || r.linkMatches(item.LogicalPath, item.TargetPath)) {
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

	// A link that cannot be changed is reported and skipped so one bad path never
	// holds back the rest of the library. The manifest records what is actually
	// on disk, so the next reconcile retries exactly what is left.
	entries := make(map[string]ManifestItem, len(desired))
	failed := make(map[string]string)
	fail := func(item ManifestItem, err error) {
		failed[item.LogicalPath] = err.Error()
		result.Failures = append(result.Failures, Failure{LogicalPath: item.LogicalPath, TargetPath: item.TargetPath, Reason: err.Error()})
	}
	for key, old := range previous.Entries {
		item, keep := desired[key]
		if keep && item.LogicalPath == old.LogicalPath {
			continue
		}
		if err := r.removeOwned(old); err != nil {
			fail(old, err)
			if !keep {
				entries[key] = old
			}
		}
	}
	written := 0
	for key, item := range desired {
		old, existed := previous.Entries[key]
		sameLogicalPath := existed && old.LogicalPath == item.LogicalPath
		if sameLogicalPath && old.TargetPath == item.TargetPath && (options.TrustUnchanged || r.linkMatches(item.LogicalPath, item.TargetPath)) {
			entries[key] = item
			written++
			continue
		}
		if err := r.writeLink(item, previousItem(old, sameLogicalPath)); err != nil {
			fail(item, err)
			if sameLogicalPath {
				entries[key] = old
			}
			continue
		}
		entries[key] = item
		written++
	}
	manifest := Manifest{Version: manifestVersion, UpdatedAt: time.Now().UTC(), Entries: entries}
	if err := r.writeManifest(manifest); err != nil {
		return Result{}, err
	}
	changedPaths := make([]string, 0, len(result.Actions))
	for i, action := range result.Actions {
		if reason, isFailed := failed[action.LogicalPath]; isFailed {
			result.Actions[i].Kind = "failed"
			result.Actions[i].Reason = reason
			continue
		}
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
	// Nothing could be written at all: that is a broken output root rather than
	// a few bad paths, and the task should fail visibly.
	if written == 0 && len(result.Failures) > 0 {
		return result, fmt.Errorf("vfs: no links could be written, first error: %s", result.Failures[0].Reason)
	}
	return result, nil
}

// validatePlan returns the links to create. An entry whose target falls outside
// the allowed source roots is left out and reported as a failure. An entry
// that trusted has with the same path and target counts as validated.
func (r *Reconciler) validatePlan(plan topology.Plan, trusted Manifest) (map[string]ManifestItem, []Failure, error) {
	desired := make(map[string]ManifestItem, len(plan.Entries))
	logicalPaths := make(map[string]string, len(plan.Entries))
	validator := newTargetValidator(r.config.AllowedSourceRoots)
	var failures []Failure
	for _, entry := range plan.Entries {
		if entry.StableKey == "" || entry.LogicalPath == "" {
			return nil, nil, errors.New("vfs: every plan entry requires a stable key and logical path")
		}
		linkPath, err := r.logicalPath(entry.LogicalPath)
		if err != nil {
			return nil, nil, err
		}
		if previousKey, exists := logicalPaths[entry.LogicalPath]; exists && previousKey != entry.StableKey {
			return nil, nil, fmt.Errorf("%w: %q is used by %s and %s", ErrPathConflict, entry.LogicalPath, previousKey, entry.StableKey)
		}
		logicalPaths[entry.LogicalPath] = entry.StableKey
		var target string
		if known, ok := trusted.Entries[entry.StableKey]; ok && known.LogicalPath == entry.LogicalPath && known.TargetPath == filepath.Clean(entry.TargetPath) {
			target = known.TargetPath
		} else {
			target, err = validator.validate(entry.TargetPath)
		}
		if err != nil {
			failures = append(failures, Failure{
				LogicalPath: entry.LogicalPath,
				TargetPath:  entry.TargetPath,
				Reason:      fmt.Sprintf("target is outside the configured source roots: %v", err),
			})
			continue
		}
		if filepath.Clean(linkPath) == filepath.Clean(r.manifestPath) || filepath.Clean(linkPath) == filepath.Clean(r.journalPath) || filepath.Clean(linkPath) == filepath.Clean(r.statePath) || filepath.Clean(linkPath) == filepath.Clean(SnapshotPath(r.config.Root)) || filepath.Clean(linkPath) == filepath.Clean(r.lockPath) || filepath.Clean(linkPath) == filepath.Clean(r.themeStatePath()) {
			return nil, nil, errors.New("vfs: plan cannot own plugin state file")
		}
		item := ManifestItem{LogicalPath: entry.LogicalPath, TargetPath: target}
		if previous, exists := desired[entry.StableKey]; exists && previous != item {
			return nil, nil, fmt.Errorf("vfs: stable key %q appears with multiple bindings", entry.StableKey)
		}
		desired[entry.StableKey] = item
	}
	return desired, failures, nil
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

// targetValidator is validateTarget with the folder resolutions remembered for
// one reconcile. A library's files share a few thousand folders, and on a
// network share each lookup of a path component costs milliseconds, so
// resolving every file's full path again made a full reconcile take minutes.
// Only each file itself is looked up, to catch one that is a symlink.
type targetValidator struct {
	roots    []string
	resolved map[string]resolution
}

type resolution struct {
	path string
	ok   bool
}

func newTargetValidator(roots []string) *targetValidator {
	return &targetValidator{roots: roots, resolved: make(map[string]resolution)}
}

func (v *targetValidator) evalSymlinks(path string) (string, bool) {
	if known, ok := v.resolved[path]; ok {
		return known.path, known.ok
	}
	resolved, err := filepath.EvalSymlinks(path)
	v.resolved[path] = resolution{path: resolved, ok: err == nil}
	return resolved, err == nil
}

// validate behaves as validateTarget.
func (v *targetValidator) validate(target string) (string, error) {
	if target == "" || !filepath.IsAbs(target) {
		return "", ErrOutsideRoot
	}
	target = filepath.Clean(target)
	for _, root := range v.roots {
		if !strictUnder(target, root) {
			continue
		}
		var resolved string
		info, err := os.Lstat(target)
		switch {
		case err != nil:
			// Missing media is still planned, but its existing parent must
			// resolve inside the root.
			parent, ok := v.evalSymlinks(filepath.Dir(target))
			if !ok {
				return target, nil
			}
			resolved = parent
		case info.Mode()&os.ModeSymlink != 0:
			path, err := filepath.EvalSymlinks(target)
			if err != nil {
				return target, nil
			}
			resolved = path
		default:
			parent, ok := v.evalSymlinks(filepath.Dir(target))
			if !ok {
				return target, nil
			}
			resolved = filepath.Join(parent, filepath.Base(target))
		}
		resolvedRoot, ok := v.evalSymlinks(root)
		if !ok {
			resolvedRoot = root
		}
		if !strictUnder(resolved, resolvedRoot) && filepath.Clean(resolved) != filepath.Clean(resolvedRoot) {
			return "", ErrOutsideRoot
		}
		return target, nil
	}
	return "", ErrOutsideRoot
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
		if err != nil {
			return target, nil
		}
		// The source root may itself be reached through a symlink, so compare
		// resolved paths on both sides.
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			resolvedRoot = root
		}
		if !strictUnder(resolved, resolvedRoot) && filepath.Clean(resolved) != filepath.Clean(resolvedRoot) {
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
		// An interrupted run can leave a correct link that the manifest does not
		// record yet. It already matches, so adopt it.
		if r.linkMatches(item.LogicalPath, item.TargetPath) {
			return nil
		}
		if previous == nil {
			return fmt.Errorf("%w: %s is not in the previous manifest; remove it to let the plugin manage this path", ErrNotOwned, item.LogicalPath)
		}
		if !r.linkMatches(item.LogicalPath, previous.TargetPath) {
			return fmt.Errorf("%w: %s target changed outside the manifest; remove it to let the plugin manage this path", ErrNotOwned, item.LogicalPath)
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
