package version

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Build-time parameters set via -ldflags. These act as overrides: when a
// release/packaging build injects them (see .goreleaser.yml and the
// publish-fork-npm workflow), the values below are replaced and treated as
// authoritative. When they are left at their defaults (local `go build` /
// `make build` / `go run`), init() fills in meaningful values from the build
// metadata embedded by the Go toolchain.

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
	return formatFullVersion(Version, BuildID)
}

// formatFullVersion is the pure formatter behind [FullVersion], split out so
// it can be unit-tested without touching package-level state.
func formatFullVersion(v, buildID string) string {
	if buildID != "" && buildID != "unknown" {
		return fmt.Sprintf("%s (%s)", v, buildID)
	}
	return v
}

// A user may install crush using `go install github.com/charmbracelet/crush@latest`
// without -ldflags, in which case the version above is unset. As a workaround
// we use the embedded build version that *is* set when using `go install` (and
// is only set for `go install` and not for `go build`). For plain `go build`
// from a checkout, that main version is "(devel)", so we additionally derive a
// meaningful version from the VCS metadata the toolchain embeds
// (vcs.revision / vcs.modified) — this lets two local dev builds be told apart
// and surfaces a "-dirty" marker when the working tree had uncommitted
// changes. Release/packaged builds inject Version via ldflags and are left
// untouched.
func init() {
	info, _ := debug.ReadBuildInfo()
	Version, Commit = resolveVersion(Version, Commit, info)
	if BuildID == "" {
		BuildID = deriveBuildID()
	}
}

// resolveVersion decides the final Version and Commit from the ldflags-provided
// defaults together with the build metadata embedded by the Go toolchain. It is
// pure and unit-testable; init() is a thin wrapper around it.
//
// Precedence for Version:
//   - an ldflags-injected value (defaultVersion != "devel") always wins — this
//     is the release/packaged-build path and MUST NOT be clobbered here. The
//     npm publish workflow additionally verifies each built binary reports this
//     value (see .github/workflows/publish-fork-npm.yml "Verify" step);
//   - otherwise the module version resolved by `go install pkg@version`, when
//     it is not the placeholder pseudo-version Go emits for dirty local builds;
//   - otherwise a VCS-derived "devel-<commit>[-dirty]" for local dev builds.
//
// Commit is filled from VCS only when the ldflags default is still "unknown".
func resolveVersion(defaultVersion, defaultCommit string, info *debug.BuildInfo) (version, commit string) {
	version, commit = defaultVersion, defaultCommit
	if info == nil {
		return version, commit
	}
	if version == "devel" && usableModuleVersion(info.Main.Version) {
		mv := info.Main.Version
		version = mv
	}
	vcs := readVCS(info)
	if commit == "unknown" && vcs.revision != "" {
		commit = vcs.revision
	}
	if version == "devel" {
		if dv := deriveDevVersion(vcs.revision, vcs.modified); dv != "" {
			version = dv
		}
	}
	return version, commit
}

// usableModuleVersion reports whether a BuildInfo main-module version is
// meaningful enough to expose directly. Local checkout builds can report
// v0.0.0-<timestamp>-<commit>[+dirty], which is a Go pseudo-version, not a
// release version users can match to a package. Those fall through to the
// VCS-derived devel-<commit>[-dirty] format instead.
func usableModuleVersion(v string) bool {
	if v == "" || v == "(devel)" {
		return false
	}
	if strings.HasPrefix(v, "v0.0.0-") || strings.HasPrefix(v, "0.0.0-") {
		return false
	}
	if strings.Contains(v, "+dirty") {
		return false
	}
	return true
}

// vcsInfo holds the subset of [debug.BuildInfo] settings that describe the
// source control state the binary was built from.
type vcsInfo struct {
	revision string // "vcs.revision": full commit hash
	modified string // "vcs.modified": "true", "false", or empty
}

// readVCS extracts VCS settings from a build info record. These entries are
// embedded automatically by the Go toolchain (Go 1.18+) when building from a
// VCS checkout.
func readVCS(info *debug.BuildInfo) vcsInfo {
	var v vcsInfo
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			v.revision = s.Value
		case "vcs.modified":
			v.modified = s.Value
		}
	}
	return v
}

// deriveDevVersion builds a human-meaningful version for a development build
// from embedded VCS metadata, e.g. "devel-06c8078" or "devel-06c8078-dirty".
// It returns an empty string when no revision is available, signalling the
// caller to keep the plain "devel" default.
func deriveDevVersion(revision, modified string) string {
	if revision == "" {
		return ""
	}
	short := revision
	if len(short) > 7 {
		short = short[:7]
	}
	v := "devel-" + short
	if modified == "true" {
		v += "-dirty"
	}
	return v
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
