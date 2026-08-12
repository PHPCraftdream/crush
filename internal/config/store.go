package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	hyperp "github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/oauth/hyper"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// fileSnapshot captures metadata about a config file at a point in time.
type fileSnapshot struct {
	Path    string
	Exists  bool
	Size    int64
	ModTime int64 // UnixNano
}

// RuntimeOverrides holds per-session settings that are never persisted to
// disk. They are applied on top of the loaded Config and survive only for
// the lifetime of the process (or workspace).
type RuntimeOverrides struct {
	SkipPermissionRequests bool
}

// storeSnapshot is an immutable point-in-time view of everything that
// ReloadFromDisk (or a copy-on-write mutator) replaces as one unit. A
// reader that loads a *storeSnapshot via ConfigStore.snap always sees a
// single, internally-consistent generation: config/resolver/knownProviders/
// etc. all came from the same load or the same mutation, never a mix of an
// old and a new generation torn across separate fields.
//
// Fields NOT here (workingDir, globalDataPath) are set once when the
// ConfigStore is constructed and never change afterwards, so they don't
// need to be part of the versioned snapshot.
type storeSnapshot struct {
	config             *Config
	resolver           VariableResolver
	knownProviders     []catwalk.Provider
	loadedPaths        []string // config files that were successfully loaded
	trackedConfigPaths []string // unique, normalized config file paths
	snapshots          map[string]fileSnapshot
	workspacePath      string // .crush/crush.json (recomputed on every reload)
	overrides          RuntimeOverrides

	// generation is a monotonically increasing counter assigned at publish
	// time (see ConfigStore.publishLocked). It exists so a long-running
	// candidate build in reloadFromDiskUnlocked (which runs WITHOUT
	// publishMu — see reloadMu) can detect, at publish time, whether some
	// other writer already published a newer generation while the build
	// was in flight, and rebase instead of silently clobbering it with a
	// candidate built from a now-stale base. Never read by anything
	// outside this CAS check; it is not part of the store's public API.
	generation uint64
}

// clone returns a shallow copy of the snapshot. Callers that need to
// change one field (e.g. workspacePath, overrides) can clone then
// mutate the copy before publishing it — the original snapshot, still
// visible to any reader that loaded it earlier, is never touched.
func (sn *storeSnapshot) clone() *storeSnapshot {
	if sn == nil {
		return &storeSnapshot{}
	}
	c := *sn
	return &c
}

// ConfigStore is the single entry point for all config access. It owns the
// pure-data Config, runtime state (working directory, resolver, known
// providers), and persistence to both global and workspace config files.
//
// Thread-safety model: all state that changes together as one logical
// generation (config, resolver, knownProviders, loadedPaths, overrides,
// workspacePath, staleness tracking) lives in an immutable *storeSnapshot
// published through the snap atomic.Pointer. Readers call snap.Load() once
// and read every field from the same snapshot — lock-free, and always a
// single consistent generation. Writers (both ReloadFromDisk and the
// copy-on-write setters like SetCompactMode) serialize against each other
// via publishMu, build a brand new snapshot from a shallow copy of
// the config plus freshly cloned nested collections, and publish it with a
// single snap.Store — the old snapshot, and anything still holding a
// reference to it, is left completely untouched.
type ConfigStore struct {
	snap atomic.Pointer[storeSnapshot]

	// workingDir and globalDataPath are set once at construction time
	// (Load / NewTestStore) and never mutated afterwards, so they are
	// safe to read without synchronization.
	workingDir     string
	globalDataPath string // ~/.local/share/crush/crush.json

	// publishMu is the single mutex that serialises ALL snapshot
	// publications — both ReloadFromDisk (which rebuilds the entire
	// config from disk) and the copy-on-write mutators (updateConfig,
	// RefreshStalenessSnapshot, CaptureStalenessSnapshot). Every
	// read-current-generation → build-new → snap.Store cycle takes this
	// lock for its full duration, so a writer can never publish a clone
	// built from a stale generation on top of a reload that already
	// published a newer one (the lost-update race that existed when
	// reloadMu and writeMu were separate locks).
	//
	// autoReload's redundant-reload dedup is reloadMu.TryLock() (see
	// reloadMu below), NOT publishMu — Load holds ONLY publishMu for its
	// whole body, never reloadMu, so a re-entrant autoReload call from
	// inside configureProviders (running under Load or a reload) would
	// find reloadMu free, succeed its TryLock, and proceed into
	// buildAndPublishReload, which acquires publishMu itself — on the
	// SAME goroutine already holding it. sync.Mutex is not reentrant in
	// Go, so that self-deadlocks and hangs forever. This is exactly why
	// configureProviders must only ever call removeConfigFieldBestEffort
	// (which never calls autoReload at all) and must NEVER call the full
	// RemoveConfigField/SetConfigFields (which do call autoReload) from a
	// path that runs under a held publishMu.
	publishMu sync.Mutex

	// diskWriteMu serialises the on-disk read-modify-write cycle
	// (os.ReadFile → sjson.Set/Delete → atomicWriteFile) in
	// SetConfigFields and RemoveConfigField. Without it, two concurrent
	// callers writing to the same crush.json path could each read the
	// pre-write file, apply only their own key, and have the second
	// atomicWriteFile clobber the first — a classic lost-update on the
	// file itself, independent of the in-memory snapshot race that
	// publishMu fixes. It is deliberately separate from publishMu so
	// that disk I/O does not block lock-free snapshot readers, and so
	// that the autoReload call at the end of each disk write (which
	// acquires publishMu via ReloadFromDisk) cannot deadlock against a
	// publishMu already held by the same goroutine. Lock ordering is
	// always publishMu → diskWriteMu (when both are held), and
	// diskWriteMu is always released before autoReload is called.
	diskWriteMu sync.Mutex

	// reloadMu serialises the CANDIDATE-BUILD phase of a disk reload
	// (loadFromConfigPaths, workspace merge, configureProviders — which
	// includes every shell-substitution ResolveValue call for provider
	// keys/headers — and configureSelectedModels) against other
	// concurrent reload attempts, WITHOUT holding publishMu. This is the
	// fix for the lock-convoy where a single hung "$(...)" in a config
	// value used to block publishMu — and therefore every reader,
	// including unrelated runtime mutators like SetSkipPermissionRequests
	// or SetProviderRuntimeConfig — for as long as the shell substitution
	// ran (up to resolveTimeout).
	//
	// reloadFromDiskUnlocked takes reloadMu (via defer, held for its
	// whole call), builds a full candidate snapshot from local variables
	// ONLY (no store mutation), then — still holding reloadMu — takes
	// publishMu just long enough to CAS-check the generation and
	// publish. autoReload uses TryLock on reloadMu (not publishMu) to
	// preserve the original "skip a redundant reload when one is already
	// in progress" behaviour without reintroducing the publishMu hold
	// during the expensive candidate-build phase.
	//
	// Lock ordering when both are held: reloadMu is acquired first and
	// held THROUGH the publishMu acquisition (nested, not sequential —
	// reloadMu is NOT released before publishMu is taken). The global
	// order is always reloadMu → publishMu → diskWriteMu → the
	// inter-process config file lock, consistently in every call path
	// that touches more than one of these; there is no path that
	// acquires them in the reverse order. It is this consistent nesting
	// order — not any claim that the two locks are never held together —
	// that rules out a deadlock here.
	reloadMu sync.Mutex
}

// loadSnapshot returns the current published snapshot. It never returns
// nil: the store is always constructed with an initial snapshot.
func (s *ConfigStore) loadSnapshot() *storeSnapshot {
	sn := s.snap.Load()
	if sn == nil {
		// Defensive: should not happen for a store built via Load or
		// NewTestStore, but avoids a nil-pointer panic for any
		// zero-value ConfigStore{} a test might construct directly.
		return &storeSnapshot{}
	}
	return sn
}

// publishLocked assigns the next generation number to next and publishes
// it. The caller MUST already hold publishMu — generation is only ever
// assigned/read while holding that lock, so incrementing it here is not
// itself a race even though the field is also readable lock-free via
// loadSnapshot() (readers only ever compare it back against a value they
// captured under publishMu; see reloadFromDiskUnlocked's CAS check).
func (s *ConfigStore) publishLocked(next *storeSnapshot) {
	next.generation = s.loadSnapshot().generation + 1
	s.snap.Store(next)
}

// Config returns the pure-data config struct (read-only after load).
func (s *ConfigStore) Config() *Config {
	return s.loadSnapshot().config
}

// Generation returns the current generation number of the config snapshot.
// The generation is incremented on every config mutation (reload, set operations, etc.).
// Callers can use this to detect when config has changed and invalidate caches.
func (s *ConfigStore) Generation() uint64 {
	return s.loadSnapshot().generation
}

// Snapshot returns both the config and its generation atomically from a single
// storeSnapshot. This prevents reading config and generation separately and
// getting inconsistent results if a reload occurs between the two reads
// (task #341, P1-3).
func (s *ConfigStore) Snapshot() (*Config, uint64) {
	sn := s.loadSnapshot()
	return sn.config, sn.generation
}

// WorkingDir returns the current working directory.
func (s *ConfigStore) WorkingDir() string {
	return s.workingDir
}

// Resolver returns the variable resolver.
func (s *ConfigStore) Resolver() VariableResolver {
	return s.loadSnapshot().resolver
}

// Resolve resolves a variable reference using the configured resolver.
func (s *ConfigStore) Resolve(key string) (string, error) {
	resolver := s.loadSnapshot().resolver
	if resolver == nil {
		return "", fmt.Errorf("no variable resolver configured")
	}
	return resolver.ResolveValue(key)
}

// KnownProviders returns the list of known providers. The returned slice
// is not copied: reload/mutation always replaces the whole slice (never
// mutates an existing one in place), so a snapshot's backing array is
// never written to after publication and it's safe to hand it out
// directly.
func (s *ConfigStore) KnownProviders() []catwalk.Provider {
	return s.loadSnapshot().knownProviders
}

// SetupAgents (re)configures the coder and task agents on the CURRENT
// generation, through the copy-on-write updateConfig path — it does NOT
// mutate the currently-published *Config in place (that was the P2.1 bug
// this method used to have before being briefly removed as dead code; it
// turned out to still be a real, load-bearing test helper — see
// coordinator_test.go's newRoleModelTestCoordinator/newWorkerToolTestCoordinator/
// TestBuildTools_CoderHasAskQuestion, which call this on a from-scratch
// config where IsConfigured() is false, so config.Load's own
// configureSelectedModels/SetupAgents call never ran and Agents would
// otherwise stay empty). Config.SetupAgents itself only reads
// Options/DisabledTools and assigns a fresh Agents map, so running it on a
// clone and publishing that clone is a correct, race-free way to expose the
// same behavior at the ConfigStore level.
func (s *ConfigStore) SetupAgents() {
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.SetupAgents()
	})
}

// Overrides returns the runtime overrides as a value copy. Callers cannot
// mutate the store's internal snapshot state through the returned value —
// use SetSkipPermissionRequests for writes, which goes through the
// copy-on-write publish path (publishMu + clone + atomic Store) so the
// change is visible as a new generation and survives subsequent reloads
// (reloadFromDiskLocked carries overrides forward from prev.overrides).
func (s *ConfigStore) Overrides() RuntimeOverrides {
	return s.loadSnapshot().overrides
}

// SetSkipPermissionRequests sets the SkipPermissionRequests runtime override
// through the copy-on-write path (publishMu + snapshot clone + atomic Store),
// so the change is visible as a new generation and survives subsequent
// reloads (reloadFromDiskLocked carries overrides forward from prev.overrides).
// Unlike the old Overrides() pointer-return pattern, this cannot lose the
// write to a concurrent reload that publishes a new snapshot between the
// caller's read and write.
func (s *ConfigStore) SetSkipPermissionRequests(v bool) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	cur := s.loadSnapshot()
	next := cur.clone()
	next.overrides.SkipPermissionRequests = v
	s.publishLocked(next)
}

// LoadedPaths returns the config file paths that were successfully loaded.
// Returns a copy so callers can't mutate the snapshot's backing array.
func (s *ConfigStore) LoadedPaths() []string {
	return slices.Clone(s.loadSnapshot().loadedPaths)
}

// configPath returns the file path for the given scope.
func (s *ConfigStore) configPath(scope Scope) (string, error) {
	switch scope {
	case ScopeWorkspace:
		workspacePath := s.loadSnapshot().workspacePath
		if workspacePath == "" {
			return "", ErrNoWorkspaceConfig
		}
		return workspacePath, nil
	default:
		return s.globalDataPath, nil
	}
}

// HasConfigField checks whether a key exists in the config file for the given
// scope.
func (s *ConfigStore) HasConfigField(scope Scope, key string) bool {
	path, err := s.configPath(scope)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return gjson.Get(string(data), key).Exists()
}

// configWriteLockTimeout caps how long a config write waits for the
// inter-process sidecar lock before failing. Mirrors the 30s budget used by
// cliprovider's acquireMCPConfigLock for the same class of cross-process
// config read-modify-write, so a wedged sibling crush process (debugger
// attached, suspended shell, frozen network mount) cannot indefinitely freeze
// parallel runs that share the same crush.json.
//
// This is the budget for explicit, user-triggered writes (SetConfigFields,
// RemoveConfigField/RemoveProviderAPIKey called outside of Load/reload).
// Callers that run INSIDE Load/reloadFromDiskLocked while holding publishMu
// must NOT use this timeout directly — see internalConfigWriteLockTimeout.
const configWriteLockTimeout = 30 * time.Second

// internalConfigWriteLockTimeout bounds config writes issued from inside
// configureProviders (e.g. the "drop legacy providers.anthropic OAuth
// config" cleanup), which runs synchronously inside Load and
// reloadFromDiskLocked while publishMu is held for the whole call. Regressed
// by b53cbf3d: before that commit this was a same-process disk write with no
// cross-process lock and completed in milliseconds; after it, the same call
// can block for up to configWriteLockTimeout (30s) on the inter-process
// sidecar lock if a sibling crush process holds it (or is wedged holding
// it) — and because publishMu is held for the duration, EVERY reader of the
// config store (including app startup via Load, and ReloadFromDisk,
// SetSkipPermissionRequests, CaptureStalenessSnapshot, etc.) stalls too.
//
// A much shorter budget is safe here specifically because this call site
// already tolerates failure: on timeout the on-disk key simply isn't
// removed yet, the in-memory config is still corrected via Providers.Del
// right after (see configureProviders), and the very next successful
// autoReload/Load (this process or a sibling one) will retry the disk
// cleanup. This is a best-effort cache-eviction of a deprecated on-disk
// key, not a user-initiated write that must succeed synchronously.
const internalConfigWriteLockTimeout = 2 * time.Second

// configWriteLockStallLogThreshold is how long withConfigWriteLock waits
// for the inter-process sidecar lock before logging a warning that the wait
// is unusually long. This makes cross-process contention (or a wedged
// sibling process holding the lock) diagnosable — without it, a stalled
// config write just looks like "crush doesn't start" or "crush hangs" with
// no signal pointing at the lock file. Logged regardless of which timeout
// (configWriteLockTimeout or internalConfigWriteLockTimeout) is in effect,
// and regardless of whether the wait eventually succeeds or times out.
const configWriteLockStallLogThreshold = 500 * time.Millisecond

// withConfigWriteLock runs fn while holding BOTH the in-process diskWriteMu
// and an exclusive OS-level lock on a sidecar lock file co-located with the
// config path, waiting up to configWriteLockTimeout for the OS lock. Use
// this for explicit, user-triggered writes. Callers running inside
// Load/reloadFromDiskLocked (i.e. anything reachable from configureProviders
// while publishMu is held) must call withConfigWriteLockCtx with a much
// shorter, caller-supplied timeout instead — see internalConfigWriteLockTimeout.
func (s *ConfigStore) withConfigWriteLock(path string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.withConfigWriteLockCtx(ctx, path, fn)
}

// withConfigWriteLockCtx is withConfigWriteLock parameterised on the
// inter-process lock wait's deadline via ctx, so callers on a tight budget
// (e.g. the config-cleanup call inside configureProviders, which runs while
// publishMu is held for the entire Load/reloadFromDiskLocked call) can pass
// a much shorter timeout than the default 30s. See
// internalConfigWriteLockTimeout for why a short budget is safe there.
//
// diskWriteMu serialises concurrent goroutines inside THIS process; the OS
// lock (flock on POSIX, LockFileEx on Windows, via session.FileLock)
// serialises SEPARATE crush processes that share the same config file — two
// parallel `crush run` sessions on one machine each own a private
// diskWriteMu, so without the OS lock each could read the same pre-write
// file, apply only its own key, and the second atomicWriteFile would
// silently erase the first writer's key (a classic lost-update across
// processes that diskWriteMu alone cannot prevent, since it is not visible
// to the other process). The OS lock is per open file description, so two
// ConfigStore instances in one test (each with its own diskWriteMu) contend
// on it exactly as two real processes would.
//
// Both locks are released before fn returns, so the caller's subsequent
// autoReload (which acquires publishMu via ReloadFromDisk) cannot deadlock
// against a re-entrant caller that already holds publishMu and entered this
// path (e.g. Load → configureProviders → RemoveConfigField). Lock ordering
// is always diskWriteMu → inter-process file lock.
func (s *ConfigStore) withConfigWriteLockCtx(ctx context.Context, path string, fn func() error) error {
	s.diskWriteMu.Lock()
	defer s.diskWriteMu.Unlock()

	waitStart := time.Now()
	lock, err := session.AcquireFileLockContext(ctx, path+".lock")
	if wait := time.Since(waitStart); wait >= configWriteLockStallLogThreshold {
		// Diagnosability (see configWriteLockStallLogThreshold): without
		// this, a contended or wedged sidecar lock just looks like "crush
		// hangs on startup" with nothing pointing at the lock file.
		slog.Warn("Config write waited unusually long for inter-process lock",
			"path", path+".lock", "wait", wait, "succeeded", err == nil)
	}
	if err != nil {
		return fmt.Errorf("failed to lock config file %q: %w", path, err)
	}
	// Deliberately NOT calling os.Remove on the sidecar after Release: the
	// lock file is left on disk permanently by design, not cleaned up here.
	// A remove-after-release would race a concurrent process that has
	// already reopened/relocked the same path between our unlock and the
	// remove — on POSIX that's harmless (flock is keyed off the inode, so
	// unlinking a path while another process holds an open fd to the old
	// inode doesn't disturb that process's lock), but on Windows it is not:
	// os.OpenFile here does not pass FILE_SHARE_DELETE, so a concurrent
	// holder's open handle would make our os.Remove fail with a sharing
	// violation, and even if it succeeded, deleting the path out from under
	// a process that just resolved it for LockFileEx risks the next opener
	// creating a distinct, unrelated file object at the same path while the
	// old one is still locked — reintroducing exactly the split-brain this
	// lock exists to prevent. Leaving a handful of empty *.lock sidecars
	// next to crush.json is a one-time, bounded cost; deleting them is not.
	defer lock.Release()
	return fn()
}

// SetConfigField sets a key/value pair in the config file for the given scope.
// After a successful write, it automatically reloads config to keep in-memory
// state fresh.
func (s *ConfigStore) SetConfigField(scope Scope, key string, value any) error {
	return s.SetConfigFields(scope, map[string]any{key: value})
}

// SetConfigFields sets multiple key/value pairs in the config file for the
// given scope in a single read-modify-write, so callers that need to persist
// several keys at once (e.g. UpdatePreferredModels) get one atomic on-disk
// write instead of one per key. Keys are applied in sorted order so the
// resulting JSON is deterministic regardless of map iteration order, keeping
// crush.json diffs stable across runs.
//
// The read-modify-write cycle is protected at two levels: an in-process
// diskWriteMu (serialises concurrent goroutines within this ConfigStore) and
// an inter-process OS-level file lock on a path+".lock" sidecar, acquired via
// withConfigWriteLock (backed by session.FileLock — flock on POSIX,
// LockFileEx on Windows). The in-process mutex alone cannot prevent two
// separate `crush` processes sharing the same crush.json from each reading
// the pre-write file, applying only their own keys, and the second
// atomicWriteFile silently clobbering the first — the file lock closes that
// cross-process gap. After a successful write, it automatically reloads
// config to keep in-memory state fresh.
func (s *ConfigStore) SetConfigFields(scope Scope, kv map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return fmt.Errorf("%v: %w", kv, err)
	}

	// Apply keys in sorted order so the on-disk output is deterministic
	// regardless of map iteration order (keeps crush.json diffs stable).
	keys := make([]string, 0, len(kv))
	for key := range kv {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	// withConfigWriteLock serialises the read-modify-write both in-process
	// (diskWriteMu) and across processes (OS lock on path+".lock") — see its
	// doc comment. The lock is released before autoReload below so that
	// autoReload's publishMu acquisition cannot deadlock.
	if err := s.withConfigWriteLock(path, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				data = []byte("{}")
			} else {
				return fmt.Errorf("failed to read config file: %w", err)
			}
		}
		newValue := string(data)
		for _, key := range keys {
			newValue, err = sjson.Set(newValue, key, kv[key])
			if err != nil {
				return fmt.Errorf("failed to set config field %s: %w", key, err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		if err := atomicWriteFile(path, []byte(newValue), 0o600); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Auto-reload to keep in-memory state fresh after config edits. Runs
	// OUTSIDE withConfigWriteLock so its publishMu acquisition cannot
	// deadlock against diskWriteMu/the file lock (see SetConfigFields
	// history). This alone does not make autoReload safe to call from
	// EVERY context: a caller that reaches this point while already
	// holding publishMu itself (Load, via configureSelectedModels ->
	// updatePreferredModelsLocked -> SetConfigFields) would still hang
	// here, because autoReload's own dedup guard is reloadMu.TryLock(),
	// not publishMu — see Load's doc comment on why it now also holds
	// reloadMu for exactly this reason.
	if err := s.autoReload(context.Background()); err != nil {
		// Log warning but don't fail the write - disk is already updated.
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}

	return nil
}

// RemoveConfigField deletes a single key from the config file for the given
// scope (e.g. removing a stored provider API key). Like SetConfigFields, the
// on-disk read-modify-write is protected at two levels: the in-process
// diskWriteMu and an inter-process OS-level file lock on a path+".lock"
// sidecar acquired via withConfigWriteLock (backed by session.FileLock —
// flock on POSIX, LockFileEx on Windows), so two separate `crush` processes
// racing to edit the same crush.json cannot silently clobber each other's
// change. After a successful write, it automatically reloads config to keep
// in-memory state fresh.
//
// This waits up to the full configWriteLockTimeout (30s) for the
// inter-process lock. Callers running inside Load/reloadFromDiskLocked
// (i.e. under configureProviders while publishMu is held) must NOT use
// this — use removeConfigFieldBestEffort instead, which bounds the wait
// to internalConfigWriteLockTimeout so a contended/wedged lock cannot stall
// the whole config subsystem. See internalConfigWriteLockTimeout's doc.
func (s *ConfigStore) RemoveConfigField(scope Scope, key string) error {
	path, err := s.configPath(scope)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	if err := s.removeConfigFieldAt(ctx, path, key); err != nil {
		return err
	}

	// Auto-reload to keep in-memory state fresh after config edits.
	// Runs OUTSIDE withConfigWriteLock (see SetConfigFields).
	if err := s.autoReload(context.Background()); err != nil {
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}

	return nil
}

// removeConfigFieldAt performs the on-disk read-modify-write for
// RemoveConfigField/removeConfigFieldBestEffort against an already-resolved
// path, bounding the inter-process lock wait to ctx's deadline. Shared so
// both the full-timeout public API and the short-timeout internal caller
// (configureProviders) go through one write implementation.
func (s *ConfigStore) removeConfigFieldAt(ctx context.Context, path, key string) error {
	// Skip acquiring the write lock entirely when there is no config file to
	// edit: withConfigWriteLockCtx creates a path+".lock" sidecar (and its
	// parent directory) as a side effect of acquiring the OS-level lock, so
	// without this check, removing a field from a config that was never
	// written (a clean install, or a scope whose file genuinely doesn't
	// exist) would leave behind an empty *.lock file and directory for no
	// reason. There is nothing to delete from a nonexistent file, so this is
	// a legitimate no-op rather than an error.
	//
	// This check races benignly against a concurrent process creating path:
	// if that happens in the narrow window between this Stat and the lock
	// acquisition below, this call simply misses removing the key this time
	// (the same best-effort semantics removeConfigFieldBestEffort already
	// documents) rather than erroring or corrupting anything.
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat config file: %w", err)
	}

	return s.withConfigWriteLockCtx(ctx, path, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read config file: %w", err)
		}
		newValue, err := sjson.Delete(string(data), key)
		if err != nil {
			return fmt.Errorf("failed to delete config field %s: %w", key, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		if err := atomicWriteFile(path, []byte(newValue), 0o600); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
		return nil
	})
}

// removeConfigFieldBestEffort deletes a single key from the config file for
// the given scope, bounding the inter-process lock wait to
// internalConfigWriteLockTimeout instead of the full configWriteLockTimeout.
//
// Use this ONLY from call paths that run inside Load/reloadFromDiskLocked
// while publishMu is held for the whole call (currently: the "drop legacy
// providers.anthropic OAuth config" cleanup in configureProviders). Because
// publishMu gates every reader of the config store (ReloadFromDisk,
// SetSkipPermissionRequests, CaptureStalenessSnapshot, and app startup via
// Load itself), a stall here — waiting on a sidecar lock held by a
// contended or wedged sibling crush process — would freeze the whole config
// subsystem for as long as the wait budget allows. Regressed by b53cbf3d,
// which replaced a same-process, lock-free disk write with the
// cross-process-locked withConfigWriteLock path without adjusting the
// timeout for callers running under publishMu.
//
// Does NOT call autoReload — unlike RemoveConfigField, this is always
// called from inside Load/reloadFromDiskLocked, where publishMu is held for
// the whole call but reloadMu is NOT. autoReload's redundant-reload dedup is
// reloadMu.TryLock() (see the reloadMu field doc), which would SUCCEED here
// (nothing holds reloadMu), so calling autoReload from this path would be a
// genuine, deadlocking re-entrant call — not a safe no-op — since it would
// proceed into buildAndPublishReload and try to acquire publishMu on the
// same goroutine that already holds it. This function must never call
// autoReload for exactly that reason. The in-memory removal is instead
// handled by the caller via Providers.Del immediately after, and any
// process's next successful reload re-reads the on-disk state.
//
// On error (including timeout), the error is logged and swallowed: the
// caller already tolerates this key not being removed from disk yet — the
// in-memory state is corrected regardless, and the next successful
// Load/reload (in this process or a sibling one) retries the disk cleanup.
func (s *ConfigStore) removeConfigFieldBestEffort(scope Scope, key string) {
	path, err := s.configPath(scope)
	if err != nil {
		slog.Warn("Skipping best-effort config field removal: no path for scope", "key", key, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), internalConfigWriteLockTimeout)
	defer cancel()
	if err := s.removeConfigFieldAt(ctx, path, key); err != nil {
		slog.Warn("Best-effort config field removal did not complete; will retry on next reload",
			"key", key, "path", path, "error", err)
	}
}

// ReadModelsAtScope reads the per-scope `models.large` / `models.small` entries
// directly from the on-disk file for the given scope, ignoring any merge with
// the other scope. Returns (nil, nil) for a slot that the scope's file does not
// define; returns an error only on read/parse failure. Used by `crush models
// state` to show "what each scope says" alongside the effective merged view.
//
// Fork patch: batch 11 — `crush models state` needs per-scope visibility.
func (s *ConfigStore) ReadModelsAtScope(scope Scope) (large, small *SelectedModel, err error) {
	all, err := s.ReadAllModelsAtScope(scope)
	if err != nil {
		return nil, nil, err
	}
	return all[SelectedModelTypeLarge], all[SelectedModelTypeSmall], nil
}

// ReadAllModelsAtScope reads the per-scope `models.*` entries for all four
// slots (large, small, worker, reviewer) directly from the on-disk file for
// the given scope, ignoring any merge with the other scope. Missing slots are
// absent from the returned map; returns an error only on read/parse failure.
//
// Fork patch: worker/reviewer CLI settability — `crush models state` needs
// per-scope visibility into all four slots, not just large/small.
func (s *ConfigStore) ReadAllModelsAtScope(scope Scope) (map[SelectedModelType]*SelectedModel, error) {
	path, perr := s.configPath(scope)
	if perr != nil {
		// No path for this scope (e.g. workspace not initialised) — treat as
		// "nothing set". Not an error.
		return map[SelectedModelType]*SelectedModel{}, nil
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return map[SelectedModelType]*SelectedModel{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, rerr)
	}
	var sm struct {
		Models map[SelectedModelType]SelectedModel `json:"models"`
	}
	if err := json.Unmarshal(data, &sm); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[SelectedModelType]*SelectedModel, len(sm.Models))
	for _, slot := range []SelectedModelType{SelectedModelTypeLarge, SelectedModelTypeSmall, SelectedModelTypeWorker, SelectedModelTypeReviewer} {
		if v, ok := sm.Models[slot]; ok {
			v := v
			out[slot] = &v
		}
	}
	return out, nil
}

// updateConfig applies a targeted, copy-on-write mutation to the store's
// config and publishes it as a new generation.
//
// It takes publishMu (serialising all copy-on-write mutators AND reloads
// against each other — without it, two concurrent callers could both read
// the same starting snapshot, apply their own change to independent copies,
// and have the second Store() silently discard the first writer's change;
// worse, a concurrent reload's fresh-from-disk generation could be
// silently overwritten by a writer publishing a clone of the pre-reload
// generation),
// shallow-copies the top-level *Config (cfgCopy := *cur.config), and hands
// the mutate callback a pointer to that copy. mutate is responsible for
// cloning any nested map/pointer it intends to change (e.g. Options, a
// map[K]V field) before writing through it — updateConfig only guarantees
// the top-level struct is a fresh copy, not anything it points to. Once
// mutate returns, the new *Config is published as part of a new
// storeSnapshot (resolver/knownProviders/etc. carried over unchanged from
// the snapshot the mutation started from) via a single atomic Store.
//
// This is intentionally synchronous and always publishes — callers that
// also persist to disk (the common case: every setter below writes through
// SetConfigField/SetConfigFields right after) will shortly see their
// change superseded by autoReload's own fresh-from-disk snapshot; that's
// fine and matches pre-refactor semantics, where the in-memory mutation
// was always a best-effort "make the change visible immediately" step
// ahead of the disk-round-trip reload. When autoReload is skipped (no
// workingDir configured, e.g. in unit tests), this in-memory mutation is
// the only durable change, exactly as before.
func (s *ConfigStore) updateConfig(mutate func(cfgCopy *Config)) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.updateConfigLocked(mutate)
}

// updateConfigLocked applies the same copy-on-write mutation as
// updateConfig but WITHOUT acquiring publishMu. The caller MUST already
// hold publishMu.
//
// It exists for the re-entrant call path inside Load: Load holds
// publishMu, then configureSelectedModels → updatePreferredModelLocked →
// updateConfigLocked applies the in-memory mutation without re-acquiring
// the lock. Without this separation, updateConfig would deadlock on the
// Lock call because the calling goroutine already holds publishMu.
func (s *ConfigStore) updateConfigLocked(mutate func(cfgCopy *Config)) {
	cur := s.loadSnapshot()
	next := cur.clone()

	var cfgCopy Config
	if cur.config != nil {
		cfgCopy = *cur.config
	}
	mutate(&cfgCopy)
	next.config = &cfgCopy

	s.publishLocked(next)
}

// UpdatePreferredModel updates the preferred model for the given type and
// persists it to the config file at the given scope.
func (s *ConfigStore) UpdatePreferredModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	return s.UpdatePreferredModels(scope, map[SelectedModelType]SelectedModel{modelType: model})
}

// UpdatePreferredModels updates and persists multiple model slots (e.g.
// large/small/worker/reviewer) in a single write via SetConfigFields, so
// callers that need to set several slots at once (like `crush models use`)
// get one atomic on-disk write instead of one write per slot. Callers are
// responsible for validating every entry in models BEFORE calling this —
// this function assumes all inputs are already valid and only performs
// writes; it does not partially apply on error, but nor does it need to,
// since validation is expected to have already happened.
func (s *ConfigStore) UpdatePreferredModels(scope Scope, models map[SelectedModelType]SelectedModel) error {
	if len(models) == 0 {
		return nil
	}
	fields := make(map[string]any, len(models))
	for modelType, model := range models {
		fields[fmt.Sprintf("models.%s", modelType)] = model
	}
	if err := s.SetConfigFields(scope, fields); err != nil {
		return fmt.Errorf("failed to update preferred models: %w", err)
	}
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.Models = maps.Clone(cfgCopy.Models)
		if cfgCopy.Models == nil {
			cfgCopy.Models = make(map[SelectedModelType]SelectedModel, len(models))
		}
		for modelType, model := range models {
			cfgCopy.Models[modelType] = model
		}
	})
	for modelType, model := range models {
		if err := s.recordRecentModel(scope, modelType, model); err != nil {
			return err
		}
	}
	return nil
}

// updatePreferredModelLocked is the re-entrant-safe variant of
// UpdatePreferredModel for callers that already hold publishMu (i.e. Load
// via configureSelectedModels). It uses updateConfigLocked /
// recordRecentModelLocked instead of the lock-taking variants.
func (s *ConfigStore) updatePreferredModelLocked(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	return s.updatePreferredModelsLocked(scope, map[SelectedModelType]SelectedModel{modelType: model})
}

// updatePreferredModelsLocked is the re-entrant-safe variant of
// UpdatePreferredModels for callers that already hold publishMu.
func (s *ConfigStore) updatePreferredModelsLocked(scope Scope, models map[SelectedModelType]SelectedModel) error {
	if len(models) == 0 {
		return nil
	}
	fields := make(map[string]any, len(models))
	for modelType, model := range models {
		fields[fmt.Sprintf("models.%s", modelType)] = model
	}
	if err := s.SetConfigFields(scope, fields); err != nil {
		return fmt.Errorf("failed to update preferred models: %w", err)
	}
	s.updateConfigLocked(func(cfgCopy *Config) {
		cfgCopy.Models = maps.Clone(cfgCopy.Models)
		if cfgCopy.Models == nil {
			cfgCopy.Models = make(map[SelectedModelType]SelectedModel, len(models))
		}
		for modelType, model := range models {
			cfgCopy.Models[modelType] = model
		}
	})
	for modelType, model := range models {
		if err := s.recordRecentModelLocked(scope, modelType, model); err != nil {
			return err
		}
	}
	return nil
}

// SetSelectedModelRuntime overrides a single model slot (large/small/
// worker/reviewer) in memory ONLY — no disk write, no autoReload, no
// recent-models bookkeeping. It exists for callers that need a
// process-lifetime override rather than a persisted preference, e.g.
// `crush run --model=...`/--small-model=...`, which temporarily swaps the
// active model for one non-interactive invocation and must NOT leave that
// override sitting in crush.json for the next run to inherit.
//
// Before this method existed, that one-shot CLI override went through
// app.config.Config().Models[...] = ... — mutating the map returned by
// Config() directly from a different package, bypassing ConfigStore
// entirely and racing any concurrent reader of the same map. Now it goes
// through the same copy-on-write path (updateConfig) as every other
// mutator, just without the SetConfigFields disk round-trip.
func (s *ConfigStore) SetSelectedModelRuntime(modelType SelectedModelType, model SelectedModel) {
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.Models = maps.Clone(cfgCopy.Models)
		if cfgCopy.Models == nil {
			cfgCopy.Models = make(map[SelectedModelType]SelectedModel, 1)
		}
		cfgCopy.Models[modelType] = model
	})
}

// UpdateAgentAllowedTools replaces the given agent's AllowedTools in memory
// ONLY (not persisted to disk), via the same copy-on-write publish path as
// every other in-memory-only mutator (see SetSelectedModelRuntime,
// SetProviderRuntimeConfig). The change is published as a new generation
// instead of mutating whatever *Config a concurrent reader currently holds
// via Config() in place.
//
// It exists for callers like app.disableToolsInConfig (`crush run`'s
// sub-agent ban / smart+worker bypass), which used to do
// cfg := app.config.Config(); cfg.Agents[id] = agent — writing straight into
// the map backing the currently-published snapshot. Config() is documented
// as read-only after load; any reader that captured a *Config pointer before
// that write (e.g. a concurrent goroutine mid-turn) would see the mutated
// AllowedTools retroactively, and any reader that captures one after would
// see it too even though no new generation was actually published. Routing
// through updateConfig instead clones Agents (a map, so updateConfig's
// shallow top-level *Config copy alone does not protect it — mutate is
// responsible for cloning nested maps, see updateConfig's doc comment)
// before writing the single entry, so old snapshots are left untouched.
func (s *ConfigStore) UpdateAgentAllowedTools(agentID string, tools []string) {
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.Agents = maps.Clone(cfgCopy.Agents)
		agent := cfgCopy.Agents[agentID]
		agent.AllowedTools = tools
		cfgCopy.Agents[agentID] = agent
	})
}

// SetCompactMode sets the compact mode setting and persists it.
func (s *ConfigStore) SetCompactMode(scope Scope, enabled bool) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		tuiCopy := cloneTUIOptions(optsCopy.TUI)
		tuiCopy.CompactMode = enabled
		optsCopy.TUI = tuiCopy
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.tui.compact_mode", enabled)
}

// SetTransparentBackground sets the transparent background setting and persists it.
func (s *ConfigStore) SetTransparentBackground(scope Scope, enabled bool) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		tuiCopy := cloneTUIOptions(optsCopy.TUI)
		tuiCopy.Transparent = &enabled
		optsCopy.TUI = tuiCopy
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.tui.transparent", enabled)
}

// cloneOptions returns a fresh *Options copy suitable for copy-on-write
// mutation: a nil input yields a fresh zero-value Options (matching the
// historical "if s.config.Options == nil { s.config.Options = &Options{} }"
// lazy-init behaviour), and a non-nil input is shallow-copied so the
// caller can freely reassign its own fields (like TUI) without touching
// the Options struct any other snapshot might still be reading.
func cloneOptions(o *Options) *Options {
	if o == nil {
		return &Options{}
	}
	c := *o
	return &c
}

// cloneTUIOptions mirrors cloneOptions for the nested *TUIOptions pointer.
func cloneTUIOptions(t *TUIOptions) *TUIOptions {
	if t == nil {
		return &TUIOptions{}
	}
	c := *t
	return &c
}

// SetProviderAPIKey sets the API key for a provider and persists it.
func (s *ConfigStore) SetProviderAPIKey(scope Scope, providerID string, apiKey any) error {
	var providerConfig ProviderConfig
	var exists bool
	var setKeyOrToken func()

	switch v := apiKey.(type) {
	case string:
		if err := s.SetConfigField(scope, fmt.Sprintf("providers.%s.api_key", providerID), v); err != nil {
			return fmt.Errorf("failed to save api key to config file: %w", err)
		}
		setKeyOrToken = func() { providerConfig.APIKey = v }
	case *oauth.Token:
		if err := s.SetConfigFields(scope, map[string]any{
			fmt.Sprintf("providers.%s.api_key", providerID): v.AccessToken,
			fmt.Sprintf("providers.%s.oauth", providerID):   v,
		}); err != nil {
			return err
		}
		setKeyOrToken = func() {
			providerConfig.APIKey = v.AccessToken
			providerConfig.OAuthToken = v
			switch providerID {
			case string(catwalk.InferenceProviderCopilot):
				providerConfig.SetupGitHubCopilot()
			}
		}
	}

	// Take publishMu around the in-memory provider update so no concurrent
	// reload can swap the Providers *csync.Map between loadSnapshot() and
	// .Set(). Each reload creates a BRAND NEW *csync.Map (confirmed by
	// reading the load chain: loadFromBytes → json.Unmarshal →
	// Map.UnmarshalJSON allocates a fresh inner map, and setDefaults
	// creates a fresh NewMap if unmarshal left it nil — the old map is
	// never carried forward into the new snapshot). Without publishMu, a
	// concurrent reload could publish a new snapshot between our
	// loadSnapshot() and .Set(), leaving our write in an already-orphaned
	// map invisible to new readers. The disk write above already persists
	// the key, so a subsequent reload picks it up from disk regardless;
	// publishMu just closes the narrow window where the in-memory update
	// is silently lost before that reload runs.
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	sn := s.loadSnapshot()
	providerConfig, exists = sn.config.Providers.Get(providerID)
	if exists {
		setKeyOrToken()
		sn.config.Providers.Set(providerID, providerConfig)
		return nil
	}

	var foundProvider *catwalk.Provider
	for _, p := range sn.knownProviders {
		if string(p.ID) == providerID {
			foundProvider = &p
			break
		}
	}

	if foundProvider != nil {
		providerConfig = ProviderConfig{
			ID:           providerID,
			Name:         foundProvider.Name,
			BaseURL:      foundProvider.APIEndpoint,
			Type:         foundProvider.Type,
			Disable:      false,
			ExtraHeaders: make(map[string]string),
			ExtraParams:  make(map[string]string),
			Models:       foundProvider.Models,
		}
		setKeyOrToken()
	} else {
		return fmt.Errorf("provider with ID %s not found in known providers", providerID)
	}
	sn.config.Providers.Set(providerID, providerConfig)
	return nil
}

// SetProviderRuntimeConfig updates a provider's config and increments the
// generation, publishing a new snapshot. It is for in-memory-only provider
// updates that are NOT persisted to disk — e.g. re-resolving an API key
// template after a 401 error in the coordinator. A subsequent reload will
// rebuild Providers from disk (re-resolving the template), discarding this
// change by design.
//
// publishMu is held so no concurrent reload can swap the Providers
// *csync.Map between loadSnapshot() and .Set(). Each reload creates a brand
// new *csync.Map (confirmed: loadFromBytes → json.Unmarshal calls
// Map.UnmarshalJSON which allocates a fresh inner map; setDefaults creates a
// fresh NewMap if unmarshal left it nil), so without this lock the .Set()
// could land in an already-orphaned map that no reader sees.
//
// Unlike the old implementation that mutated in-place without incrementing
// generation, this now publishes a new snapshot with an incremented generation,
// ensuring the cache key contract is honored (task #341, P1-3).
func (s *ConfigStore) SetProviderRuntimeConfig(providerID string, pc ProviderConfig) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	cur := s.loadSnapshot()
	next := cur.clone()
	next.config.Providers.Set(providerID, pc)
	s.publishLocked(next)
}

// copilotRefreshTokenFn and hyperExchangeTokenFn indirect the two external
// OAuth refresh calls used by RefreshOAuthToken below. They default to the
// real network-calling implementations; tests override them (package-private,
// restored via t.Cleanup) to simulate a slow refresh call — e.g. one that
// blocks until a concurrent ReloadFromDisk has published a new generation —
// without making a real network call or depending on hyper.BaseURL()'s
// process-wide sync.OnceValue memoization (see the caveat on
// TestProviders_ConcurrentErrorCollection_NotLost in provider_test.go for why
// that value cannot be safely redirected per-test).
var (
	copilotRefreshTokenFn = copilot.RefreshToken
	hyperExchangeTokenFn  = hyper.ExchangeToken
)

// RefreshOAuthToken refreshes the OAuth token for the given provider.
// Before making an external refresh request, it checks the config file on
// disk to see if another Crush session has already refreshed the token. If
// a newer, unexpired token is found, it is used instead of refreshing. If
// the exchange fails (e.g. because another session already rotated the
// refresh token), the disk is re-checked to recover the other session's
// token.
func (s *ConfigStore) RefreshOAuthToken(ctx context.Context, scope Scope, providerID string) error {
	providers := s.loadSnapshot().config.Providers
	providerConfig, exists := providers.Get(providerID)
	if !exists {
		return fmt.Errorf("provider %s not found", providerID)
	}

	if providerConfig.OAuthToken == nil {
		return fmt.Errorf("provider %s does not have an OAuth token", providerID)
	}

	// Check if another session refreshed the token recently by reading
	// the current token from the config file on disk.
	newToken, err := s.loadTokenFromDisk(scope, providerID)
	if err != nil {
		slog.Warn("Failed to read token from config file, proceeding with refresh", "provider", providerID, "error", err)
	} else if newToken != nil && !newToken.IsExpired() && newToken.AccessToken != providerConfig.OAuthToken.AccessToken {
		slog.Info("Using token refreshed by another session", "provider", providerID)
		return s.applyToken(providerConfig, newToken, providerID)
	}

	var refreshedToken *oauth.Token
	var refreshErr error
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		refreshedToken, refreshErr = copilotRefreshTokenFn(ctx, providerConfig.OAuthToken.RefreshToken)
	case hyperp.Name:
		refreshedToken, refreshErr = hyperExchangeTokenFn(ctx, providerConfig.OAuthToken.RefreshToken)
	default:
		return fmt.Errorf("OAuth refresh not supported for provider %s", providerID)
	}
	if refreshErr != nil {
		// The exchange may have failed because another session already
		// rotated the refresh token. Re-read the config file and use the
		// other session's token if available.
		if diskToken, diskErr := s.loadTokenFromDisk(scope, providerID); diskErr == nil &&
			diskToken != nil &&
			!diskToken.IsExpired() &&
			diskToken.AccessToken != providerConfig.OAuthToken.AccessToken {
			slog.Info("Using token refreshed by another session after exchange failure", "provider", providerID)
			return s.applyToken(providerConfig, diskToken, providerID)
		}
		return fmt.Errorf("failed to refresh OAuth token for provider %s: %w", providerID, refreshErr)
	}

	slog.Info("Successfully refreshed OAuth token", "provider", providerID)
	providerConfig.OAuthToken = refreshedToken
	providerConfig.APIKey = refreshedToken.AccessToken

	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		providerConfig.SetupGitHubCopilot()
	}

	// Use SetProviderRuntimeConfig to publish a new snapshot with incremented
	// generation, ensuring cache invalidation works correctly (task #341, P1-3).
	s.SetProviderRuntimeConfig(providerID, providerConfig)

	if err := s.SetConfigFields(scope, map[string]any{
		fmt.Sprintf("providers.%s.api_key", providerID): refreshedToken.AccessToken,
		fmt.Sprintf("providers.%s.oauth", providerID):   refreshedToken,
	}); err != nil {
		return fmt.Errorf("failed to persist refreshed token: %w", err)
	}

	return nil
}

// applyToken updates the in-memory provider config with the given token
// and publishes a new snapshot with incremented generation (task #341, P1-3).
func (s *ConfigStore) applyToken(providerConfig ProviderConfig, token *oauth.Token, providerID string) error {
	providerConfig.OAuthToken = token
	providerConfig.APIKey = token.AccessToken
	if providerID == string(catwalk.InferenceProviderCopilot) {
		providerConfig.SetupGitHubCopilot()
	}
	s.SetProviderRuntimeConfig(providerID, providerConfig)
	return nil
}

// loadTokenFromDisk reads the OAuth token for the given provider from the
// config file on disk. Returns nil if the token is not found or matches the
// current in-memory token.
func (s *ConfigStore) loadTokenFromDisk(scope Scope, providerID string) (*oauth.Token, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	oauthKey := fmt.Sprintf("providers.%s.oauth", providerID)
	oauthResult := gjson.Get(string(data), oauthKey)
	if !oauthResult.Exists() {
		return nil, nil
	}

	var token oauth.Token
	if err := json.Unmarshal([]byte(oauthResult.Raw), &token); err != nil {
		return nil, err
	}

	if token.AccessToken == "" {
		return nil, nil
	}

	return &token, nil
}

// recordRecentModel records a model in the recent models list.
func (s *ConfigStore) recordRecentModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	if model.Provider == "" || model.Model == "" {
		return nil
	}

	eq := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}

	entry := SelectedModel{
		Provider: model.Provider,
		Model:    model.Model,
	}

	current := s.loadSnapshot().config.RecentModels[modelType]
	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return eq(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModelsPerType {
		updated = updated[:maxRecentModelsPerType]
	}

	if slices.EqualFunc(current, updated, eq) {
		return nil
	}

	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.RecentModels = maps.Clone(cfgCopy.RecentModels)
		if cfgCopy.RecentModels == nil {
			cfgCopy.RecentModels = make(map[SelectedModelType][]SelectedModel)
		}
		cfgCopy.RecentModels[modelType] = updated
	})

	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), updated); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}

	return nil
}

// recordRecentModelLocked is the re-entrant-safe variant of
// recordRecentModel for callers that already hold publishMu. It uses
// updateConfigLocked instead of updateConfig.
func (s *ConfigStore) recordRecentModelLocked(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	if model.Provider == "" || model.Model == "" {
		return nil
	}

	eq := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}

	entry := SelectedModel{
		Provider: model.Provider,
		Model:    model.Model,
	}

	current := s.loadSnapshot().config.RecentModels[modelType]
	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return eq(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModelsPerType {
		updated = updated[:maxRecentModelsPerType]
	}

	if slices.EqualFunc(current, updated, eq) {
		return nil
	}

	s.updateConfigLocked(func(cfgCopy *Config) {
		cfgCopy.RecentModels = maps.Clone(cfgCopy.RecentModels)
		if cfgCopy.RecentModels == nil {
			cfgCopy.RecentModels = make(map[SelectedModelType][]SelectedModel)
		}
		cfgCopy.RecentModels[modelType] = updated
	})

	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), updated); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}

	return nil
}

// NewTestStore creates a ConfigStore for testing purposes.
func NewTestStore(cfg *Config, loadedPaths ...string) *ConfigStore {
	s := &ConfigStore{}
	s.snap.Store(&storeSnapshot{
		config:      cfg,
		loadedPaths: loadedPaths,
	})
	return s
}

// testStoreOpts configures the fields newTestConfigStore should set on the
// snapshot/store it builds. Only used by this package's own white-box
// tests, which used to build ConfigStore{config: ..., globalDataPath: ...}
// literals directly — that stopped compiling once config/workspacePath
// moved behind the snap atomic.Pointer, so tests now go through this
// helper instead.
type testStoreOpts struct {
	config         *Config
	globalDataPath string
	workspacePath  string
	resolver       VariableResolver
	loadedPaths    []string
}

// newTestConfigStore builds a *ConfigStore for this package's white-box
// tests from the given options, publishing them as a single snapshot the
// same way production code does. globalDataPath is stored directly on the
// ConfigStore (it's the one config-related field that isn't part of the
// snapshot), everything else goes into the initial storeSnapshot.
func newTestConfigStore(opts testStoreOpts) *ConfigStore {
	s := &ConfigStore{globalDataPath: opts.globalDataPath}
	s.snap.Store(&storeSnapshot{
		config:        opts.config,
		workspacePath: opts.workspacePath,
		resolver:      opts.resolver,
		loadedPaths:   opts.loadedPaths,
	})
	return s
}

// ImportCopilot attempts to import a GitHub Copilot token from disk.
func (s *ConfigStore) ImportCopilot() (*oauth.Token, bool) {
	if s.HasConfigField(ScopeGlobal, "providers.copilot.api_key") || s.HasConfigField(ScopeGlobal, "providers.copilot.oauth") {
		return nil, false
	}

	diskToken, hasDiskToken := copilot.RefreshTokenFromDisk()
	if !hasDiskToken {
		return nil, false
	}

	slog.Info("Found existing GitHub Copilot token on disk. Authenticating...")
	refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, err := copilot.RefreshToken(refreshCtx, diskToken)
	if err != nil {
		slog.Error("Unable to import GitHub Copilot token", "error", err)
		return nil, false
	}

	if err := s.SetProviderAPIKey(ScopeGlobal, string(catwalk.InferenceProviderCopilot), token); err != nil {
		return token, false
	}

	if err := s.SetConfigFields(ScopeGlobal, map[string]any{
		"providers.copilot.api_key": token.AccessToken,
		"providers.copilot.oauth":   token,
	}); err != nil {
		slog.Error("Unable to save GitHub Copilot token to disk", "error", err)
	}

	slog.Info("GitHub Copilot successfully imported")
	return token, true
}

// SetTheme sets the TUI theme and persists it.
func (s *ConfigStore) SetTheme(scope Scope, theme string) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		tuiCopy := cloneTUIOptions(optsCopy.TUI)
		tuiCopy.Theme = theme
		optsCopy.TUI = tuiCopy
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.tui.theme", theme)
}

// SetKeepAliveEnabled persists the WebAudio keep-alive preference.
// Persisted as a literal bool (NOT *bool) so the JSON form is
// `"keep_alive_enabled": true|false` — the in-memory Options carries a
// *bool only to distinguish "not set, use default ON" from an explicit
// choice, and SetConfigField writes the underlying primitive.
func (s *ConfigStore) SetKeepAliveEnabled(scope Scope, enabled bool) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		v := enabled
		optsCopy.KeepAliveEnabled = &v
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.keep_alive_enabled", enabled)
}

// RemoveProviderAPIKey removes the API key for the given provider from disk and
// removes it from the in-memory enabled providers list. Providers is a
// *csync.Map (its own internal RWMutex), so Del is safe to call directly.
func (s *ConfigStore) RemoveProviderAPIKey(scope Scope, providerID string) error {
	if err := s.RemoveConfigField(scope, fmt.Sprintf("providers.%s.api_key", providerID)); err != nil {
		return fmt.Errorf("failed to remove provider API key: %w", err)
	}
	s.loadSnapshot().config.Providers.Del(providerID)
	return nil
}

// RecordRecentModel records the given model as recently used and persists to disk.
func (s *ConfigStore) RecordRecentModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	return s.recordRecentModel(scope, modelType, model)
}

// RemoveRecentModel removes a model from the recent list and persists to disk.
func (s *ConfigStore) RemoveRecentModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	current := s.loadSnapshot().config.RecentModels[modelType]
	if current == nil {
		return nil
	}
	updated := slices.DeleteFunc(slices.Clone(current), func(m SelectedModel) bool {
		return m.Provider == model.Provider && m.Model == model.Model
	})
	if len(updated) == len(current) {
		return nil
	}
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.RecentModels = maps.Clone(cfgCopy.RecentModels)
		cfgCopy.RecentModels[modelType] = updated
	})
	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), updated); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}
	return nil
}

// LogPath returns the path to the log file.
func (s *ConfigStore) LogPath() string {
	opts := s.loadSnapshot().config.Options
	if opts == nil || opts.DataDirectory == "" {
		return ""
	}
	return filepath.Join(opts.DataDirectory, "logs", "crush.log")
}

// StalenessResult contains the result of a staleness check.
type StalenessResult struct {
	Dirty   bool
	Changed []string
	Missing []string
	Errors  map[string]error
}

// ConfigStaleness checks whether any tracked config files have changed on disk
// since the last snapshot.
func (s *ConfigStore) ConfigStaleness() StalenessResult {
	var result StalenessResult
	result.Errors = make(map[string]error)

	sn := s.loadSnapshot()

	for _, path := range sn.trackedConfigPaths {
		snapshot, hadSnapshot := sn.snapshots[path]

		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		if err != nil && !os.IsNotExist(err) {
			result.Errors[path] = err
			result.Dirty = true
		}

		if !exists {
			if hadSnapshot && snapshot.Exists {
				result.Missing = append(result.Missing, path)
				result.Dirty = true
			}
			continue
		}

		if !hadSnapshot || !snapshot.Exists {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
			continue
		}

		if snapshot.Size != info.Size() || snapshot.ModTime != info.ModTime().UnixNano() {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
		}
	}

	slices.Sort(result.Changed)
	slices.Sort(result.Missing)

	return result
}

// RefreshStalenessSnapshot captures fresh snapshots of all tracked config
// files and publishes them as part of a new store generation. It only
// touches the snapshots/trackedConfigPaths pair — config, resolver,
// knownProviders etc. are carried over unchanged from whatever generation
// is current at the time of the swap.
func (s *ConfigStore) RefreshStalenessSnapshot() error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.refreshStalenessSnapshotLocked()
	return nil
}

// refreshStalenessSnapshotLocked is the lock-free body of
// RefreshStalenessSnapshot. The caller MUST hold publishMu.
func (s *ConfigStore) refreshStalenessSnapshotLocked() {
	cur := s.loadSnapshot()
	next := cur.clone()
	if next.snapshots == nil {
		next.snapshots = make(map[string]fileSnapshot)
	} else {
		next.snapshots = maps.Clone(next.snapshots)
	}

	for _, path := range next.trackedConfigPaths {
		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		snapshot := fileSnapshot{
			Path:   path,
			Exists: exists,
		}

		if exists {
			snapshot.Size = info.Size()
			snapshot.ModTime = info.ModTime().UnixNano()
		}

		next.snapshots[path] = snapshot
	}

	s.publishLocked(next)
}

// CaptureStalenessSnapshot recomputes the set of tracked config paths (the
// paths passed in, plus the store's own workspace/global paths) and
// refreshes their on-disk snapshots, publishing both as part of a single
// new generation.
func (s *ConfigStore) CaptureStalenessSnapshot(paths []string) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.captureStalenessSnapshotLocked(paths)
}

// captureStalenessSnapshotLocked does the work of CaptureStalenessSnapshot
// without taking publishMu. The caller MUST hold publishMu.
func (s *ConfigStore) captureStalenessSnapshotLocked(paths []string) {
	seen := make(map[string]struct{})
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		seen[abs] = struct{}{}
	}

	cur := s.loadSnapshot()
	workspacePath := cur.workspacePath
	if workspacePath != "" {
		abs, err := filepath.Abs(workspacePath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}
	if s.globalDataPath != "" {
		abs, err := filepath.Abs(s.globalDataPath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}

	tracked := make([]string, 0, len(seen))
	for p := range seen {
		tracked = append(tracked, p)
	}
	slices.Sort(tracked)

	next := cur.clone()
	next.trackedConfigPaths = tracked
	s.publishLocked(next)

	s.refreshStalenessSnapshotLocked()
}

// captureStalenessSnapshot is the lock-acquiring entry point for callers
// that do NOT already hold publishMu (primarily white-box tests). Production
// code paths that already hold publishMu (Load, reloadFromDiskLocked) call
// captureStalenessSnapshotLocked directly to avoid a re-entrant deadlock.
func (s *ConfigStore) captureStalenessSnapshot(paths []string) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.captureStalenessSnapshotLocked(paths)
}

// ReloadFromDisk re-runs the config load/merge flow and updates the
// in-memory config atomically.
//
// The heavy candidate-build work (disk I/O, JSON parsing, and — the
// expensive part — configureProviders' shell-substitution ResolveValue
// calls for every provider's API key/headers, each bounded by
// resolveTimeout) deliberately runs WITHOUT holding publishMu; see
// reloadFromDiskUnlocked. A single hung "$(...)" in a config value used to
// hold publishMu for the resolver's full timeout, which serialises every
// reader/writer of the store — including unrelated runtime mutators like
// SetSkipPermissionRequests — behind it. Only the final generation-check
// and pointer swap take publishMu now, for a duration bounded by memory
// operations, not shell subprocess I/O.
func (s *ConfigStore) ReloadFromDisk(ctx context.Context) error {
	if s.workingDir == "" {
		return fmt.Errorf("cannot reload: working directory not set")
	}
	return s.reloadFromDiskUnlocked(ctx)
}

// reloadFromDiskUnlocked builds a full candidate storeSnapshot from local
// variables only — no store field is touched until the very end — then
// publishes it under a short publishMu critical section.
//
// Two locks are involved, never held simultaneously by the same call:
//
//   - reloadMu serialises the candidate-build phase against other
//     concurrent reload attempts (autoReload's TryLock on reloadMu skips a
//     redundant reload the same way it used to skip on publishMu). This
//     is purely about not wasting work building N candidates in parallel
//     when one would do — it is NOT required for correctness, since the
//     publish step below is itself safe against concurrent publishers.
//   - publishMu guards only the generation-check + swap at the end. Its
//     hold time is now bounded by a handful of map/pointer operations,
//     never by config-file I/O or shell subprocess execution.
//
// Base generation vs CAS semantics: the candidate is built starting from
// `prev`, the snapshot published at the time the build started. If some
// other writer (a copy-on-write mutator, e.g. SetSkipPermissionRequests)
// publishes a newer generation while the build is still in flight, this
// reload's candidate is stale only with respect to the fields it carries
// FORWARD from prev unchanged (overrides, trackedConfigPaths, snapshots)
// — cfg/resolver/knownProviders/loadedPaths/workspacePath are always
// freshly built from disk regardless of what changed concurrently in
// memory, so they can never regress. Rather than discarding a fully-built
// candidate (expensive: it already paid the shell-resolution cost) or
// silently overwriting the concurrent writer's change, the publish step
// re-reads the CURRENT snapshot under publishMu and rebases just those
// forwarded fields onto it before storing — the writer's change survives,
// and the reload's fresh disk state still wins for everything reload
// itself is authoritative over. This mirrors the reasoning already
// applied to staleness tracking (CaptureStalenessSnapshot) for the same
// class of "small piece of forwarded state, rebase onto latest" problem.
func (s *ConfigStore) reloadFromDiskUnlocked(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.buildAndPublishReload(ctx)
}

// buildAndPublishReload is reloadFromDiskUnlocked's body, factored out so
// autoReload can call it directly after its own TryLock(reloadMu) without
// double-locking reloadMu (sync.Mutex is not reentrant).
func (s *ConfigStore) buildAndPublishReload(ctx context.Context) error {
	configPaths := lookupConfigs(s.workingDir)
	cfg, loadedPaths, err := loadFromConfigPaths(configPaths)
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// prev is read WITHOUT publishMu: it is only used to seed defaults
	// (dataDir) and as the starting point for the rebase-on-publish
	// below. Reading it unlocked is safe because storeSnapshot is
	// immutable once published — loadSnapshot() always returns either
	// this value or a strictly newer one, never a torn/partial view.
	prev := s.loadSnapshot()

	var dataDir string
	if prev.config != nil && prev.config.Options != nil {
		dataDir = prev.config.Options.DataDirectory
	}
	cfg.setDefaults(s.workingDir, dataDir)

	workspacePath := filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName))
	if wsData, err := os.ReadFile(workspacePath); err == nil && len(wsData) > 0 {
		if !json.Valid(wsData) {
			return fmt.Errorf("invalid JSON in config file %s", workspacePath)
		}
		merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, wsData))
		if mergeErr == nil {
			dataDir := cfg.Options.DataDirectory
			*cfg = *merged
			cfg.setDefaults(s.workingDir, dataDir)
			loadedPaths = append(loadedPaths, workspacePath)
		}
	}

	if err := cfg.ValidateHooks(); err != nil {
		return fmt.Errorf("invalid hook configuration on reload: %w", err)
	}

	// env/resolver are built fresh from the real process environment on
	// every reload. ctx is now threaded all the way into ResolveValue
	// (via configureProviders → resolver.ResolveValue), so a caller that
	// cancels ctx (e.g. app shutdown) can abort an in-flight shell
	// substitution instead of waiting out the full resolveTimeout.
	baseEnv := env.New()
	resolver := NewShellVariableResolver(baseEnv, WithContext(ctx))
	providers, err := Providers(cfg)
	if err != nil {
		return fmt.Errorf("failed to load providers during reload: %w", err)
	}

	// This is the expensive step this refactor is about: configureProviders
	// resolves every provider's API key/headers (and MCP discovery),
	// each ResolveValue call bounded by resolveTimeout. It runs under
	// reloadMu (serialising it against other reload attempts) but NOT
	// publishMu — a hung shell substitution here no longer blocks
	// SetSkipPermissionRequests/SetProviderRuntimeConfig/other readers.
	if err := cfg.configureProviders(ctx, s, baseEnv, resolver, providers); err != nil {
		return fmt.Errorf("failed to configure providers during reload: %w", err)
	}

	if !cfg.IsConfigured() {
		slog.Warn("No providers configured after reload")
	} else {
		// persist=false: reloadFromDiskUnlocked runs without publishMu
		// held, so configureSelectedModels must NOT take the Locked
		// (reentrant-only) path here — there is no reentrant lock to
		// avoid re-acquiring. Only Load (which does hold publishMu for
		// its whole body) passes persist=true.
		if err := configureSelectedModels(s, cfg, providers, false); err != nil {
			return fmt.Errorf("failed to configure selected models during reload: %w", err)
		}
		cfg.SetupAgents()
	}

	// Every fallible step has succeeded. Build the candidate snapshot from
	// what this build started with (prev) — see the rebase step below for
	// why this is safe even if a concurrent writer published a newer
	// generation while the above ran unlocked.
	candidate := &storeSnapshot{
		config:             cfg,
		resolver:           resolver,
		knownProviders:     providers,
		loadedPaths:        loadedPaths,
		trackedConfigPaths: prev.trackedConfigPaths,
		snapshots:          prev.snapshots,
		workspacePath:      workspacePath,
		overrides:          prev.overrides,
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	// Rebase: if the currently-published snapshot is no longer `prev`
	// (some copy-on-write mutator published in the meantime), carry its
	// overrides/staleness-tracking forward onto the candidate instead of
	// the ones captured before the unlocked build — otherwise this
	// publish would silently revert that concurrent change. cfg/resolver/
	// knownProviders/loadedPaths/workspacePath are NOT rebased: they are
	// reload's own authoritative fresh-from-disk output regardless of
	// what changed concurrently in memory.
	cur := s.loadSnapshot()
	if cur.generation != prev.generation {
		candidate.overrides = cur.overrides
		candidate.trackedConfigPaths = cur.trackedConfigPaths
		candidate.snapshots = cur.snapshots
	}

	s.publishLocked(candidate)

	// Caller (this function) already holds publishMu — use the Locked
	// variant to avoid a re-entrant deadlock.
	s.captureStalenessSnapshotLocked(loadedPaths)

	return nil
}

func (s *ConfigStore) autoReload(ctx context.Context) error {
	if s.workingDir == "" {
		return nil // Expected skip: working directory not set.
	}
	// Skip if a reload is already in progress. This covers both concurrent
	// auto-reloads after parallel writes and the re-entrant call that
	// configureProviders could in principle trigger mid-build. reloadMu
	// (not publishMu) is the guard now: the candidate-build phase this
	// dedups against no longer holds publishMu at all, so TryLock on
	// publishMu would no longer detect "a reload is already in progress"
	// for most of that phase.
	//
	// Note: a write that completes after the in-progress reload has already
	// read the config file won't be reflected in memory until the next
	// reload. That's acceptable — writes are rare and the next user action
	// or file-watch tick picks it up. Callers needing guaranteed freshness
	// after a write should call ReloadFromDisk explicitly.
	if !s.reloadMu.TryLock() {
		return nil
	}
	defer s.reloadMu.Unlock()
	return s.buildAndPublishReload(ctx)
}
