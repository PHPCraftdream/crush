package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestSetConfigFields_TwoStoresSameFile_BothUpdatesSurvive simulates two
// parallel crush processes writing DIFFERENT keys to the SAME config file at
// the same time. Each ConfigStore owns a private diskWriteMu (just as two
// separate OS processes would), so in-process serialisation cannot help —
// only the inter-process sidecar lock (path+".lock", via session.FileLock)
// can prevent the lost update. session.FileLock is backed by flock/LockFileEx,
// which are per-open-file-description, so two opens inside one test process
// contend on the lock exactly as two real processes would.
//
// Without the sidecar lock (the pre-fix code, which only had diskWriteMu),
// both stores would read the same "{}", each apply only its own key, and the
// second atomicWriteFile rename would erase the first store's key — a silent
// cross-process lost update. Run with -race.
func TestSetConfigFields_TwoStoresSameFile_BothUpdatesSurvive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	// Many iterations widen the window: under the pre-fix code this lost an
	// update on a large fraction of iterations; under the fix it never does.
	const iterations = 100
	for i := range iterations {
		require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

		// Two independent stores sharing the SAME globalDataPath — two
		// separate in-process mutexes, like two crush processes sharing one
		// config file. workingDir is "" so autoReload is a no-op (keeps the
		// test free of provider/network work).
		store1 := newTestConfigStore(testStoreOpts{globalDataPath: configPath})
		store2 := newTestConfigStore(testStoreOpts{globalDataPath: configPath})

		var wg sync.WaitGroup
		start := make(chan struct{})
		var err1, err2 error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			err1 = store1.SetConfigFields(ScopeGlobal, map[string]any{
				"options.tui.theme": "dark",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			err2 = store2.SetConfigFields(ScopeGlobal, map[string]any{
				"options.tui.compact_mode": true,
			})
		}()
		close(start)
		wg.Wait()

		require.NoError(t, err1, "iter %d: store1 write failed", i)
		require.NoError(t, err2, "iter %d: store2 write failed", i)

		data, rerr := os.ReadFile(configPath)
		require.NoError(t, rerr)
		require.True(t, gjson.Get(string(data), "options.tui.theme").Exists(),
			"iter %d: store1's key (theme) was lost — cross-process lost update", i)
		require.True(t, gjson.Get(string(data), "options.tui.compact_mode").Exists(),
			"iter %d: store2's key (compact_mode) was lost — cross-process lost update", i)
	}
}

// TestUnprotectedRMW_DeterministicallyLosesUpdate is the proof that the bug
// class fixed by withConfigWriteLock is real. It reproduces the pre-fix body
// of SetConfigFields MINUS any lock (diskWriteMu is process-private and thus
// irrelevant across processes): two "processes" each read-modify-write the
// same file, synchronised by a barrier so BOTH finish reading BEFORE either
// writes. Each therefore reads "{}" and writes a file containing only its own
// key; the second write overwrites the first — exactly one update survives.
// This is the cross-process lost update that diskWriteMu alone cannot prevent.
func TestUnprotectedRMW_DeterministicallyLosesUpdate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	// Phase 1: both read the file BEFORE any write (barrier). Both see "{}".
	var readA, readB []byte
	var readWg sync.WaitGroup
	readWg.Add(2)
	go func() {
		defer readWg.Done()
		d, err := os.ReadFile(configPath)
		if err != nil || len(d) == 0 {
			d = []byte("{}")
		}
		readA = d
	}()
	go func() {
		defer readWg.Done()
		d, err := os.ReadFile(configPath)
		if err != nil || len(d) == 0 {
			d = []byte("{}")
		}
		readB = d
	}()
	readWg.Wait()

	// Phase 2: both write from their own (stale, identical) read. Each write
	// is a full overwrite, so the second clobbers the first.
	var writeWg sync.WaitGroup
	writeWg.Add(2)
	go func() {
		defer writeWg.Done()
		out, err := sjson.Set(string(readA), "options.tui.theme", "dark")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configPath, []byte(out), 0o600))
	}()
	go func() {
		defer writeWg.Done()
		out, err := sjson.Set(string(readB), "options.tui.compact_mode", true)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configPath, []byte(out), 0o600))
	}()
	writeWg.Wait()

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	themeExists := gjson.Get(string(data), "options.tui.theme").Exists()
	compactExists := gjson.Get(string(data), "options.tui.compact_mode").Exists()
	require.True(t, themeExists || compactExists, "at least one update must survive")
	require.False(t, themeExists && compactExists,
		"both updates survived WITHOUT a lock — the deterministic lost-update reproduction no longer loses, so this proof test is no longer valid")
}
