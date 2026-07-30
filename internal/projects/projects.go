package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/session"
)

const projectsFileName = "projects.json"

// registerLockTimeout caps how long Register waits for the inter-process
// sidecar lock on projects.json before failing. Mirrors configWriteLockTimeout
// in internal/config/store.go (30s), which uses the same
// session.AcquireFileLockContext primitive for the same class of
// cross-process read-modify-write: a wedged sibling `crush run` process
// (debugger attached, suspended shell, frozen network mount) must not
// indefinitely freeze every other parallel `crush` invocation's startup.
const registerLockTimeout = 30 * time.Second

// Project represents a tracked project directory.
type Project struct {
	Path         string    `json:"path"`
	DataDir      string    `json:"data_dir"`
	LastAccessed time.Time `json:"last_accessed"`
}

// ProjectList holds the list of tracked projects.
type ProjectList struct {
	Projects []Project `json:"projects"`
}

// mu serialises concurrent goroutines within THIS process (mirrors
// ConfigStore.diskWriteMu). It does NOT by itself protect against two
// separate `crush` processes racing to read-modify-write projects.json —
// that cross-process gap is closed by the OS-level file lock acquired in
// Register (see session.AcquireFileLockContext and
// ConfigStore.withConfigWriteLockCtx's doc comment for the same pattern).
var mu sync.Mutex

// projectsFilePath returns the path to the projects.json file.
func projectsFilePath() string {
	return filepath.Join(filepath.Dir(config.GlobalConfigData()), projectsFileName)
}

// Load reads the projects list from disk.
func Load() (*ProjectList, error) {
	mu.Lock()
	defer mu.Unlock()
	return loadLocked()
}

// loadLocked is Load's implementation for callers that already hold mu
// (i.e. Register, via its own read-modify-write cycle). Callers outside
// this file MUST use Load instead.
func loadLocked() (*ProjectList, error) {
	path := projectsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectList{Projects: []Project{}}, nil
		}
		return nil, err
	}

	var list ProjectList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}

	return &list, nil
}

// Save writes the projects list to disk atomically (write to a temp file in
// the same directory, then rename over the destination) so a crash mid-write
// cannot leave a truncated/corrupt projects.json behind — a truncated file
// would fail json.Unmarshal on every subsequent Load/List/Register call in
// every future crush process, not just the one that crashed.
func Save(list *ProjectList) error {
	mu.Lock()
	defer mu.Unlock()
	return saveLocked(list)
}

// saveLocked is Save's implementation for callers that already hold mu
// (i.e. Register). Callers outside this file MUST use Save instead.
func saveLocked(list *ProjectList) error {
	path := projectsFilePath()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	return fsext.AtomicWriteFile(path, data, 0o600)
}

// Register adds or updates a project in the list.
//
// The whole load -> mutate -> save cycle runs under both the in-process mu
// (serialising goroutines within this process) and an inter-process OS-level
// file lock on a ".lock" sidecar next to projects.json (serialising separate
// `crush` processes, acquired via session.AcquireFileLockContext — the same
// primitive ConfigStore.withConfigWriteLockCtx uses for crush.json). Register
// runs on the startup path of every `crush` process, so N parallel `crush
// run` invocations racing to register their own project is the normal case,
// not a hypothetical: without the file lock, two concurrent Register calls
// (in one process or across processes) could each Load the same pre-write
// list, append/update only their own entry, and the second Save would
// silently discard the first writer's project.
//
// The inter-process lock wait happens BEFORE mu is taken, not while holding
// it — mirroring the fix in internal/config/store.go's
// withConfigWriteLockCtx (lock ordering: diskWriteMu -> file lock there;
// here it's file lock -> mu, since mu only needs to guard the actual disk
// I/O, not the cross-process wait). This avoids the regression that
// motivated H-2 (task #153): holding an in-process lock for the full
// duration of a potentially-long cross-process wait would stall every other
// in-process caller (e.g. concurrent List() calls) for as long as a wedged
// sibling process holds the file lock.
func Register(workingDir, dataDir string) error {
	path := projectsFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), registerLockTimeout)
	defer cancel()

	lock, err := session.AcquireFileLockContext(ctx, path+".lock")
	if err != nil {
		return fmt.Errorf("failed to lock projects file %q: %w", path, err)
	}
	// Deliberately NOT removing the sidecar after Release: see
	// ConfigStore.withConfigWriteLockCtx's doc comment in
	// internal/config/store.go for why a remove-after-release races a
	// concurrent process reopening the same path (harmless on POSIX,
	// unsafe on Windows without FILE_SHARE_DELETE).
	defer lock.Release()

	mu.Lock()
	defer mu.Unlock()

	list, err := loadLocked()
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	// Check if project already exists
	found := false
	for i, p := range list.Projects {
		if p.Path == workingDir {
			list.Projects[i].DataDir = dataDir
			list.Projects[i].LastAccessed = now
			found = true
			break
		}
	}

	if !found {
		list.Projects = append(list.Projects, Project{
			Path:         workingDir,
			DataDir:      dataDir,
			LastAccessed: now,
		})
	}

	// Sort by last accessed (most recent first)
	slices.SortFunc(list.Projects, func(a, b Project) int {
		if a.LastAccessed.After(b.LastAccessed) {
			return -1
		}
		if a.LastAccessed.Before(b.LastAccessed) {
			return 1
		}
		return 0
	})

	return saveLocked(list)
}

// List returns all tracked projects sorted by last accessed.
func List() ([]Project, error) {
	list, err := Load()
	if err != nil {
		return nil, err
	}
	return list.Projects, nil
}
