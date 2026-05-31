package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ForbiddenWritesEnv is the env var orchestrators set to blacklist
// specific paths from the write / edit / multiedit tools. Comma-separated
// list of absolute or working-dir-relative paths. Designed to be set by
// a parent process before exec'ing `crush run`, so a wrapper like
// `crush run --json > /tmp/out.json` can guarantee the model can't
// collide with its own stdout-redirect target — the failure mode where
// the JSON envelope ended up on line 1 and write-tool content on lines
// 2+ in the same file, observed in the shamir-db .tmp-audit-D.json case.
const ForbiddenWritesEnv = "CRUSH_FORBID_WRITES"

// CheckForbiddenWrite returns an error if path is on the comma-separated
// CRUSH_FORBID_WRITES env-var blacklist. Comparison uses absolute, cleaned
// paths; case-insensitive on Windows. Empty env var means no
// restrictions.
func CheckForbiddenWrite(path string) error {
	raw := os.Getenv(ForbiddenWritesEnv)
	if raw == "" {
		return nil
	}
	target := normalizeForbiddenPath(path)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if normalizeForbiddenPath(entry) == target {
			return fmt.Errorf(
				"write to %q forbidden by %s (this path is on the orchestrator's blacklist — typically a shell stdout-redirect target). Return the content as your tool output or in final_text, or write to a different path",
				path, ForbiddenWritesEnv,
			)
		}
	}
	return nil
}

func normalizeForbiddenPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		return strings.ToLower(abs)
	}
	return abs
}
