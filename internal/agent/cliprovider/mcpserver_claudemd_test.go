package cliprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Fork-only tests (batch 15) for the CLAUDE.md filter applied at MCP
// Read tool level. The filter prevents a sub-agent from seeing the
// delegation guidance injected by `crush claude-init` — without it, the
// sub-agent reads "delegate to crush" and spawns another sub-agent,
// recursing.

func TestIsClaudeMdPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"CLAUDE.md", true},
		{"/home/x/repo/CLAUDE.md", true},
		{"D:\\dev\\go\\crush\\c\\CLAUDE.md", true},
		{"claude.md", true},        // case-insensitive
		{"Claude.md", true},
		{"CLAUDE.MD", true},
		{"AGENTS.md", false},
		{"README.md", false},
		{"sub/CLAUDE.md.bak", false}, // basename doesn't match exactly
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, isClaudeMdPath(tc.path))
		})
	}
}

func TestStripCrushClaudeInitBlock_RemovesV8Block(t *testing.T) {
	in := `# CLAUDE.md

User content before.

<!-- crush-claude-init:v8 -->
## Working with crush
Lots of delegation guidance the sub-agent should not see.
<!-- /crush-claude-init -->

User content after.
`
	out := stripCrushClaudeInitBlock(in)
	assert.NotContains(t, out, "crush-claude-init")
	assert.NotContains(t, out, "Working with crush")
	assert.NotContains(t, out, "delegation guidance")
	assert.Contains(t, out, "User content before.")
	assert.Contains(t, out, "User content after.")
}

func TestStripCrushClaudeInitBlock_RemovesV9Block(t *testing.T) {
	in := `<!-- crush-claude-init:v9 -->
fresh marker
<!-- /crush-claude-init -->
`
	out := stripCrushClaudeInitBlock(in)
	assert.NotContains(t, out, "fresh marker")
}

func TestStripCrushClaudeInitBlock_RemovesMultipleBlocks(t *testing.T) {
	// Defence against an operator who somehow ended up with two
	// versioned blocks (e.g. mid-migration). The regex is non-greedy
	// per block, so both should disappear independently.
	in := `prefix
<!-- crush-claude-init:v7 -->
old
<!-- /crush-claude-init -->
middle
<!-- crush-claude-init:v9 -->
new
<!-- /crush-claude-init -->
suffix
`
	out := stripCrushClaudeInitBlock(in)
	assert.NotContains(t, out, "old")
	assert.NotContains(t, out, "new")
	assert.Contains(t, out, "prefix")
	assert.Contains(t, out, "middle")
	assert.Contains(t, out, "suffix")
}

func TestStripCrushClaudeInitBlock_LeavesContentWithoutBlock(t *testing.T) {
	in := "just plain content\nno markers here\n"
	out := stripCrushClaudeInitBlock(in)
	assert.Equal(t, in, out)
}

func TestStripCrushClaudeInitBlock_UnclosedMarkerLeftAsIs(t *testing.T) {
	// If the closing marker is missing (corrupt file), don't munch
	// everything after the opening marker — the regex requires both
	// halves, so the broken half stays visible. That's the safe
	// failure mode: operator sees something is off rather than
	// having half the file disappear.
	in := `before
<!-- crush-claude-init:v9 -->
no closing marker
keep this visible
`
	out := stripCrushClaudeInitBlock(in)
	assert.Equal(t, in, out)
}
