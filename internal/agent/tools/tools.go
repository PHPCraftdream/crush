package tools

import (
	"bytes"
	"context"
	"html/template"
	"os/exec"
	"testing"

	"charm.land/fantasy"
)

type (
	sessionIDContextKey string
	messageIDContextKey string
	supportsImagesKey   string
	modelNameKey        string
)

const (
	// SessionIDContextKey is the key for the session ID in the context.
	SessionIDContextKey sessionIDContextKey = "session_id"
	// MessageIDContextKey is the key for the message ID in the context.
	MessageIDContextKey messageIDContextKey = "message_id"
	// SupportsImagesContextKey is the key for the model's image support capability.
	SupportsImagesContextKey supportsImagesKey = "supports_images"
	// ModelNameContextKey is the key for the model name in the context.
	ModelNameContextKey modelNameKey = "model_name"
)

// getContextValue is a generic helper that retrieves a typed value from context.
// If the value is not found or has the wrong type, it returns the default value.
func getContextValue[T any](ctx context.Context, key any, defaultValue T) T {
	value := ctx.Value(key)
	if value == nil {
		return defaultValue
	}
	if typedValue, ok := value.(T); ok {
		return typedValue
	}
	return defaultValue
}

// GetSessionFromContext retrieves the session ID from the context.
func GetSessionFromContext(ctx context.Context) string {
	return getContextValue(ctx, SessionIDContextKey, "")
}

// GetMessageFromContext retrieves the message ID from the context.
func GetMessageFromContext(ctx context.Context) string {
	return getContextValue(ctx, MessageIDContextKey, "")
}

// GetSupportsImagesFromContext retrieves whether the model supports images from the context.
func GetSupportsImagesFromContext(ctx context.Context) bool {
	return getContextValue(ctx, SupportsImagesContextKey, false)
}

// GetModelNameFromContext retrieves the model name from the context.
func GetModelNameFromContext(ctx context.Context) string {
	return getContextValue(ctx, ModelNameContextKey, "")
}

// Tool error contract: the three ways a tool can fail, and when to use
// each.
//
// A fantasy.AgentTool has three failure channels, and picking the wrong
// one is the difference between "the model corrects itself and carries
// on" and "the whole run dies on the spot":
//
//   1. Recoverable — `return fantasy.NewTextErrorResponse(msg), nil`.
//      The model sees the message as an ordinary tool result and can fix
//      its input next turn. Already in use: file-not-found (with
//      spelling suggestions) and path-is-a-directory in view.go, the
//      invalid todo status in todos.go (f51baaca), non-200 HTTP status
//      in fetch/download/sourcegraph, search errors in glob/grep.
//
//   2. Recoverable, turn-ending — a level-1 response with
//      `resp.StopTurn = true`. The turn ends cleanly: the session stays
//      alive, history stays intact, and the reason is visible to the
//      operator. Already in use: permission denials
//      (NewPermissionDeniedResponse directly below), the hook veto in
//      hooked_tool.go.
//
//   3. Fatal — `return fantasy.ToolResponse{}, err`. The whole run
//      dies; see below for the mechanism.
//
// The mechanism that makes level 3 catastrophic: in fantasy v0.25.2
// (agent.go), a non-nil error from a tool's Run makes executeSingleTool
// return its second value — named isCriticalError at the call site in
// executeTools — as true, and executeTools then does
// `return nil, errorResult.Error`, unwinding the agent loop then and
// there. The model never sees the message; the run simply ends. A
// response carrying IsError returns false there instead and is fed back
// to the model as an ordinary tool result it can react to. The two forms
// look nearly identical in Go — `return resp, nil` vs `return resp, err`
// is a reflex-level edit — which is exactly why this contract exists:
// do not "simplify" a response into a returned error, and do not add new
// level-3 sites for model-input failures.
//
// Choosing a level — the retry-invariance criterion. Return level 3 if
// and only if the failure is INVARIANT TO RETRY in this session: no
// input the model could send next, and no passage of time, would make
// the call succeed. A missing session ID is fatal — the next call fails
// identically, so continuing merely loops. A failed DB read or write is
// fatal for the same reason. An unresolvable DNS name is NOT fatal —
// the next call may succeed, and the model can equally pick a different
// URL. A malformed URL, a value outside the legal set, a path the OS
// rejects, a negative offset: all model input, all correctable, all
// level 1 or 2. This criterion is checkable by reading the code, unlike
// "could the model figure out its mistake", which would require
// modelling someone else's mind.
//
// Why the default is recoverable — the radius asymmetry. An error made
// recoverable by mistake costs one extra model turn. An error made fatal
// by mistake costs the entire session — potentially many minutes of work
// on a large prompt (a single mistyped todo status did exactly that, 42
// seconds into a 75k-character run; see f51baaca). When in doubt, return
// a response, not an error.
//
// The counter-argument, so this does not read one-sided: a fatal error
// reaches the OPERATOR — it surfaces through the run's JSON envelope and
// exit status, and cannot be ignored. A NewTextErrorResponse reaches the
// MODEL, which may swallow it and cheerfully continue, masking an
// infrastructure failure as ordinary tool chatter. Blanket-demoting
// every error to level 1 is therefore just as wrong as blanket-promoting
// to level 3. Level 2 exists for precisely this middle: end the turn,
// keep the session, make the refusal loud. Infrastructure that is broken
// for good within this session (missing session ID, unusable DB) stays
// at level 3 so a human finds out.
//
// error_contract_test.go enforces the model-input half of this contract.
// It names every tool that still returns a level-3 error for model input,
// so its failure output doubles as the outstanding work list.

// NewPermissionDeniedResponse returns a tool response indicating the user
// denied permission, with StopTurn set so the agent loop does not retry.
func NewPermissionDeniedResponse() fantasy.ToolResponse {
	resp := fantasy.NewTextErrorResponse("User denied permission")
	resp.StopTurn = true
	return resp
}

// ghAvailable indicates whether the `gh` CLI is available on PATH.
var ghAvailable = func() bool {
	if testing.Testing() {
		return false
	}
	_, err := exec.LookPath("gh")
	return err == nil
}()

// toolDescriptionData is the common data structure for tool description templates.
type toolDescriptionData struct {
	GhAvailable bool
}

// renderToolDescription renders a tool description template with the given data.
func renderToolDescription(tmpl *template.Template) string {
	data := toolDescriptionData{
		GhAvailable: ghAvailable,
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}

// renderTemplate renders a Go template with the given data.
func renderTemplate(tmpl *template.Template, data any) string {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}
