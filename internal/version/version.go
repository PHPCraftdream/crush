package version

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
)

// Build-time parameters set via -ldflags.

var (
	Version = "devel"
	Commit  = "unknown"
	// BuildID is a unique identifier for this build. For release builds it
	// equals Commit; for development builds (go run / go build without
	// ldflags) it is derived from the executable's modification time, which
	// changes on every recompilation.
	//
	// Fork merge note (origin/main 2026-05-16): upstream introduced this in
	// 9e126c27 to detect stale REST servers during development. We keep it
	// because the same problem applies to our WebSocket server: when the dev
	// loop rebuilds the binary, the browser tab may still be talking to the
	// previous process. BuildID gives the WUI a cheap freshness signal.
	BuildID = ""
)

// FullVersion is consumed by the web UI's status bar.
//
// Fork merge note: upstream removed BuildTime in favour of BuildID. We keep
// the parenthesised-suffix shape that the WUI already renders and just feed
// it the new value when available.
func FullVersion() string {
	if BuildID != "" && BuildID != "unknown" {
		return fmt.Sprintf("%s (%s)", Version, BuildID)
	}
	return Version
}

// A user may install crush using `go install github.com/charmbracelet/crush@latest`.
// without -ldflags, in which case the version above is unset. As a workaround
// we use the embedded build version that *is* set when using `go install` (and
// is only set for `go install` and not for `go build`).
func init() {
	info, ok := debug.ReadBuildInfo()
	if ok {
		mainVersion := info.Main.Version
		if mainVersion != "" && mainVersion != "(devel)" {
			Version = mainVersion
		}
	}

	// Derive BuildID when not set via ldflags.
	if BuildID == "" {
		BuildID = deriveBuildID()
	}
}

// deriveBuildID uses the running executable's modification time as a unique
// build fingerprint. This changes on every recompilation (including `go run`),
// making it reliable for detecting stale servers during development.
func deriveBuildID() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	return strconv.FormatInt(fi.ModTime().UnixNano(), 36)
}
