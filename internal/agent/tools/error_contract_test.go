package tools

// Guard test for the tool error contract documented in tools.go: input a
// model can plausibly send must never come back as a returned Go error.
//
// fantasy gives a tool three failure channels (tools.go has the full
// contract): a text-error response the model can learn from, the same
// with StopTurn, or a returned error. Only the third kills the whole
// run: fantasy's executeSingleTool flags a non-nil Run error as critical
// and executeTools aborts the agent loop with it, so one malformed URL
// or one OS-rejected path ends a session that may be minutes old, and
// the model never learns why (that is how f51baaca's wild incident
// looked).
//
// What is asserted is the boundary that decides all of it — Run's
// second return value must be nil on bad model input — and deliberately
// NOT the message text: a content check would pass just as well against
// a fatal error whose message happens to contain the expected substring
// (p483's revert-check spelled that trap out).
//
// This test is EXPECTED TO BE RED while #490/#491 are open. Its failure
// message is the work list; it goes green when the last entry is fixed.
// Known-compliant tools are fed the same kind of bad input inside the
// same run, and TestErrorContract_Control_ValidInputSucceeds proves the
// harness green on valid input — so a red entry can only mean "this
// tool returns a Go error for model input", never "the wiring is
// broken".
//
// Coverage:
//
//   - Covered: download, fetch, sourcegraph (malformed URL / dead
//     network), view, edit, write, multiedit (OS-rejected path), todos
//     (invalid enum, fixed by f51baaca), glob, grep, ls (bad pattern /
//     missing path) as compliant controls; the MkdirAll / final-write
//     class — parent-path-component-is-a-file for write, edit,
//     multiedit and download, download file_path naming an existing
//     directory, and write/edit onto a read-only file (the final
//     rename); view's residual branches — a file that cannot be opened
//     for reading (share-locked on Windows, unreadable mode elsewhere)
//     and, on Windows, the ERROR_INVALID_NAME stat residual.
//   - NOT covered, on purpose: bash (its input space is the command
//     string, not JSON params — separate work); askquestion (its one
//     Go error is the deliberate control-flow AskQuestionError that
//     surfaces the question to the operator); webfetch, websearch,
//     crushinfo, crushlogs, jobkill, joboutput (no returned-error
//     sites, grep-verified — nothing for this guard to catch);
//     mcp-tools.go, list_mcp_resources.go and read_mcp_resource.go
//     (their level-3 sites are missing session IDs and MCP client
//     wiring, which is session infrastructure, correctly fatal per
//     the contract, and unreachable in this harness without an MCP
//     server); readdelegationtranscript (its fatal sites are
//     session/DB infrastructure, correctly fatal per the contract,
//     and its model-input refusal path is already pinned by
//     read_delegation_transcript_test.go); the deliberately-fatal
//     residual of the errno split itself — media and resource
//     failures (full disk, I/O device errors, write-protected media,
//     out-of-memory) are SUPPOSED to stay level 3; this guard cannot
//     construct them without a genuinely broken disk, so that half
//     is pinned by the classifier unit tests (os_failure_*_test.go)
//     instead; download's temp-file create/chmod/sync/close sites
//     and MkdirAll under an unwritable parent (constructing those
//     needs ACL-level permission setup that plain Go cannot do on
//     Windows — measured: a directory's read-only attribute does NOT
//     block child creation, MkdirAll and CreateTemp both succeed —
//     so those sites share the classifier but have no direct guard
//     input on this platform).
//
// The guard pins the boundary per tool, not per line: view.go's
// text-read residual is now reached deterministically (a share-
// locked or unreadable file); the image-read site shares the
// identical call shape and classifier.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// contractTransport is a deterministic http.RoundTripper: it either
// fails like a dead network (an unreachable host in the wild produces
// exactly this shape of error from client.Do) or returns a canned
// response, so the HTTP tools are tested with no sockets and no DNS.
type contractTransport struct {
	err    error
	status int
	body   string
}

func (t contractTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{
		StatusCode: t.status,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(t.body)),
	}, nil
}

func netDownClient() *http.Client {
	return &http.Client{Transport: contractTransport{err: fmt.Errorf(
		"dial tcp: lookup unreachable.invalid: no such host")}}
}

func cannedClient(status int, body string) *http.Client {
	return &http.Client{Transport: contractTransport{status: status, body: body}}
}

// runContractTool marshals params and runs the tool, returning Run's two
// values untouched; a marshal failure would be a harness bug, so it
// fails the test loudly instead of masquerading as a contract violation.
func runContractTool(t *testing.T, ctx context.Context, tool fantasy.AgentTool, name string, params any) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", name, err)
	}
	return tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: name, Input: string(input)})
}

func TestErrorContract_BadModelInputIsAResponseNotAnError(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "error-contract-guard")

	// A NUL byte makes a path the OS itself rejects: every path syscall
	// fails with EINVAL on every Go platform, which is NOT
	// os.IsNotExist, so it slips past the friendly "file not found"
	// handling and lands on the residual errno returns — filepath.Abs at
	// view.go:128, or os.Stat at view.go:191 for inputs Abs tolerates,
	// and os.Stat at edit.go:115, write.go:93 and multiedit.go:161. A
	// model plausibly sends this after mangling a
	// string; the input is correctable — the model can resend a clean
	// path — so per the contract it must come back as a response.
	const nulPath = "bad\x00path.txt"

	run := func(tool fantasy.AgentTool, name string, params any) (fantasy.ToolResponse, error) {
		return runContractTool(t, ctx, tool, name, params)
	}

	type contractCase struct {
		tool string
		desc string
		run  func() (fantasy.ToolResponse, error)
	}

	cases := []contractCase{
		{
			tool: "download",
			desc: `malformed URL "http://exa mple.com/x"`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewDownloadTool(&mockPermissionService{}, workingDir, netDownClient()),
					DownloadToolName,
					DownloadParams{URL: "http://exa mple.com/x", FilePath: "out.bin"})
			},
		},
		{
			tool: "download",
			desc: "unreachable host https://unreachable.invalid/x",
			run: func() (fantasy.ToolResponse, error) {
				return run(NewDownloadTool(&mockPermissionService{}, workingDir, netDownClient()),
					DownloadToolName,
					DownloadParams{URL: "https://unreachable.invalid/x", FilePath: "out.bin"})
			},
		},
		{
			tool: "fetch",
			desc: `malformed URL "http://exa mple.com/x"`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewFetchTool(&mockPermissionService{}, workingDir, netDownClient()),
					FetchToolName,
					FetchParams{URL: "http://exa mple.com/x", Format: "text"})
			},
		},
		{
			tool: "fetch",
			desc: "unreachable host https://unreachable.invalid/x",
			run: func() (fantasy.ToolResponse, error) {
				return run(NewFetchTool(&mockPermissionService{}, workingDir, netDownClient()),
					FetchToolName,
					FetchParams{URL: "https://unreachable.invalid/x", Format: "text"})
			},
		},
		{
			// sourcegraph's request-creation error site cannot be
			// reached by model input at all (the URL is fixed), so only
			// the transport failure is guardable here.
			tool: "sourcegraph",
			desc: "network down while searching",
			run: func() (fantasy.ToolResponse, error) {
				return run(NewSourcegraphTool(netDownClient()),
					SourcegraphToolName,
					SourcegraphParams{Query: "repo:charmbracelet/crush TestMain"})
			},
		},
		{
			tool: "view",
			desc: `OS-rejected path "bad\x00path.txt"`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewViewTool(&mockPermissionService{}, mockFileTracker{}, nil, workingDir),
					ViewToolName,
					ViewParams{FilePath: nulPath})
			},
		},
		{
			tool: "edit",
			desc: `OS-rejected path "bad\x00path.txt" (file-creation mode)`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewEditTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, workingDir),
					EditToolName,
					EditParams{FilePath: nulPath, NewString: "x"})
			},
		},
		{
			tool: "write",
			desc: `OS-rejected path "bad\x00path.txt"`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, workingDir),
					WriteToolName,
					WriteParams{FilePath: nulPath, Content: "x"})
			},
		},
		{
			tool: "multiedit",
			desc: `OS-rejected path "bad\x00path.txt" (file-creation mode)`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewMultiEditTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, workingDir),
					MultiEditToolName,
					MultiEditParams{FilePath: nulPath, Edits: []MultiEditOperation{{NewString: "x"}}})
			},
		},
		{
			// write.go MkdirAll: os.Stat of notes.txt/child.md fails
			// with ENOTDIR but os.IsNotExist is TRUE (measured), so the
			// demoted `!os.IsNotExist` branch is skipped and control
			// reaches the unconditionally fatal MkdirAll.
			tool: "write",
			desc: `parent path component is a file (notes.txt/child.md)`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
				return run(NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					WriteToolName,
					WriteParams{FilePath: "notes.txt/child.md", Content: "x"})
			},
		},
		{
			// edit.go MkdirAll in file-creation mode: same ENOTDIR-but-
			// IsNotExist stat, same fall-through to a fatal MkdirAll.
			tool: "edit",
			desc: `parent path component is a file (notes.txt/child.md), file-creation mode`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
				return run(NewEditTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					EditToolName,
					EditParams{FilePath: "notes.txt/child.md", NewString: "x"})
			},
		},
		{
			// multiedit.go MkdirAll in file-creation mode: same
			// fall-through as edit.
			tool: "multiedit",
			desc: `parent path component is a file (notes.txt/child.md), file-creation mode`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
				return run(NewMultiEditTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					MultiEditToolName,
					MultiEditParams{FilePath: "notes.txt/child.md", Edits: []MultiEditOperation{{NewString: "x"}}})
			},
		},
		{
			// download.go MkdirAll: the fetch already succeeded when the
			// parent of the target turns out to live under the file
			// notes.txt, and the fatal MkdirAll kills the run anyway.
			tool: "download",
			desc: `parent path component is a file (notes.txt/child.md)`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
				return run(NewDownloadTool(&mockPermissionService{}, dir, cannedClient(http.StatusOK, "payload")),
					DownloadToolName,
					DownloadParams{URL: "https://example.com/ok.bin", FilePath: "notes.txt/child.md"})
			},
		},
		{
			// download.go rename: MkdirAll of the parent succeeds (it is
			// the working dir itself) and the temp file is fully written,
			// but renaming a file over an existing directory is refused
			// by the OS.
			tool: "download",
			desc: `file_path names an existing directory`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(dir, "somedir"), 0o755))
				return run(NewDownloadTool(&mockPermissionService{}, dir, cannedClient(http.StatusOK, "payload")),
					DownloadToolName,
					DownloadParams{URL: "https://example.com/ok.bin", FilePath: "somedir"})
			},
		},
		{
			// write.go final AtomicWriteFile rename: the temp file is
			// written next to the read-only target, then the rename
			// over it is refused.
			tool: "write",
			desc: `target file is read-only (rename refused)`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				roPath := filepath.Join(dir, "ro.txt")
				require.NoError(t, os.WriteFile(roPath, []byte("old"), 0o644))
				require.NoError(t, os.Chmod(roPath, 0o444))
				t.Cleanup(func() { _ = os.Chmod(roPath, 0o644) })
				return run(NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					WriteToolName,
					WriteParams{FilePath: "ro.txt", Content: "new content"})
			},
			// On Windows the atomic-write rename over a read-only file
			// fails with ERROR_ACCESS_DENIED(5); on Linux rename
			// ignores the target's write bit, so the case is vacuous
			// there by design.
		},
		{
			// edit.go final AtomicWriteFile rename via
			// commitFileChange: same refusal on the read-only target.
			tool: "edit",
			desc: `target file is read-only (rename refused)`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				roPath := filepath.Join(dir, "ro.txt")
				require.NoError(t, os.WriteFile(roPath, []byte("old"), 0o644))
				require.NoError(t, os.Chmod(roPath, 0o444))
				t.Cleanup(func() { _ = os.Chmod(roPath, 0o644) })
				return run(NewEditTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					EditToolName,
					EditParams{FilePath: "ro.txt", OldString: "old", NewString: "new"})
			},
			// On Windows the atomic-write rename over a read-only file
			// fails with ERROR_ACCESS_DENIED(5); on Linux rename
			// ignores the target's write bit, so the case is vacuous
			// there by design.
		},

		// Compliant controls: the same bad-input diet on tools that
		// already honour the contract. They must NOT appear in the
		// violation list; if one ever does, that is a regression.
		{
			tool: "todos",
			desc: `invalid todo status "done" (level 1 since f51baaca)`,
			run: func() (fantasy.ToolResponse, error) {
				sessions, _ := newTranscriptTestDB(t)
				sess, err := sessions.Create(context.Background(), "error-contract")
				if err != nil {
					t.Fatalf("create session: %v", err)
				}
				todosCtx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)
				return runContractTool(t, todosCtx, NewTodosTool(sessions), TodosToolName,
					TodosParams{Todos: []TodoItem{{
						Content:    "do the thing",
						Status:     "done",
						ActiveForm: "Doing the thing",
					}}})
			},
		},
		{
			tool: "view",
			desc: `nonexistent file "nope.txt"`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewViewTool(&mockPermissionService{}, mockFileTracker{}, nil, workingDir),
					ViewToolName,
					ViewParams{FilePath: "nope.txt"})
			},
		},
		{
			tool: "glob",
			desc: `malformed pattern "["`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewGlobTool(workingDir),
					GlobToolName,
					GlobParams{Pattern: "["})
			},
		},
		{
			tool: "grep",
			desc: `malformed regex "("`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewGrepTool(workingDir, config.ToolGrep{}),
					GrepToolName,
					GrepParams{Pattern: "(", Path: workingDir})
			},
		},
		{
			tool: "ls",
			desc: `nonexistent directory "no-such-dir"`,
			run: func() (fantasy.ToolResponse, error) {
				return run(NewLsTool(&mockPermissionService{}, workingDir, config.ToolLs{}),
					LSToolName,
					LSParams{Path: "no-such-dir"})
			},
		},
	}

	// A file that exists but cannot be opened: on Windows a shareMode-0
	// CreateFile lock (os.Stat succeeds, os.Open fails errno 32), on
	// Unix mode 000. It exercises view's residual osFailureIsFatal read
	// branch, which is already compliant — this entry guards correct
	// code and must stay green. Root (euid 0 on Unix) bypasses mode
	// bits entirely, so the case is skipped there: root cannot be handed
	// an unreadable file this way.
	if runtime.GOOS == "windows" || os.Geteuid() != 0 {
		cases = append(cases, contractCase{
			tool: "view",
			desc: `existing file that cannot be opened for reading (share-locked on Windows, unreadable mode elsewhere)`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				makeUnreadableFileForTest(t, dir, "unreadable.txt")
				resp, err := run(NewViewTool(&mockPermissionService{}, mockFileTracker{}, nil, dir),
					ViewToolName,
					ViewParams{FilePath: "unreadable.txt"})
				// A reachability proof, not a message pin (which the
				// header forbids): err == nil alone would also pass if
				// some earlier friendly branch handled the input, in
				// which case the residual read branch was never
				// exercised. Only the "Cannot read" response proves the
				// stat-succeeded-open-failed path was actually taken.
				require.NoError(t, err)
				require.Contains(t, resp.Content, "Cannot read")
				return resp, err
			},
		})
	}

	// Windows-only: '<' is an illegal filename character here, so
	// os.Stat fails with ERROR_INVALID_NAME(123), and — measured —
	// os.IsNotExist is FALSE, so it slips past the friendly not-found
	// branch into view's residual Stat branch. On Unix '<' is a legal
	// filename character, hence the windows-only build of this case.
	if runtime.GOOS == "windows" {
		cases = append(cases, contractCase{
			tool: "view",
			desc: `path with an invalid character "bad<name.txt" (ERROR_INVALID_NAME)`,
			run: func() (fantasy.ToolResponse, error) {
				dir := t.TempDir()
				resp, err := run(NewViewTool(&mockPermissionService{}, mockFileTracker{}, nil, dir),
					ViewToolName,
					ViewParams{FilePath: "bad<name.txt"})
				// Same reachability proof as above: the "Cannot access"
				// response is what the residual Stat branch returns.
				require.NoError(t, err)
				require.Contains(t, resp.Content, "Cannot access")
				return resp, err
			},
		})
	}

	var violations []string
	for _, tc := range cases {
		if _, err := tc.run(); err != nil {
			violations = append(violations, fmt.Sprintf(
				"  - %s — %s\n      returned a Go error (contract level 3): %v",
				tc.tool, tc.desc, err))
		}
	}

	require.Empty(t, violations,
		"Bad model input must come back as a tool response the model can "+
			"learn from (contract levels 1-2, see the error contract in "+
			"tools.go), never as a returned Go error (level 3): fantasy's "+
			"executeSingleTool flags a non-nil Run error as critical and "+
			"executeTools aborts the whole agent loop with it — one "+
			"malformed URL or one OS-rejected path would kill a run that "+
			"may be minutes old, and the model would never learn why.\n\n"+
			"This list is the remaining work under #490/#491, not a flake "+
			"and not a broken harness (TestErrorContract_Control_"+
			"ValidInputSucceeds proves the harness green on valid input). "+
			"The test goes green when the last entry is fixed:\n\n%s",
		strings.Join(violations, "\n"))
}

// TestErrorContract_Control_ValidInputSucceeds is the anti-vacuum half of
// the guard: the same harness with valid input must let every covered
// tool succeed (err == nil, IsError == false). Green here plus red in the
// guard means the red entries are caused by the bad input alone — that is
// how the guard is known not to be vacuously failing.
func TestErrorContract_Control_ValidInputSucceeds(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "error-contract-control")

	controls := []struct {
		name string
		run  func(t *testing.T) fantasy.ToolResponse
	}{
		{
			name: "view reads an existing file",
			run: func(t *testing.T) fantasy.ToolResponse {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("hello"), 0o644))
				resp, err := runContractTool(t, ctx,
					NewViewTool(&mockPermissionService{}, mockFileTracker{}, nil, dir),
					ViewToolName, ViewParams{FilePath: "existing.txt"})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: "edit creates a new file",
			run: func(t *testing.T) fantasy.ToolResponse {
				dir := t.TempDir()
				resp, err := runContractTool(t, ctx,
					NewEditTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					EditToolName, EditParams{FilePath: "created.txt", NewString: "content"})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: "write writes a new file",
			run: func(t *testing.T) fantasy.ToolResponse {
				dir := t.TempDir()
				resp, err := runContractTool(t, ctx,
					NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					WriteToolName, WriteParams{FilePath: "written.txt", Content: "content"})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: "multiedit creates and edits a file",
			run: func(t *testing.T) fantasy.ToolResponse {
				dir := t.TempDir()
				resp, err := runContractTool(t, ctx,
					NewMultiEditTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir),
					MultiEditToolName, MultiEditParams{
						FilePath: "multi.txt",
						Edits: []MultiEditOperation{
							{NewString: "seed"},
							{OldString: "seed", NewString: "grown"},
						},
					})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: "download with a live server",
			run: func(t *testing.T) fantasy.ToolResponse {
				dir := t.TempDir()
				resp, err := runContractTool(t, ctx,
					NewDownloadTool(&mockPermissionService{}, dir, cannedClient(http.StatusOK, "downloaded-bytes")),
					DownloadToolName, DownloadParams{URL: "https://example.com/ok.bin", FilePath: "dl.bin"})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: "fetch with a live server",
			run: func(t *testing.T) fantasy.ToolResponse {
				dir := t.TempDir()
				resp, err := runContractTool(t, ctx,
					NewFetchTool(&mockPermissionService{}, dir, cannedClient(http.StatusOK, "fetched-content")),
					FetchToolName, FetchParams{URL: "https://example.com/ok", Format: "text"})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: "sourcegraph with a live server",
			run: func(t *testing.T) fantasy.ToolResponse {
				resp, err := runContractTool(t, ctx,
					NewSourcegraphTool(cannedClient(http.StatusOK, `{"data":{"search":{"results":{}}}}`)),
					SourcegraphToolName, SourcegraphParams{Query: "anything"})
				require.NoError(t, err)
				return resp
			},
		},
		{
			name: "todos saves a valid status",
			run: func(t *testing.T) fantasy.ToolResponse {
				sessions, _ := newTranscriptTestDB(t)
				sess, err := sessions.Create(context.Background(), "error-contract-control")
				require.NoError(t, err)
				todosCtx := context.WithValue(context.Background(), SessionIDContextKey, sess.ID)
				resp, err := runContractTool(t, todosCtx, NewTodosTool(sessions), TodosToolName,
					TodosParams{Todos: []TodoItem{{
						Content:    "do the thing",
						Status:     "pending",
						ActiveForm: "Doing the thing",
					}}})
				require.NoError(t, err)
				return resp
			},
		},
	}

	for _, c := range controls {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			resp := c.run(t)
			require.False(t, resp.IsError, "valid input must succeed: %s", resp.Content)
		})
	}
}
