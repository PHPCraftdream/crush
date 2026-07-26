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
const forkBaseVersion = "0.1.7"

// UpstreamTriagedVersion is the highest charmbracelet/crush release whose
// commits have been triaged into this fork — every commit up to that tag has
// been ported, evaluated, or explicitly skipped with a recorded reason (see
// the merge workflow and the SKIP-note convention in CLAUDE.md, plus the
// per-batch plans under docs/plans/).
//
// It is NOT a claim that the fork contains upstream's code up to that tag —
// the fork diverges heavily and most upstream commits are deliberately
// skipped. It answers a different question: how far has anyone actually
// LOOKED at upstream. That is the number that goes stale silently, so it is
// surfaced in `crush --version` output rather than living only in a plan
// document.
//
// Bumped by hand at the end of an upstream triage pass, never automatically.
const UpstreamTriagedVersion = "v0.87.0"

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
// usableModuleVersion, readVCS, deriveDevVersion) diverge from upstream.
// Upstream unconditionally overwrote Version with info.Main.Version; the fork
// makes an ldflags-injected Version authoritative (release/npm builds MUST
// win — see the "Verify" step in .github/workflows/publish-fork-npm.yml) and
// only derives a value from build metadata for un-injected local builds.
//
// A user may install crush using `go install github.com/charmbracelet/crush@latest`
// without -ldflags, in which case the version above is unset. As a workaround
// we use the embedded build version that *is* set when using `go install` (and
// is only set for `go install` and not for `go build`). For plain `go build`
// from a checkout, that main version may still be "(devel)" or a pseudo-
// version depending on the toolchain, so we additionally derive a meaningful
// version from the VCS metadata the toolchain embeds (vcs.revision) — this
// lets two local dev builds be told apart. The derived string is always the
// bare "<hash>-<forkBaseVersion>" (e.g. "141ac19-0.1.7"): no upstream-tag-
// shaped prefix is ever recovered or prepended here, even when
// info.Main.Version happens to be a pseudo-version built on top of a real
// upstream tag. That upstream signal is deliberately surfaced elsewhere, as
// the hand-maintained UpstreamTriagedVersion appended by root.go's Execute()
// — showing two differently-sourced "upstream version" numbers in one line
// (one incidental, one deliberate) is more confusing than showing only the
// deliberate one. Neither path ever includes a "devel" or dirty marker in the
// output. Release/packaged builds inject Version via ldflags and are left
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
//     it is a clean release version (not a pseudo-version and not "(devel)"),
//     wins directly;
//   - otherwise a VCS-derived "<commit>-<forkBaseVersion>" string for local dev
//     builds (no "devel" marker, no upstream-tag-shaped prefix — see
//     deriveDevVersion).
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
		if dv := deriveDevVersion(vcs.revision); dv != "" {
			version = dv
		}
	}
	return version, commit
}

// pseudoVersionSuffixRe matches the Go-toolchain pseudo-version suffix built on
// top of a real prior tag, e.g. the "-0.20260628185628-e47711a0e3e4" part of
// "v0.72.1-0.20260628185628-e47711a0e3e4" (optionally followed by "+dirty").
// usableModuleVersion uses it to reject the raw pseudo-version from being
// shown directly.
var pseudoVersionSuffixRe = regexp.MustCompile(`-0\.\d{14}-[0-9a-f]{12}(\+dirty)?$`)

// usableModuleVersion reports whether a BuildInfo main-module version is
// meaningful enough to expose directly. Local checkout builds can report
// v0.0.0-<timestamp>-<commit>[+dirty], which is a Go pseudo-version, not a
// release version users can match to a package. Those fall through to the
// VCS-derived <commit>-<forkBaseVersion> format instead. A pseudo-version
// built on top of a real prior tag (e.g. "v0.72.1-0.<timestamp>-<commit>[+dirty]")
// is rejected here too — it is just as unhelpful as the v0.0.0 case, and its
// base tag is deliberately NOT recovered or shown (see deriveDevVersion).
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
// from embedded VCS metadata, embedding the fork's current release-line
// version (forkBaseVersion), e.g. "06c8078-0.1.7" for a clean checkout. It
// returns an empty string when no revision is available, signalling the
// caller to keep the plain "devel" default. No "devel" marker and no dirty
// marker are ever included in the returned string — the commit hash +
// forkBaseVersion are the only content.
//
// This deliberately never prepends an upstream-tag-shaped prefix, even when
// one could be recovered from a Go pseudo-version (e.g. "v0.72.1" from
// "v0.72.1-0.<timestamp>-<commit>"). That recovery previously existed
// (extractBaseTag, removed) on the assumption that a plain `go build .`
// always produces info.Main.Version == "(devel)" with no base tag at all —
// but that assumption does not hold for every Go toolchain: a local `go
// build .` can embed a real pseudo-version with a recoverable base tag,
// which produced a confusing "v0.72.1-<hash>-0.1.7" that looked like it
// carried a deliberate upstream-tracking signal but didn't. That role is
// now filled by the deliberately hand-maintained UpstreamTriagedVersion
// (appended separately by root.go's Execute()), which is the one and only
// upstream-version signal shown to the user.
func deriveDevVersion(revision string) string {
	if revision == "" {
		return ""
	}
	short := revision
	if len(short) > 7 {
		short = short[:7]
	}
	return short + "-" + forkBaseVersion
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
