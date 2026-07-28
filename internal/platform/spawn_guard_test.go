package platform

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// spawnGuardExemptions lists the only non-test files under internal/ that
// may reach for os/exec's process-spawning API directly. Every entry needs
// a reason: an unexplained exemption is how the Windows console-window fix
// silently rotted the first time.
//
// Paths are slash-separated and relative to the repository root.
var spawnGuardExemptions = map[string]string{
	"internal/platform/command.go":   "the sanctioned constructor itself — it IS the wrapper every other call site must use",
	"internal/shell/exec_unix.go":    "mirrors interp.DefaultExecHandler with an exec.Cmd literal (needs Path, not LookPath semantics); sets SysProcAttr explicitly via isolateProcess",
	"internal/shell/exec_windows.go": "mirrors interp.DefaultExecHandler with an exec.Cmd literal; sets SysProcAttr{HideWindow: true} inline on the literal",
}

// TestNoUnhardenedProcessSpawns fails when any non-test file under
// internal/ spawns a child process without going through
// [Command].
//
// Why this test exists: on Windows a console-subsystem child spawned by a
// console-less crush (the normal state of a detached/orchestrator
// `crush run` — see cmd.maybeDetachConsole) pops a real console window
// that flashes on screen, covers the operator's work and steals keyboard
// focus. The fix is one process-creation flag, and it was originally
// applied by convention: "remember to call platform.HideConsoleWindow at
// every new call site". Conventions do not survive refactors, and nothing
// in the test suite could tell whether the convention still held.
//
// The guarded surface is deliberately narrow — exec.Command,
// exec.CommandContext and exec.Cmd composite literals. exec.LookPath and
// friends do not start a process and are not flagged.
func TestNoUnhardenedProcessSpawns(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	internalDir := filepath.Join(root, "internal")

	var offenders []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if _, exempt := spawnGuardExemptions[rel]; exempt {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			switch sel.Sel.Name {
			case "Command", "CommandContext", "Cmd":
				// exec.Cmd appears legitimately as a TYPE (parameters,
				// struct fields, `var cmd *exec.Cmd`). Only a composite
				// literal actually constructs — and thus can spawn — one.
				if sel.Sel.Name == "Cmd" && !isCompositeLitType(file, sel) {
					return true
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d uses exec.%s",
					rel, fset.Position(sel.Pos()).Line, sel.Sel.Name))
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)

	require.Empty(t, offenders,
		"these files spawn child processes without platform.Command, which on Windows lets a "+
			"console window flash up and steal focus.\nUse platform.Command(ctx, name, args...) instead, "+
			"or add the file to spawnGuardExemptions with a reason:\n  %s",
		strings.Join(offenders, "\n  "))
}

// isCompositeLitType reports whether sel is used as the type of a
// composite literal (`exec.Cmd{...}`) rather than merely referenced as a
// type name (`*exec.Cmd`, `func(c *exec.Cmd)`).
func isCompositeLitType(file *ast.File, sel *ast.SelectorExpr) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if lit.Type == sel {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestSpawnGuardExemptionsAreCurrent keeps the exemption list honest: an
// entry naming a file that no longer exists is stale and would silently
// widen the guard's blind spot if the path were ever recreated.
func TestSpawnGuardExemptionsAreCurrent(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	for rel, reason := range spawnGuardExemptions {
		require.NotEmpty(t, reason, "exemption %q must carry a reason", rel)
		require.FileExists(t, filepath.Join(root, filepath.FromSlash(rel)),
			"exemption %q names a file that no longer exists — remove it", rel)
	}
}

// repoRoot resolves the repository root from this test file's own location,
// so the walk works regardless of the working directory the test runs in.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot determine test file location")
	// <root>/internal/platform/spawn_guard_test.go -> <root>
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}
