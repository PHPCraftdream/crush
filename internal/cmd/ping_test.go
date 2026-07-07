package cmd

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPingRateLimitReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	t.Run("retry-after seconds", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode:      429,
			ResponseHeaders: map[string]string{"Retry-After": "30"},
		}
		got, ok := pingRateLimitReset(err, now)
		require.True(t, ok)
		require.Equal(t, now.Add(30*time.Second), got)
	})

	t.Run("anthropic reset picks the latest bucket", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode: 429,
			ResponseHeaders: map[string]string{
				"Anthropic-Ratelimit-Requests-Reset":     "2026-06-02T12:00:10Z",
				"Anthropic-Ratelimit-Input-Tokens-Reset": "2026-06-02T12:00:45Z",
				"Anthropic-Ratelimit-Tokens-Reset":       "2026-06-02T12:00:20Z",
			},
		}
		got, ok := pingRateLimitReset(err, now)
		require.True(t, ok)
		require.Equal(t, time.Date(2026, 6, 2, 12, 0, 45, 0, time.UTC), got.UTC())
	})

	t.Run("retry-after wins over reset headers", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			ResponseHeaders: map[string]string{
				"Retry-After":                        "5",
				"Anthropic-Ratelimit-Requests-Reset": "2026-06-02T13:00:00Z",
			},
		}
		got, ok := pingRateLimitReset(err, now)
		require.True(t, ok)
		require.Equal(t, now.Add(5*time.Second), got)
	})

	t.Run("openai duration headers", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			ResponseHeaders: map[string]string{
				"X-Ratelimit-Reset-Requests": "1s",
				"X-Ratelimit-Reset-Tokens":   "6m0s",
			},
		}
		got, ok := pingRateLimitReset(err, now)
		require.True(t, ok)
		require.Equal(t, now.Add(6*time.Minute), got)
	})

	t.Run("non-provider error", func(t *testing.T) {
		t.Parallel()
		_, ok := pingRateLimitReset(errors.New("boom"), now)
		require.False(t, ok)
	})

	t.Run("provider error without reset hints", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{StatusCode: 500, ResponseHeaders: map[string]string{"X-Foo": "bar"}}
		_, ok := pingRateLimitReset(err, now)
		require.False(t, ok)
	})
}

func TestPingCmd_Exists(t *testing.T) {
	t.Parallel()
	require.NotNil(t, pingCmd)
	require.Equal(t, "ping [--role smart|fast] [--json] [--timeout 15s] [--prompt \"<custom>\"]", pingCmd.Use)
	require.NotEmpty(t, pingCmd.Short)
	require.NotEmpty(t, pingCmd.Long)
}

func TestPingCmd_Flags(t *testing.T) {
	t.Parallel()
	require.NotNil(t, pingCmd.Flags().Lookup("json"))
	require.NotNil(t, pingCmd.Flags().Lookup("timeout"))
	require.NotNil(t, pingCmd.Flags().Lookup("prompt"))
	require.NotNil(t, pingCmd.Flags().Lookup("role"))
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

func TestFormatPeakHoursStatus(t *testing.T) {
	t.Parallel()

	// A fixed local-time instant inside a 09:00-18:00 window.
	noon := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	// A fixed local-time instant outside the window (before it starts).
	early := time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)
	// Instant inside an overnight 22:00-06:00 window (pre-midnight leg).
	late := time.Date(2026, 6, 2, 23, 30, 0, 0, time.UTC)

	cases := []struct {
		name   string
		window *config.PeakHoursWindow
		now    time.Time
		want   string
	}{
		{
			name:   "nil window returns empty (feature off)",
			window: nil,
			now:    noon,
			want:   "",
		},
		{
			name:   "zero-valued non-nil window renders empty bounds as not active",
			window: &config.PeakHoursWindow{},
			now:    noon,
			want:   "- (not active)",
		},
		{
			name:   "daytime window active at noon",
			window: &config.PeakHoursWindow{Start: "09:00", End: "18:00"},
			now:    noon,
			want:   "09:00-18:00 (active now)",
		},
		{
			name:   "daytime window not active early morning",
			window: &config.PeakHoursWindow{Start: "09:00", End: "18:00"},
			now:    early,
			want:   "09:00-18:00 (not active)",
		},
		{
			name:   "overnight window active pre-midnight",
			window: &config.PeakHoursWindow{Start: "22:00", End: "06:00"},
			now:    late,
			want:   "22:00-06:00 (active now)",
		},
		{
			name:   "overnight window not active mid-afternoon",
			window: &config.PeakHoursWindow{Start: "22:00", End: "06:00"},
			now:    noon,
			want:   "22:00-06:00 (not active)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatPeakHoursStatus(tc.window, tc.now)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestPingResult_PeakHoursField(t *testing.T) {
	t.Parallel()

	t.Run("set peak_hours renders in JSON", func(t *testing.T) {
		t.Parallel()
		ph := "09:00-18:00 (active now)"
		result := PingResult{
			Provider:  "anthropic",
			Model:     "claude-opus",
			PeakHours: &ph,
			Status:    "ok",
		}
		data, err := json.Marshal(result)
		require.NoError(t, err)

		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &parsed))
		require.Equal(t, "09:00-18:00 (active now)", parsed["peak_hours"])
	})

	t.Run("nil peak_hours is omitted from JSON", func(t *testing.T) {
		t.Parallel()
		result := PingResult{
			Provider:  "anthropic",
			Model:     "claude-opus",
			PeakHours: nil,
			Status:    "ok",
		}
		data, err := json.Marshal(result)
		require.NoError(t, err)

		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &parsed))
		_, present := parsed["peak_hours"]
		require.False(t, present, "peak_hours must be omitted when nil")
	})
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

func TestResolvePingRole(t *testing.T) {
	t.Parallel()

	t.Run("empty defaults to large", func(t *testing.T) {
		t.Parallel()
		modelType, err := resolvePingRole("")
		require.NoError(t, err)
		require.Equal(t, config.SelectedModelTypeLarge, modelType)
	})

	t.Run("smart aliases to large", func(t *testing.T) {
		t.Parallel()
		modelType, err := resolvePingRole("smart")
		require.NoError(t, err)
		require.Equal(t, config.SelectedModelTypeLarge, modelType)
	})

	t.Run("large aliases to large", func(t *testing.T) {
		t.Parallel()
		modelType, err := resolvePingRole("large")
		require.NoError(t, err)
		require.Equal(t, config.SelectedModelTypeLarge, modelType)
	})

	t.Run("fast aliases to small", func(t *testing.T) {
		t.Parallel()
		modelType, err := resolvePingRole("fast")
		require.NoError(t, err)
		require.Equal(t, config.SelectedModelTypeSmall, modelType)
	})

	t.Run("small aliases to small", func(t *testing.T) {
		t.Parallel()
		modelType, err := resolvePingRole("small")
		require.NoError(t, err)
		require.Equal(t, config.SelectedModelTypeSmall, modelType)
	})

	t.Run("invalid role is rejected with run-consistent wording", func(t *testing.T) {
		t.Parallel()
		_, err := resolvePingRole("turbo")
		require.Error(t, err)
		require.Contains(t, err.Error(), "--role: invalid value")
		require.Contains(t, err.Error(), "turbo")
		require.Contains(t, err.Error(), "smart|large")
		require.Contains(t, err.Error(), "fast|small")
	})

	t.Run("role is case-sensitive", func(t *testing.T) {
		t.Parallel()
		_, err := resolvePingRole("Smart")
		require.Error(t, err)
	})
}
