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
//     missing path) as compliant controls.
//   - NOT covered, on purpose: bash (separate work on this branch, and
//     its input space is the command string, not JSON params);
//     askquestion (its one Go error is the deliberate control-flow
//     AskQuestionError that surfaces the question to the operator);
//     webfetch, websearch, crushinfo, crushlogs, jobkill, joboutput (no
//     returned-error sites at all, grep-verified — nothing for this
//     guard to catch); listmcpresources and readmcpresource (fatal
//     sites sit behind MCP client wiring this test does not have);
//     readdelegationtranscript (its fatal sites are session/DB
//     infrastructure, correctly fatal per the contract, and its
//     model-input refusal path is already pinned by
//     read_delegation_transcript_test.go).
//
// The guard pins the boundary per tool, not per line: view.go also has
// fatal errno returns at the image-read and text-read sites, but
// reaching those deterministically requires a stat-succeeds-then-read-
// fails filesystem state (corruption or a race), which is not model
// input. The fix pattern for them is the same as for the sites below.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

	cases := []struct {
		tool string
		desc string
		run  func() (fantasy.ToolResponse, error)
	}{
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
