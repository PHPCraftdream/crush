package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPingCmd_Exists(t *testing.T) {
	t.Parallel()
	require.NotNil(t, pingCmd)
	require.Equal(t, "ping [--json] [--timeout 15s] [--prompt \"<custom>\"]", pingCmd.Use)
	require.NotEmpty(t, pingCmd.Short)
	require.NotEmpty(t, pingCmd.Long)
}

func TestPingCmd_Flags(t *testing.T) {
	t.Parallel()
	require.NotNil(t, pingCmd.Flags().Lookup("json"))
	require.NotNil(t, pingCmd.Flags().Lookup("timeout"))
	require.NotNil(t, pingCmd.Flags().Lookup("prompt"))
}

func TestPingCmd_DefaultTimeout(t *testing.T) {
	t.Parallel()
	flag := pingCmd.Flags().Lookup("timeout")
	require.NotNil(t, flag)
	require.Equal(t, "15s", flag.DefValue)

	// Verify it parses as a duration
	duration, err := time.ParseDuration(flag.DefValue)
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, duration)
}

func TestPingFastCmd_Exists(t *testing.T) {
	t.Parallel()
	require.NotNil(t, pingFastCmd)
	require.Equal(t, "ping-fast [--json] [--timeout 15s] [--prompt \"<custom>\"]", pingFastCmd.Use)
	require.NotEmpty(t, pingFastCmd.Short)
	require.NotEmpty(t, pingFastCmd.Long)
}

func TestPingFastCmd_Flags(t *testing.T) {
	t.Parallel()
	require.NotNil(t, pingFastCmd.Flags().Lookup("json"))
	require.NotNil(t, pingFastCmd.Flags().Lookup("timeout"))
	require.NotNil(t, pingFastCmd.Flags().Lookup("prompt"))
}

func TestPingResult_JSONMarshal(t *testing.T) {
	t.Parallel()

	result := PingResult{
		Provider:         "anthropic",
		Model:            "claude-opus",
		Effort:           "high",
		Atom:             "opus",
		Status:           "ok",
		LatencyMs:        742,
		Response:         "OK",
		PromptTokens:     18,
		CompletionTokens: 1,
		CostUSD:          0,
		Error:            nil,
	}

	// Verify JSON marshaling works
	data, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotNil(t, data)

	// Verify unmarshaling works
	var parsed map[string]interface{}
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)

	require.Equal(t, "anthropic", parsed["provider"])
	require.Equal(t, "claude-opus", parsed["model"])
	require.Equal(t, "ok", parsed["status"])
	require.Equal(t, float64(742), parsed["latency_ms"])
	require.Equal(t, "OK", parsed["response"])
	require.Nil(t, parsed["error"])
}

func TestPingResult_WithError(t *testing.T) {
	t.Parallel()

	errMsg := "authentication failed"
	result := PingResult{
		Provider:  "anthropic",
		Model:     "claude-opus",
		Status:    "error",
		LatencyMs: 150,
		Error:     &errMsg,
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	var parsed map[string]interface{}
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)

	require.Equal(t, "error", parsed["status"])
	require.Equal(t, "authentication failed", parsed["error"])
}

func TestStringPtr(t *testing.T) {
	t.Parallel()

	s := "test"
	ptr := stringPtr(s)
	require.NotNil(t, ptr)
	require.Equal(t, s, *ptr)
}

func TestPingResult_DegradedStatus(t *testing.T) {
	t.Parallel()

	// Response must be exactly "OK" for ok status
	responses := []struct {
		response string
		expected string
	}{
		{"OK", "ok"},
		{"ok", "degraded"},
		{"OK\n", "degraded"},
		{"", "degraded"},
		{"YES", "degraded"},
	}

	for _, tc := range responses {
		status := "ok"
		if tc.response != "OK" {
			status = "degraded"
		}
		require.Equal(t, tc.expected, status)
	}
}

func TestPingCmd_CommandsRegistered(t *testing.T) {
	t.Parallel()

	var pingFound, pingFastFound bool
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "ping" {
			pingFound = true
		}
		if cmd.Name() == "ping-fast" {
			pingFastFound = true
		}
	}

	require.True(t, pingFound, "ping command should be registered")
	require.True(t, pingFastFound, "ping-fast command should be registered")
}
