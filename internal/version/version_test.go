package version

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveDevVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		revision string
		modified string
		want     string
	}{
		{
			name:     "no vcs info keeps default",
			revision: "",
			modified: "",
			want:     "",
		},
		{
			name:     "clean commit is shortened",
			revision: "06c807842604abcdef1234567890abcdef123456",
			modified: "false",
			want:     "devel-06c8078",
		},
		{
			name:     "dirty commit appends marker",
			revision: "06c807842604abcdef1234567890abcdef123456",
			modified: "true",
			want:     "devel-06c8078-dirty",
		},
		{
			name:     "short revision is not truncated further",
			revision: "abc1234",
			modified: "",
			want:     "devel-abc1234",
		},
		{
			name:     "modified true without explicit clean flag still marks dirty",
			revision: "deadbeef",
			modified: "true",
			want:     "devel-deadbee-dirty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, deriveDevVersion(tt.revision, tt.modified))
		})
	}
}

func TestReadVCS(t *testing.T) {
	t.Parallel()
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "fullcommitabcd"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "GOOS", Value: "linux"},
		},
	}
	v := readVCS(info)
	require.Equal(t, "fullcommitabcd", v.revision)
	require.Equal(t, "true", v.modified)
}

func TestReadVCS_Empty(t *testing.T) {
	t.Parallel()
	// A build with no embedded VCS metadata (e.g. built outside a checkout)
	// yields a zero value rather than garbage.
	require.Equal(t, vcsInfo{}, readVCS(&debug.BuildInfo{}))
}

func TestResolveVersion(t *testing.T) {
	t.Parallel()
	// buildInfo builds a *debug.BuildInfo for a binary produced by `go build .`
	// from a checkout: Main.Version is "(devel)" and the toolchain embeds VCS
	// settings (revision + modified flag).
	buildInfo := func(revision, modified string) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: revision},
				{Key: "vcs.modified", Value: modified},
			},
		}
	}
	tests := []struct {
		name          string
		defaultVer    string // ldflags-injected Version (or "devel")
		defaultCommit string // ldflags-injected Commit (or "unknown")
		info          *debug.BuildInfo
		wantVersion   string
		wantCommit    string
	}{
		{
			// The release/packaged build path: ldflags injects "v0.1.0". It
			// MUST survive — init() must never clobber it, even though VCS
			// metadata is present and Main.Version is "(devel)".
			name:       "ldflags-injected version is preserved (npm/packaged)",
			defaultVer: "v0.1.0", defaultCommit: "unknown",
			info:        buildInfo("06c807842604", "false"),
			wantVersion: "v0.1.0",
			wantCommit:  "06c807842604", // populated from VCS (was "unknown")
		},
		{
			name:       "ldflags-injected version and commit both preserved",
			defaultVer: "v1.2.3", defaultCommit: "deadbeef",
			info:        buildInfo("abcdef123456", "true"),
			wantVersion: "v1.2.3",
			wantCommit:  "deadbeef", // not "unknown", so VCS does not override
		},
		{
			name:       "ldflags-injected version wins over module version",
			defaultVer: "v1.2.3", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main:     debug.Module{Version: "v0.2.0"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef123456"}},
			},
			wantVersion: "v1.2.3",
			wantCommit:  "abcdef123456",
		},
		{
			// `go install pkg@version` resolves a real module version that wins
			// over the "devel" default.
			name:       "go install main version wins",
			defaultVer: "devel", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main:     debug.Module{Version: "v0.2.0"},
				Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "false"}},
			},
			wantVersion: "v0.2.0",
			wantCommit:  "unknown",
		},
		{
			// Local checkout builds can expose a v0.0.0 pseudo-version through
			// BuildInfo. That is not a package/release version and was the source
			// of packaged binaries reporting v0.0.0-...+dirty, so fall through to
			// the clearer dev format.
			name:       "local pseudo module version is treated as dev",
			defaultVer: "devel", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.0-20260705112643-90c57af7ca7a+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "90c57af7ca7a5c50d2ee959eaa1668120b4b1729"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			wantVersion: "devel-90c57af-dirty",
			wantCommit:  "90c57af7ca7a5c50d2ee959eaa1668120b4b1729",
		},
		{
			// Local dev build: ldflags not used, so derive from VCS.
			name:       "dev build derives clean version from vcs",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        buildInfo("06c807842604", "false"),
			wantVersion: "devel-06c8078",
			wantCommit:  "06c807842604",
		},
		{
			name:       "dev build with dirty tree appends marker",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        buildInfo("06c807842604", "true"),
			wantVersion: "devel-06c8078-dirty",
			wantCommit:  "06c807842604",
		},
		{
			name:       "dev build without vcs metadata keeps devel default",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			wantVersion: "devel",
			wantCommit:  "unknown",
		},
		{
			name:       "nil build info keeps defaults",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        nil,
			wantVersion: "devel",
			wantCommit:  "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotVersion, gotCommit := resolveVersion(tt.defaultVer, tt.defaultCommit, tt.info)
			require.Equal(t, tt.wantVersion, gotVersion, "version")
			require.Equal(t, tt.wantCommit, gotCommit, "commit")
		})
	}
}

func TestUsableModuleVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{version: "", want: false},
		{version: "(devel)", want: false},
		{version: "v0.0.0-20260705112643-90c57af7ca7a", want: false},
		{version: "v0.0.0-20260705112643-90c57af7ca7a+dirty", want: false},
		{version: "0.0.0-20260705112643-90c57af7ca7a", want: false},
		{version: "v0.2.0", want: true},
		{version: "v0.2.1-rc.1", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, usableModuleVersion(tt.version))
		})
	}
}

func TestFormatFullVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version string
		buildID string
		want    string
	}{
		{name: "release version with build id", version: "v0.1.0", buildID: "abc123", want: "v0.1.0 (abc123)"},
		{name: "dev version with build id", version: "devel-06c8078", buildID: "xyz", want: "devel-06c8078 (xyz)"},
		{name: "no build id omits suffix", version: "v0.1.0", buildID: "", want: "v0.1.0"},
		{name: "unknown build id omits suffix", version: "v0.1.0", buildID: "unknown", want: "v0.1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, formatFullVersion(tt.version, tt.buildID))
		})
	}
}
