package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsInjectCmd = &cobra.Command{
	Use:   "inject <session-id>",
	Short: "Inject a user message into a session from another process",
	Long: `Inject a message into a running (or at-rest) session as if the user
had typed it. The message is persisted immediately as a normal user
message — it renders in the web UI exactly like anything the user sends —
and a cross-process "pending inject" signal is queued so that whichever
process is currently running the session splices it into the live prompt.

The message text comes from either -m/--message (inline) or -f/--file
(read from a UTF-8 file). Exactly one of the two must be given.

If the session is currently running in another process, the message is
merged into its next provider request without restarting the turn. With
--interrupt the inject is marked so the running turn is cancelled and
restarted with the new message (interrupt handling itself lives in the
running process). If no process is currently running the session, the
message is still persisted and will be picked up the next time the
session runs.

The <session-id> may be a full session id or a hash prefix as printed by
"sessions list".`,
	Args: cobra.ExactArgs(1),
	Example: `
# Inline message
crush sessions inject pr-42 -m "also update the changelog"

# From a file
crush sessions inject pr-42 -f ./notes/next-step.md

# Interrupt the current turn and restart with this message
crush sessions inject pr-42 -m "stop, wrong approach" --interrupt

# Match by hash prefix, machine-readable result
crush sessions inject 8a3f0c -m "continue" --json
  `,
	RunE: sessionsInjectCmdRun,
}

func init() {
	sessionsInjectCmd.Flags().StringP("message", "m", "", "Message text to inject (mutually exclusive with --file)")
	sessionsInjectCmd.Flags().StringP("file", "f", "", "Read message text from this file (mutually exclusive with --message)")
	sessionsInjectCmd.Flags().Bool("interrupt", false, "Cancel the current turn and restart it with this message")
	sessionsInjectCmd.Flags().Bool("json", false, "Emit a structured JSON result")

	sessionsCmd.AddCommand(sessionsInjectCmd)
}

// injectResult is the wire shape of `crush sessions inject --json`.
type injectResult struct {
	SessionID string `json:"session_id"`
	Hash      string `json:"hash"`
	MessageID string `json:"message_id"`
	Interrupt bool   `json:"interrupt"`
	Running   bool   `json:"running"`
	Status    string `json:"status"` // injected | queued-for-interrupt | persisted-offline
}

func sessionsInjectCmdRun(cmd *cobra.Command, args []string) error {
	inline, _ := cmd.Flags().GetString("message")
	file, _ := cmd.Flags().GetString("file")
	interrupt, _ := cmd.Flags().GetBool("interrupt")
	asJSON, _ := cmd.Flags().GetBool("json")

	text, err := resolveInjectText(inline, file)
	if err != nil {
		return err
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, msg, err := doInject(cmd.Context(), a.Sessions, a.Messages, args[0], text, interrupt)
	if err != nil {
		return err
	}

	running := isSessionLockAlive(a.Config().Options.DataDirectory, sess.ID)

	status := "injected"
	switch {
	case !running:
		status = "persisted-offline"
	case interrupt:
		status = "queued-for-interrupt"
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(injectResult{
			SessionID: sess.ID,
			Hash:      session.HashID(sess.ID),
			MessageID: msg.ID,
			Interrupt: interrupt,
			Running:   running,
			Status:    status,
		})
	}

	switch status {
	case "persisted-offline":
		fmt.Fprintf(os.Stderr, "message persisted; no process is currently running this session — it will be picked up when the session next runs\n")
	case "queued-for-interrupt":
		fmt.Fprintf(os.Stderr, "queued for interrupt on session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
	default:
		fmt.Fprintf(os.Stderr, "injected into session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
	}
	return nil
}

// resolveInjectText validates the -m/-f pair (exactly one required) and
// returns the message text, reading from the file when -f is given.
func resolveInjectText(inline, file string) (string, error) {
	switch {
	case inline == "" && file == "":
		return "", fmt.Errorf("no message given: pass exactly one of -m/--message or -f/--file")
	case inline != "" && file != "":
		return "", fmt.Errorf("both -m/--message and -f/--file given: pass exactly one")
	}
	text := inline
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("failed to read message file %s: %w", file, err)
		}
		text = string(data)
	}
	if text == "" {
		return "", fmt.Errorf("message text is empty")
	}
	return text, nil
}

// doInject resolves the session, persists a normal user message (so the web
// UI renders it as user-typed), and queues the cross-process pending-inject
// signal referencing that message row. Split out from RunE so it is testable
// without a full app/config bootstrap.
func doInject(
	ctx context.Context,
	sessions session.Service,
	messages message.Service,
	idOrHash, text string,
	interrupt bool,
) (session.Session, message.Message, error) {
	sess, err := resolveSessionID(ctx, sessions, idOrHash)
	if err != nil {
		return session.Session{}, message.Message{}, err
	}

	// Mirrors sessionAgent.createUserMessage's CreateMessageParams shape so
	// the message renders identically to anything the user sends.
	msg, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	if err != nil {
		return session.Session{}, message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}

	if err := sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID,
		MessageID: msg.ID,
		Content:   text,
		Interrupt: interrupt,
	}); err != nil {
		return session.Session{}, message.Message{}, fmt.Errorf("failed to queue pending inject: %w", err)
	}
	return sess, msg, nil
}

// isSessionLockAlive reports whether a live process currently holds this
// session's lock. The heartbeat mtime is authoritative: on Windows the
// exclusive lock prevents reading the PID while the holder is alive, so a
// fresh lock with PID 0 still means "running".
func isSessionLockAlive(dataDir, sessionID string) bool {
	if dataDir == "" || sessionID == "" {
		return false
	}
	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		return false
	}
	return lockPulseStatus(int64(time.Since(info.ModTime()).Seconds())) != "offline"
}
