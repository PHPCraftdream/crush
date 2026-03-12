package version

import (
	"fmt"
	"runtime/debug"
)

// Build-time parameters set via -ldflags
var (
	Version  = "devel"
	BuildTime = "unknown"
)

// FullVersion returns version with build time
func FullVersion() string {
	if BuildTime != "unknown" {
		return fmt.Sprintf("%s (%s)", Version, BuildTime)
	}
	return Version
}

// A user may install crush using `go install github.com/charmbracelet/crush@latest`.
// without -ldflags, in which case the version above is unset. As a workaround
// we use the embedded build version that *is* set when using `go install` (and
// is only set for `go install` and not for `go build`).
func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	mainVersion := info.Main.Version
	if mainVersion != "" && mainVersion != "(devel)" {
		Version = mainVersion
	}
}
