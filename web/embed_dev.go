//go:build dev

package web

import "io/fs"

// FS returns nil in dev mode — the server will proxy to the rspack dev server.
func FS() fs.FS { return nil }
