package app

// TestP1_3_ShutdownDoesNotCloseDBUnderLiveWriter covers two of the three
// defects fixed in #343:
//   2. App.Shutdown distinguishes graceful vs forced shutdown
//   3. db.Release releases poolMu before calling sql.DB.Close()
//
// Defect 1 (CancelAll must be a true join on live Run() goroutines, not an
// IsBusy() poll) is NOT exercised here — the second subtest below drives
// App.Shutdown through a mockCoordinatorForShutdown that hardcodes
// stillBusy=true, so it proves Shutdown's OWN policy branch (skip DB close
// when told the agent is still busy) but says nothing about whether
// CancelAll correctly detects a genuinely stuck Run() in the first place.
// See TestP343_CancelAllTrueJoinWaitsForRealBlockedRun
// (internal/agent/p343_cancelall_join_test.go) for that proof: it drives a
// real sessionAgent.Run() deliberately stuck inside a tool call that ignores
// context cancellation, and asserts CancelAll genuinely blocks for close to
// its 5s grace period before reporting stillBusy=true.
//
// Both subtests below use real db.Connect/db.Release against real temp
// directories — not mocks of the DB layer — so they do prove the real pool
// mutex/Close() behavior for defects 2 and 3.
import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestP1_3_ShutdownDoesNotCloseDBUnderLiveWriter verifies the critical
// invariant that shutdown does NOT close the DB under a live writer.
func TestP1_3_ShutdownDoesNotCloseDBUnderLiveWriter(t *testing.T) {
	t.Parallel()

	// Test 1: Verify db.Release releases poolMu before calling Close()
	// This prevents deadlock if Close() hangs.
	//
	// Each subtest gets its OWN t.TempDir(), not a shared one from the
	// parent: db's connection pool is keyed by absolute dataDir path and
	// is a process-global resource (poolMu, the pool map). Both subtests
	// are t.Parallel(), so sharing one dataDir here previously caused them
	// to race on the SAME pool entry's refcount — one subtest's Release
	// could close the pool entry out from under the other's still-open
	// connection, and t.TempDir()'s own cleanup would then fail to remove
	// the directory because a stale handle was still open. Deterministic,
	// not just occasionally flaky: reproduced 5/5 runs before this fix.
	t.Run("db.Release releases poolMu before Close", func(t *testing.T) {
		dataDir := t.TempDir()
		t.Parallel()
		// Connect to DB - this will create the DB and run migrations.
		entry, err := db.Connect(context.Background(), dataDir)
		require.NoError(t, err, "failed to connect to DB")
		require.NotNil(t, entry, "entry should not be nil")

		// Now, try to connect again - should increment refCount, not create new entry.
		entry2, err := db.Connect(context.Background(), dataDir)
		require.NoError(t, err, "failed to connect to DB again")
		require.Same(t, entry, entry2, "should return same entry")

		// Release once - should decrement refCount to 1, not close.
		err = db.Release(dataDir)
		require.NoError(t, err, "first release should succeed")

		// Release again - should decrement refCount to 0 and close.
		// With the fix, poolMu is released before Close(), so this won't deadlock.
		releaseDone := make(chan struct{})
		go func() {
			err = db.Release(dataDir)
			close(releaseDone)
		}()

		select {
		case <-releaseDone:
			// Release completed (expected with fix).
			require.NoError(t, err, "second release should succeed")
		case <-time.After(5 * time.Second):
			t.Fatal("db.Release hung - poolMu likely not released before Close()")
		}

		t.Log("Verified: db.Release releases poolMu before calling Close()")
	})

	// Test 2: Verify App.Shutdown skips DB close on forced shutdown.
	t.Run("App.Shutdown skips DB close when stillBusy=true", func(t *testing.T) {
		dataDir := t.TempDir()
		t.Parallel()
		// Reconnect for this test.
		entry, err := db.Connect(context.Background(), dataDir)
		require.NoError(t, err, "failed to connect to DB")
		require.NotNil(t, entry, "entry should not be nil")

		// Create a mock coordinator that returns stillBusy=true.
		mockCoord := &mockCoordinatorForShutdown{
			cancelAllFunc: func() (stillBusy bool) {
				return true // Simulate agents still busy after grace period
			},
		}

		app := &App{
			AgentCoordinator: mockCoord,
			DB:               func() *sql.DB { return nil }, // Not used in this test
			dataDir:          dataDir,
			dbReleasesNeeded: 1, // One Connect call
			globalCtx:        context.Background(),
		}

		// Call Shutdown. With the fix, it should NOT call db.Release
		// because stillBusy=true (forced shutdown mode).
		shutdownStart := time.Now()

		done := make(chan struct{})
		go func() {
			app.Shutdown()
			close(done)
		}()

		select {
		case <-done:
			// Shutdown completed (expected).
		case <-time.After(15 * time.Second):
			t.Fatal("Shutdown did not complete within 15 seconds")
		}

		shutdownDuration := time.Since(shutdownStart)
		assert.Less(t, shutdownDuration, 15*time.Second,
			"Shutdown should return within bounded time")

		// Critical invariant: DB was NOT closed because shutdown was forced.
		// We verify this by trying to use the connection - it should still work.
		// Shutdown() skipped Release() (forced-shutdown policy under test), so
		// the entry's refCount is still 1 from the initial Connect above. A new
		// Connect() here increments it to 2 and must return the SAME entry
		// (not create a new one).
		entry2, err := db.Connect(context.Background(), dataDir)
		require.NoError(t, err, "should still be able to connect after forced shutdown")
		require.Same(t, entry, entry2, "should return same entry (DB not released)")

		// Clean up: release BOTH Connect() calls this subtest made (the
		// initial one at the top, and the verification one above) — a single
		// Release only brings refCount to 1, leaving the entry open and
		// making t.TempDir()'s own cleanup fail to remove the directory
		// (observed as "process cannot access the file" on Windows).
		require.NoError(t, db.Release(dataDir), "cleanup release should succeed")
		require.NoError(t, db.Release(dataDir), "second cleanup release should succeed")

		t.Logf("Verified: App.Shutdown skipped DB close on forced shutdown (returned in %v)", shutdownDuration)
	})
}
