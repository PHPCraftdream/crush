package version

import (
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// forkBaseVersion is the fork's current release-line version, mirrored by hand
// from the "version" field in npm/crush/package.json. It is embedded into local
// dev-build version strings so the operator can see at a glance which release
// line a devel binary was built from. This fork bumps versions deliberately and
// manually (see CLAUDE.md at the repo root), so this constant must be kept in
// lockstep with npm/crush/package.json on every bump.
const forkBaseVersion = "0.1.5"

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

// Fork patch: this init() and its helpers (resolveVersion,
// usableModuleVersion, extractBaseTag, readVCS, deriveDevVersion) diverge from
// upstream. Upstream unconditionally overwrote Version with info.Main.Version;
// the fork makes an ldflags-injected Version authoritative (release/npm builds
// MUST win — see the "Verify" step in
// .github/workflows/publish-fork-npm.yml) and only derives a value from
// build metadata for un-injected local builds.
//
// A user may install crush using `go install github.com/charmbracelet/crush@latest`
// without -ldflags, in which case the version above is unset. As a workaround
// we use the embedded build version that *is* set when using `go install` (and
// is only set for `go install` and not for `go build`). For plain `go build`
// from a checkout, that main version is "(devel)", so we additionally derive a
// meaningful version from the VCS metadata the toolchain embeds
// (vcs.revision) — this lets two local dev builds be told apart. When the
// `go install` main version is a pseudo-version built on top of a real
// upstream tag (e.g. "v0.72.1-0.<timestamp>-<commit>"), that base tag is
// extracted and prepended to the derived string, so a go-install build
// reports e.g. "v0.72.1-<hash>-<forkBaseVersion>" rather than the raw ugly
// pseudo-version. Plain `go build .` has no base tag (its main version is
// "(devel)") and reports the tag-less "<hash>-<forkBaseVersion>". Neither
// path ever includes a "devel" or dirty marker in the output. Release/
// packaged builds inject Version via ldflags and are left untouched.
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
//     it is a clean release version (not a pseudo-version and not "(devel)"),
//     wins directly;
//   - otherwise a VCS-derived "[<baseTag>-]<commit>-<forkBaseVersion>" string
//     for local dev builds (no "devel" marker). The optional <baseTag> prefix
//     is the upstream release tag extracted from a `go install` pseudo-version
//     (e.g. "v0.72.1" from "v0.72.1-0.<timestamp>-<commit>"); plain
//     `go build .` has no such tag and produces the bare
//     "<commit>-<forkBaseVersion>".
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
		baseTag := ""
		if info.Main.Version != "(devel)" {
			baseTag = extractBaseTag(info.Main.Version)
		}
		if dv := deriveDevVersion(baseTag, vcs.revision); dv != "" {
			version = dv
		}
	}
	return version, commit
}

// pseudoVersionSuffixRe matches the Go-toolchain pseudo-version suffix built on
// top of a real prior tag, e.g. the "-0.20260628185628-e47711a0e3e4" part of
// "v0.72.1-0.20260628185628-e47711a0e3e4" (optionally followed by "+dirty").
// Such a string is an ugly pseudo-version; extractBaseTag uses this regex to
// peel the suffix off and recover the underlying real base tag ("v0.72.1"),
// while usableModuleVersion uses it to reject the raw pseudo-version from being
// shown directly.
var pseudoVersionSuffixRe = regexp.MustCompile(`-0\.\d{14}-[0-9a-f]{12}(\+dirty)?$`)

// extractBaseTag recovers the upstream base tag embedded inside a Go
// pseudo-version. For a `go install pkg@version` build, info.Main.Version is a
// pseudo-version of the form "<baseTag>-0.<14-digit-timestamp>-<12-hex-commit>
// [+dirty]", e.g. "v0.72.1-0.20260628185628-e47711a0e3e4". This returns the
// "<baseTag>" portion ("v0.72.1"); it returns "" when v is not a
// pseudo-version or has no base tag to recover (e.g. plain "(devel)" from
// `go build .`, or a bare "v0.0.0-..." local-build pseudo-version whose base
// tag is meaningless).
func extractBaseTag(v string) string {
	loc := pseudoVersionSuffixRe.FindStringIndex(v)
	if loc == nil {
		return ""
	}
	base := v[:loc[0]]
	// "v0.0.0"/"0.0.0" is the placeholder base Go uses when there is no real
	// prior tag; it carries no meaningful information, so treat it as absent.
	if base == "v0.0.0" || base == "0.0.0" {
		return ""
	}
	return base
}

// usableModuleVersion reports whether a BuildInfo main-module version is
// meaningful enough to expose directly. Local checkout builds can report
// v0.0.0-<timestamp>-<commit>[+dirty], which is a Go pseudo-version, not a
// release version users can match to a package. Those fall through to the
// VCS-derived <commit>-<forkBaseVersion> format instead. A pseudo-version
// built on top of a real prior tag (e.g. "v0.72.1-0.<timestamp>-<commit>[+dirty]")
// is rejected here too: its base tag is recovered separately by extractBaseTag
// and prepended to the derived string, but the raw pseudo-version itself is
// never shown.
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
	if pseudoVersionSuffixRe.MatchString(v) {
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
// from embedded VCS metadata, embedding the fork's current release-line version
// (forkBaseVersion), e.g. "06c8078-0.1.5" for a clean checkout. When baseTag is
// non-empty (recovered from a `go install` pseudo-version), it is prepended as
// "<baseTag>-", yielding e.g. "v0.72.1-06c8078-0.1.5". It returns an empty
// string when no revision is available, signalling the caller to keep the
// plain "devel" default. No "devel" marker and no dirty marker are ever
// included in the returned string — the commit hash + forkBaseVersion (and,
// when available, the upstream base tag) are the only content.
func deriveDevVersion(baseTag, revision string) string {
	if revision == "" {
		return ""
	}
	short := revision
	if len(short) > 7 {
		short = short[:7]
	}
	v := short + "-" + forkBaseVersion
	if baseTag != "" {
		v = baseTag + "-" + v
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
