package config

import (
	"path/filepath"
	"testing"
)

// isolateAllGlobalConfigPaths redirects every environment variable that
// GlobalConfig()/GlobalConfigData() (and the stdlib/XDG fallbacks they defer
// to) can resolve through, so a test can never read or merge the operator's
// real ~/.config/crush/crush.json (live API keys, MCP server definitions)
// nor the real global data-dir crush.json.
//
// It deliberately points CRUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME and
// CRUSH_GLOBAL_DATA/XDG_DATA_HOME at two DIFFERENT subdirectories of t.TempDir
// rather than the same one. lookupConfigs (see load.go) loads both
// GlobalConfig() and GlobalConfigData() and merges them via go-jsons, which
// merges (rather than replaces) array-valued fields on collision — pointing
// both env vars at the identical directory makes lookupConfigs load and
// merge that directory's crush.json with itself. That's harmless for a
// config with no array fields, but is a latent bug (doubled array entries,
// doubled ConfigStore.LoadedPaths() entries) for any test that adds one, so
// this helper keeps the two paths distinct unconditionally.
//
// Returns the two directories in case a caller needs to seed a crush.json
// in one of them (configDir for CRUSH_GLOBAL_CONFIG, dataDir for
// CRUSH_GLOBAL_DATA).
func isolateAllGlobalConfigPaths(t *testing.T) (configDir, dataDir string) {
	t.Helper()

	tmp := t.TempDir()
	configDir = filepath.Join(tmp, "global-config")
	dataDir = filepath.Join(tmp, "global-data")

	// GlobalConfig()/GlobalConfigData() check the CRUSH_GLOBAL_* vars first;
	// XDG_CONFIG_HOME/XDG_DATA_HOME are their fallback when CRUSH_GLOBAL_*
	// is unset, and home.Config()/home.Dir() (real $HOME/%USERPROFILE%) are
	// the fallback below that. Setting all four here means this isolation
	// holds regardless of which of those resolution layers a given code
	// path happens to consult.
	t.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("CRUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	return configDir, dataDir
}
