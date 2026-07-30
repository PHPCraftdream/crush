//go:build !((darwin && (amd64 || arm64)) || (freebsd && (amd64 || arm64)) || (linux && (386 || amd64 || arm || arm64 || loong64 || ppc64le || riscv64 || s390x)) || (windows && (386 || amd64 || arm64)))

package db

import (
	"database/sql"
	"fmt"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
)

func openDB(dbPath string) (*sql.DB, error) {
	// Use BEGIN IMMEDIATE so writers acquire the reserved lock up front,
	// preventing deferred-to-writer upgrade deadlocks. The "file:" prefix
	// is required for the ncruces driver to parse query parameters.
	dsn := fmt.Sprintf("file:%s?_txlock=immediate", dbPath)
	db, err := driver.Open(dsn, func(c *sqlite3.Conn) error {
		// Set pragmas for better performance via _pragma query params.
		// Format: PRAGMA name = value;
		for name, value := range pragmas {
			if err := c.Exec(fmt.Sprintf("PRAGMA %s = %s;", name, value)); err != nil {
				return fmt.Errorf("failed to set pragma %q: %w", name, err)
			}
		}
		return nil
	})
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
	dsn := fmt.Sprintf("file:%s?_txlock=deferred&mode=ro", dbPath)
	db, err := driver.Open(dsn, func(c *sqlite3.Conn) error {
		for name, value := range pragmas {
			if name == "journal_mode" {
				// journal_mode is a file-wide property already set by the
				// writer; setting it again on a read-only connection would
				// require a write the mode=ro connection can't perform.
				continue
			}
			if err := c.Exec(fmt.Sprintf("PRAGMA %s = %s;", name, value)); err != nil {
				return fmt.Errorf("failed to set pragma %q: %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open read-only database: %w", err)
	}

	return db, nil
}
