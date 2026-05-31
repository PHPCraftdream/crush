package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

var defaultEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

type cliEffortSource struct {
	Binary  string
	HelpArg string
	Regex   string
}

var (
	effortCacheMu sync.Mutex
	effortCache   = map[string][]string{}
)

func (s *cliEffortSource) Levels() []string {
	if s == nil {
		return nil
	}
	effortCacheMu.Lock()
	defer effortCacheMu.Unlock()
	if v, ok := effortCache[s.Binary]; ok {
		return v
	}
	out, err := exec.CommandContext(context.Background(), s.Binary, s.HelpArg).CombinedOutput()
	if err != nil {
		slog.Warn("could not detect effort levels — falling back", "binary", s.Binary, "err", err)
		effortCache[s.Binary] = defaultEffortLevels
		return defaultEffortLevels
	}
	re := regexp.MustCompile(s.Regex)
	m := re.FindStringSubmatch(string(out))
	if m == nil {
		slog.Warn("effort regex did not match help output — falling back", "binary", s.Binary)
		effortCache[s.Binary] = defaultEffortLevels
		return defaultEffortLevels
	}
	levels := strings.Split(m[1], ",")
	for i, l := range levels {
		levels[i] = strings.TrimSpace(l)
	}
	if len(levels) == 0 {
		levels = defaultEffortLevels
	}
	effortCache[s.Binary] = levels
	return levels
}

func (s *cliEffortSource) FormatLevels() string {
	levels := s.Levels()
	return strings.Join(levels, "|")
}

var claudeEffortSource = &cliEffortSource{
	Binary:  "claude",
	HelpArg: "--help",
	Regex:   `--effort\s+\S+\s+\S.*?\(([^)]+)\)`,
}

func resetEffortCache() {
	effortCacheMu.Lock()
	defer effortCacheMu.Unlock()
	effortCache = map[string][]string{}
}

// setEffortSourceForTesting replaces the effort source for a given set of
// atoms and returns a restore function. For use in tests only.
func setEffortSourceForTesting(src *cliEffortSource) func() {
	old := claudeEffortSource
	claudeEffortSource = src
	resetEffortCache()
	return func() {
		claudeEffortSource = old
		resetEffortCache()
	}
}

// setMockEffortBinary creates a mock effort source that returns fixed levels
// without shelling out. Returns a restore function.
func setMockEffortLevels(levels []string) func() {
	mock := &cliEffortSource{
		Binary:  "__test__",
		HelpArg: "--help",
		Regex:   `^$`,
	}
	effortCacheMu.Lock()
	effortCache["__test__"] = levels
	effortCacheMu.Unlock()
	old := claudeEffortSource
	claudeEffortSource = mock
	return func() {
		claudeEffortSource = old
		resetEffortCache()
	}
}

// setFallbackEffortSource sets claudeEffortSource to one that will fail
// (binary not found) and fall back to defaults. Returns a restore function.
func setFallbackEffortSource() func() {
	mock := &cliEffortSource{
		Binary:  "__nonexistent_binary_for_test__",
		HelpArg: "--help",
		Regex:   `^$`,
	}
	resetEffortCache()
	old := claudeEffortSource
	claudeEffortSource = mock
	return func() {
		claudeEffortSource = old
		resetEffortCache()
	}
}

// EffortLevels returns the effort levels for the given source.
func effortLevelsFor(src *cliEffortSource) []string {
	if src == nil {
		return nil
	}
	return src.Levels()
}

// FormatEffortLevels returns a human-readable list of effort levels.
func formatEffortLevels(src *cliEffortSource) string {
	if src == nil {
		return ""
	}
	return fmt.Sprintf("(%s)", src.FormatLevels())
}
