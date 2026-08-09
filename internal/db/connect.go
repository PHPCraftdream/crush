package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
)

var (
	pragmas = map[string]string{
		"foreign_keys":  "ON",
		"journal_mode":  "WAL",
		"page_size":     "4096",
		"temp_store":    "MEMORY",
		"cache_size":    "-8000",
		"synchronous":   "NORMAL",
		"secure_delete": "ON",
		"busy_timeout":  "30000",
	}
	gooseInitOnce sync.Once
	gooseInitErr  error
)

//go:embed migrations/*.sql
var FS embed.FS

func init() {
	goose.SetBaseFS(FS)

	if testing.Testing() {
		goose.SetLogger(goose.NopLogger())
	}
}

// connEntry holds a shared database connection pair and its reference
// count. db is the single-connection writer (SetMaxOpenConns(1) — see the
// comment in Connect for why). readDB is a separate, read-only pool that
// WAL allows to run concurrently with the writer without touching the
// corruption-prone write path; it is only ever nil-safe-equal to db itself
// when opening a genuinely separate read-only handle isn't possible (see
// ConnectRead). Both handles share one refCount and are opened/closed
// together so Connect/ConnectRead/Release callers don't have to reason
// about two independent lifetimes.
type connEntry struct {
	db       *sql.DB
	readDB   *sql.DB
	refCount int
}

var (
	pool   = make(map[string]*connEntry)
	poolMu sync.Mutex
)

// Connect opens a SQLite database connection for the given data
// directory and runs migrations. If a connection to the same database
// file already exists, the existing connection is returned with its
// reference count incremented. Callers must pair each Connect with a
// [Release] when they no longer need the connection.
//
// Connect only ever returns the single-connection WRITER handle. Callers
// that also want a concurrent read-only handle for hot, standalone read
// paths (list/grep/call-tree queries that don't need read-your-own-write
// consistency with a subsequent write in the same call) should additionally
// call [ConnectRead] with the same dataDir — it shares this entry's
// refCount, so a single [Release] tears down both.
func Connect(ctx context.Context, dataDir string) (*sql.DB, error) {
	entry, err := connect(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	return entry.db, nil
}

// ConnectRead opens (or reuses) a read-only connection pool for the same
// database Connect would open, sharing its reference count. WAL mode lets
// readers run fully concurrently with the single writer connection instead
// of queuing behind it — this is the point of keeping the two separate.
//
// dataDir must have already been (or be about to be) passed to Connect;
// calling ConnectRead alone still opens the writer underneath (migrations
// must run before anything reads), it just returns the reader handle. Every
// call increments the shared refCount, so a caller that calls both Connect
// and ConnectRead for the same dataDir must call [Release] the same number
// of times it called either one (once per Connect/ConnectRead pair, not
// once per function).
func ConnectRead(ctx context.Context, dataDir string) (*sql.DB, error) {
	entry, err := connect(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	return entry.readDB, nil
}

// connect is the shared implementation behind Connect and ConnectRead: it
// opens (or reuses) the pool entry for dataDir, running migrations exactly
// once, and increments the entry's refCount once per call — so callers that
// want BOTH a writer and a reader handle for the same dataDir must call
// [Release] twice (once per Connect/ConnectRead call they made), matching
// the existing "one Connect, one Release" contract extended to ConnectRead.
func connect(ctx context.Context, dataDir string) (*connEntry, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data.dir is not set")
	}

	dbPath := filepath.Join(dataDir, "crush.db")

	// Resolve to an absolute path so that different relative paths to
	// the same file share a single connection.
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	poolMu.Lock()
	defer poolMu.Unlock()

	if entry, ok := pool[absPath]; ok {
		entry.refCount++
		return entry, nil
	}

	conn, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	// Serialize all access through a single connection. SQLite
	// serializes writes at the file level anyway, and allowing multiple
	// pool connections to interleave writes/checkpoints (especially
	// under concurrent sub-agents) has caused WAL/header desync
	// resulting in SQLITE_NOTADB (26) on the next open.
	conn.SetMaxOpenConns(1)

	if err = conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := initGoose(); err != nil {
		conn.Close()
		slog.Error("Failed to initialize goose", "error", err)
		return nil, fmt.Errorf("failed to initialize goose: %w", err)
	}

	if err := goose.Up(conn, "migrations"); err != nil {
		conn.Close()
		slog.Error("Failed to apply migrations", "error", err)
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	// Open the read-only pool AFTER migrations have run on the writer, on
	// the same absolute path. WAL mode (already set on the writer above,
	// and inherited file-wide) means these reader connections observe a
	// consistent snapshot without ever blocking on — or being blocked
	// by — the writer's single connection. mode=ro additionally guards
	// against a read path accidentally issuing a write: it will fail
	// fast with SQLITE_READONLY instead of silently taking the writer's
	// place.
	//
	// If the read-only pool fails to open for any reason, we don't fail
	// Connect/ConnectRead over it — we fall back to routing reads through
	// the writer connection (readDB = conn), which is exactly today's
	// behavior. A degraded-to-serialized read path is strictly better
	// than refusing to start.
	readConn, err := openReadDB(dbPath)
	if err != nil {
		slog.Warn("Failed to open read-only pool, reads will serialize with writes", "error", err)
		readConn = conn
	}

	entry := &connEntry{db: conn, readDB: readConn, refCount: 1}
	pool[absPath] = entry
	return entry, nil
}

// Release decrements the reference count for the database at the given
// data directory. When the count reaches zero the underlying connections
// (writer and, if separate, reader) are closed and removed from the pool.
//
// A caller that obtained both a writer (Connect) and a reader (ConnectRead)
// handle for the same dataDir must call Release once per call it made to
// either function — the shared refCount is decremented once per Release,
// symmetric with connect()'s "increment once per call" contract.
func Release(dataDir string) error {
	dbPath := filepath.Join(dataDir, "crush.db")
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	var entryToClose *connEntry
	poolMu.Lock()
	entry, ok := pool[absPath]
	if !ok {
		poolMu.Unlock()
		return nil
	}

	entry.refCount--
	if entry.refCount > 0 {
		poolMu.Unlock()
		return nil
	}

	// refCount reached zero: remove from pool while holding the mutex
	// to prevent concurrent Connect() from finding and incrementing it,
	// then close the actual connections OUTSIDE the mutex to avoid
	// holding poolMu during potentially slow sql.DB.Close() calls.
	delete(pool, absPath)
	entryToClose = entry
	poolMu.Unlock()

	return closeEntry(entryToClose)
}

// closeEntry closes both handles in entry, tolerating readDB == db (the
// fallback path in connect() when the read-only pool failed to open, or in
// principle any future caller that intentionally shares one *sql.DB between
// both roles) by only closing the reader when it is a distinct handle.
func closeEntry(entry *connEntry) error {
	var readErr error
	if entry.readDB != nil && entry.readDB != entry.db {
		readErr = entry.readDB.Close()
	}
	writeErr := entry.db.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// ResetPool closes all pooled connections and clears the pool. This is
// intended for use in tests to ensure a clean state between test cases.
func ResetPool() {
	poolMu.Lock()
	defer poolMu.Unlock()
	for path, entry := range pool {
		closeEntry(entry) //nolint:errcheck
		delete(pool, path)
	}
}

func initGoose() error {
	gooseInitOnce.Do(func() {
		goose.SetBaseFS(FS)
		gooseInitErr = goose.SetDialect("sqlite3")
	})

	return gooseInitErr
}
