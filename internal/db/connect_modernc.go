//go:build (darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64))

package db

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

func openDB(dbPath string) (*sql.DB, error) {
	// Set pragmas for better performance via _pragma query params.
	// Format: _pragma=name(value)
	params := url.Values{}
	for name, value := range pragmas {
		params.Add("_pragma", fmt.Sprintf("%s(%s)", name, value))
	}
	// Use BEGIN IMMEDIATE so writers acquire the reserved lock up front,
	// preventing deferred-to-writer upgrade deadlocks.
	params.Set("_txlock", "immediate")

	dsn := fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	return db, nil
}

// openReadDB opens a read-only connection pool against the same on-disk
// file as openDB. mode=ro asks SQLite to refuse writes outright (fails
// fast with SQLITE_READONLY rather than silently taking the writer's
// place); _txlock=deferred is the read-only-appropriate counterpart to the
// writer's _txlock=immediate — a reader has no reserved lock to acquire up
// front. Unlike the writer, this pool is intentionally left at the
// database/sql default MaxOpenConns (0 = unlimited): WAL mode is designed
// for exactly this — many concurrent readers against one snapshot without
// blocking the single writer connection or each other.
func openReadDB(dbPath string) (*sql.DB, error) {
	params := url.Values{}
	for name, value := range pragmas {
		if name == "journal_mode" {
			// journal_mode is a file-wide property already set by the
			// writer; modernc's sqlite driver rejects _pragma=journal_mode
			// on a mode=ro connection (it would require a write), so skip
			// it here rather than fail every read-only connection open.
			continue
		}
		params.Add("_pragma", fmt.Sprintf("%s(%s)", name, value))
	}
	params.Set("_txlock", "deferred")
	params.Set("mode", "ro")

	dsn := fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open read-only database: %w", err)
	}

	return db, nil
}
