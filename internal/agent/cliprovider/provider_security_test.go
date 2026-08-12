package cliprovider

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestPromptNotLoggedInProduction verifies that prompts and their sensitive
// data do not appear in INFO-level logs during normal operation. This is a
// regression test for SEC-1 (security/observability): a real bug where the
// full system prompt (~12KB) was being logged to INFO via the "args" field.
func TestPromptNotLoggedInProduction(t *testing.T) {
	// Ensure diagnostic mode is OFF
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "")

	// Create a test handler that captures all log output
	var logBuf bytes.Buffer
	handler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Create args with a secret marker
	const secretMarker = "SECRET_MARKER_12345_ABCDEF_GHIJK_LMNOP_QRSTUV"
	args := []string{"--model", "sonnet", "-p", secretMarker, "--verbose"}

	// Sanitize the args
	sanitized := sanitizeArgs(args)

	// Verify the secret marker does NOT appear in the sanitized args
	sanitizedStr := strings.Join(sanitized, " ")
	if strings.Contains(sanitizedStr, secretMarker) {
		t.Errorf("SECURITY LEAK: secret marker %q found in sanitized args.\n\nSanitized args: %s", secretMarker, sanitizedStr)
	}

	// Verify the -p flag is present (flag name should remain)
	if !strings.Contains(sanitizedStr, "-p") {
		t.Errorf("Expected flag -p to remain in sanitized args, got: %s", sanitizedStr)
	}

	// Verify safe flags are preserved (e.g., --model sonnet)
	if !strings.Contains(sanitizedStr, "--model") || !strings.Contains(sanitizedStr, "sonnet") {
		t.Errorf("Expected safe flag --model and value sonnet to remain, got: %s", sanitizedStr)
	}

	// Verify [REDACTED] placeholder is present for -p value
	if !strings.Contains(sanitizedStr, "[REDACTED]") {
		t.Errorf("Expected [REDACTED] placeholder for -p value, got: %s", sanitizedStr)
	}
}

// TestPromptLoggedInDebugMode verifies that prompts CAN be logged when
// explicitly enabled via CRUSH_CLIPROVIDER_LOG_RAW_PROMPT=1. This is the
// opt-in diagnostic mode for troubleshooting CLI invocation issues.
func TestPromptLoggedInDebugMode(t *testing.T) {
	// Enable diagnostic mode
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "1")

	const secretMarker = "SECRET_MARKER_12345"
	args := []string{"--model", "sonnet", "-p", secretMarker, "--verbose"}

	// Sanitize the args - in debug mode they should NOT be redacted
	sanitized := sanitizeArgs(args)
	sanitizedStr := strings.Join(sanitized, " ")

	// In debug mode, the secret marker SHOULD appear
	if !strings.Contains(sanitizedStr, secretMarker) {
		t.Errorf("In debug mode, expected secret marker to appear, got: %s", sanitizedStr)
	}

	// Verify the args are unchanged (no [REDACTED])
	if strings.Contains(sanitizedStr, "[REDACTED]") {
		t.Errorf("In debug mode, expected no [REDACTED] placeholder, got: %s", sanitizedStr)
	}
}

// TestLogRawPromptEnabled verifies the diagnostic mode flag is respected.
func TestLogRawPromptEnabled(t *testing.T) {
	// Default: should be off
	os.Unsetenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT")
	if logRawPromptEnabled() {
		t.Error("Expected logRawPromptEnabled to be false by default")
	}

	// Set to "1": should be on
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "1")
	if !logRawPromptEnabled() {
		t.Error("Expected logRawPromptEnabled to be true when set to '1'")
	}

	// Set to anything else: should be off
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "0")
	if logRawPromptEnabled() {
		t.Error("Expected logRawPromptEnabled to be false when set to '0'")
	}

	// Set to random string: should be off
	t.Setenv("CRUSH_CLIPROVIDER_LOG_RAW_PROMPT", "random")
	if logRawPromptEnabled() {
		t.Error("Expected logRawPromptEnabled to be false when set to 'random'")
	}
}

// TestArgsSanitization verifies that argument lists are sanitized to remove
// sensitive values (like prompts or tokens) from logs while preserving flag names.
func TestArgsSanitization(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantFlag string // A flag name that should appear
		dontWant string // A value that should NOT appear (e.g., a prompt)
	}{
		{
			name:     "prompt value redacted from args",
			args:     []string{"--model", "sonnet", "-p", "SECRET_TOKEN_12345", "--verbose"},
			wantFlag: "--model",
			dontWant: "SECRET_TOKEN_12345",
		},
		{
			name:     "mcp-config path preserved (safe)",
			args:     []string{"--mcp-config", "/tmp/crush-mcp-123.json"},
			wantFlag: "--mcp-config",
			dontWant: "", // Path is safe, nothing to exclude
		},
		{
			name:     "allowed-tools preserved",
			args:     []string{"--allowedTools", "Bash,Read,Write"},
			wantFlag: "--allowedTools",
			dontWant: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sanitized := sanitizeArgs(tt.args)
			sanitizedStr := strings.Join(sanitized, " ")

			if tt.wantFlag != "" && !strings.Contains(sanitizedStr, tt.wantFlag) {
				t.Errorf("Expected flag %q to appear in sanitized args, got: %s", tt.wantFlag, sanitizedStr)
			}

			if tt.dontWant != "" && strings.Contains(sanitizedStr, tt.dontWant) {
				t.Errorf("Expected value %q to be redacted from sanitized args, got: %s", tt.dontWant, sanitizedStr)
			}
		})
	}
}
