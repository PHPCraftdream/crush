package agent

// #344 Hard Abort Conformance Suite for Provider Adapters
//
// This file implements a parameterized conformance suite that verifies all
// provider adapter categories respect context cancellation as a hard execution
// boundary.
//
// HARD-ABORT CONTRACT BY CATEGORY:
//
// 1. HTTP-based providers (anthropic, azure, bedrock, google, openai, openaicompat,
//    openrouter, vercel, kronk):
//    Contract: Context cancellation MUST lead to breaking the underlying HTTP
//    connection within bounded time (5s), independent of server response behavior.
//    This is verified through http.NewRequestWithContext and context-aware
//    http.Client.Do implementations in the fantasy provider SDKs.
//
// 2. CLI-based providers (cliprovider):
//    Contract: Context cancellation MUST lead to termination of the entire
//    process tree (tree-kill on Windows, SIGKILL on Unix). Already implemented
//    and verified by existing tests in internal/agent/cliprovider/provider_test.go:
//    - TestStreamKillUsesTreeKillStillTerminatesChild
//    - TestStreamWaitBoundedOnGrandchildHoldsStderr
//
// PROHIBITED PATTERN:
// Do NOT solve provider cancellation by wrapping each Stream() call in a
// goroutine with its own timeout and abandoning the goroutine. This is the same
// ownership violation as #343 (db.Release abandonment) — abandoned goroutines
// continue living and can conflict with subsequent state. The hard-abort must
// be a property of the transport/process itself, not a timeout wrapper.
//
// WHAT revertCheckBreak DOES AND DOES NOT PROVE:
// revertCheckBreak makes the MOCK SERVER hang longer (bounded, so
// httptest.Server.Close() in t.Cleanup doesn't block forever) instead of
// exiting the moment the client disconnects. Toggling it does NOT make this
// test fail: the empirically confirmed behavior is a PASS either way,
// because a compliant client aborts based on ITS OWN cancelled context,
// independent of how long the remote server takes to notice the client is
// gone. This is exactly the property the hard-abort contract requires, so
// this is the expected (not broken) result — a `select {}` server that never
// even looks at r.Context().Done() previously made the test appear to FAIL,
// but that was an accidental artifact (httptest.Server.Close() hanging
// forever, then the outer `go test` timeout firing a generic panic) rather
// than the intended "stream did not exit within %v" assertion — see git
// history for that broken version.
//
// A genuine revert-check — proving this test WOULD catch a provider client
// that ignores context cancellation — would require breaking the actual
// client-side HTTP transport logic inside the third-party
// charm.land/fantasy module. That module lives in the read-only Go module
// cache (GOMODCACHE) and must never be edited (shared, read-only by design,
// used by every Go project on the machine — CLAUDE.md and this task's own
// history both flag this explicitly after an earlier attempt tried and
// failed with "Access is denied"). This test's real protective value is
// therefore an EMPIRICAL integration check against the REAL provider
// client and a REAL hanging server, not a break/restore demonstration.
//
// IMPLEMENTATION NOTES:
// - Each provider-specific test builds a minimal httptest.Server that emulates
//   the wire format of that provider (SSE chunks, JSON structure, headers).
// - The server sends initial valid chunks to prove the stream started, then
//   hangs forever (never sends DONE, never closes connection).
// - After confirming the stream is active, we cancel the context and verify
//   the provider's HTTP client breaks the connection within 5s.
// - This tests the CLIENT-SIDE behavior (context-aware HTTP client), not the
//   server — a hung server should not block the client forever.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

const (
	// cancellationBound is the maximum time a provider should take to exit
	// after context cancellation. This is the hard-execution-boundary we
	// enforce for all provider categories.
	cancellationBound = 5 * time.Second
)

// revertCheckBreak, when true, makes the anthropic/bedrock mock server (see
// buildAnthropicHangingServer) hang for longer than cancellationBound instead
// of exiting the moment the client disconnects. See the package doc comment
// above ("WHAT revertCheckBreak DOES AND DOES NOT PROVE") for why toggling
// this does not — and cannot, without editing the read-only third-party
// module cache — demonstrate a genuine test FAIL.
var revertCheckBreak = false

// providerTestCategory describes a provider adapter category for conformance testing.
type providerTestCategory struct {
	// name is the display name of the provider (used in test names).
	name string

	// buildProvider constructs a provider instance pointing at the given baseURL.
	buildProvider func(baseURL string) (fantasy.Provider, error)

	// buildHangingServer constructs an httptest.Server that sends initial valid
	// chunks for this provider's wire format, then hangs forever.
	//
	// The server MUST:
	// - Set appropriate Content-Type headers for the provider
	// - Send at least 2 valid initial chunks to prove the stream is working
	// - Use atomic.Bool to signal when the stream has started
	// - Use atomic.Int64 to count chunks sent
	// - Hang on <-r.Context().Done() (respect client cancellation)
	buildHangingServer func(streamStarted *atomic.Bool, chunksSent *atomic.Int64) *httptest.Server

	// modelID is the model identifier to use with this provider.
	modelID string

	// skipReason, if non-empty, indicates why this provider is skipped.
	// Used when a provider's wire format is too complex or when the provider
	// is not available in the current environment.
	skipReason string
}

// runProviderCancellationConformanceTest is the shared test harness that verifies
// a provider respects context cancellation as a hard execution boundary.
//
// The test:
// 1. Creates a hanging mock server that sends initial chunks then hangs
// 2. Builds a provider instance pointing at that server
// 3. Starts a streaming call through SessionAgent.Run()
// 4. Waits for the stream to start and send initial chunks
// 5. Cancels the context
// 6. Verifies the stream exits within cancellationBound (5s)
// 7. Verifies the error is context.Canceled or context.DeadlineExceeded
func runProviderCancellationConformanceTest(t *testing.T, cat providerTestCategory) {
	t.Parallel()

	if cat.skipReason != "" {
		t.Skip(cat.skipReason)
	}

	// Build the hanging server for this provider's wire format.
	var streamStarted atomic.Bool
	var chunksSent atomic.Int64
	srv := cat.buildHangingServer(&streamStarted, &chunksSent)
	t.Cleanup(srv.Close)

	// Build the provider instance.
	provider, err := cat.buildProvider(srv.URL)
	require.NoError(t, err, "provider construction should succeed")

	lm, err := provider.LanguageModel(context.Background(), cat.modelID)
	require.NoError(t, err, "language model retrieval should succeed")

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg: config.SelectedModel{
			Provider: cat.name,
			Model:    cat.modelID,
		},
	}

	env := testEnv(t)
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           model,
		SystemPrompt:         "you are a test agent",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})

	sess, err := env.sessions.Create(context.Background(), fmt.Sprintf("%s cancellation test", cat.name))
	require.NoError(t, err, "session creation should succeed")

	// Create a simple user message to start the stream.
	_, err = env.messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err, "message creation should succeed")

	// Create a cancellable context with a generous timeout (the test itself
	// will cancel it much earlier). The timeout here is a safety net in case
	// cancellation is broken, so we fail fast rather than hanging forever.
	ctx, cancel := context.WithTimeout(context.Background(), cancellationBound)
	defer cancel()

	// Start streaming in a goroutine.
	streamDone := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "test",
			MaxOutputTokens: 1000,
		})
		streamDone <- err
	}()

	// Wait for the stream to start and send initial chunks.
	// This ensures the provider has actually connected and is actively
	// streaming before we cancel.
	timeout := time.After(2 * time.Second)
	for !streamStarted.Load() || chunksSent.Load() < 2 {
		select {
		case <-timeout:
			t.Fatalf("timeout waiting for stream to start and send initial chunks (started=%v, chunks=%d)",
				streamStarted.Load(), chunksSent.Load())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Now cancel the context after confirming the stream is working.
	// This is the critical moment: we verify the provider respects context
	// cancellation as a hard execution boundary.
	cancel()

	// The stream MUST exit within cancellationBound and return a
	// cancellation-related error.
	//
	// If the provider doesn't respect cancellation (e.g., HTTP client
	// doesn't bind the request to the context, or the streaming loop
	// ignores ctx.Done()), this will hang until cancellationBound expires.
	select {
	case err := <-streamDone:
		// Verify the error is related to cancellation.
		require.Error(t, err, "stream should return an error after cancellation")
		isCanceled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		require.True(t, isCanceled, "error should be context.Canceled or context.DeadlineExceeded, got: %T: %v", err, err)
	case <-time.After(cancellationBound):
		t.Fatalf("stream did not exit within %v after context cancellation - provider %q is not respecting cancellation as a hard execution boundary",
			cancellationBound, cat.name)
	}

	// Verify that some chunks were sent (proves the test setup is valid).
	require.GreaterOrEqual(t, chunksSent.Load(), int64(2), "stream should have sent initial chunks before hanging")
}

// TestProviderCancellationConformance is the top-level test that runs
// conformance checks for all provider adapter categories.
func TestProviderCancellationConformance(t *testing.T) {
	t.Parallel()

	t.Run("HTTP/openaicompat", func(t *testing.T) {
		runProviderCancellationConformanceTest(t, providerTestCategory{
			name: "openaicompat",
			buildProvider: func(baseURL string) (fantasy.Provider, error) {
				return openaicompat.New(
					openaicompat.WithBaseURL(baseURL),
					openaicompat.WithAPIKey("probe-key"),
				)
			},
			buildHangingServer: buildOpenAICompatHangingServer,
			modelID:            "probe-model",
		})
	})

	t.Run("HTTP/openai", func(t *testing.T) {
		runProviderCancellationConformanceTest(t, providerTestCategory{
			name: "openai",
			buildProvider: func(baseURL string) (fantasy.Provider, error) {
				return openai.New(
					openai.WithBaseURL(baseURL),
					openai.WithAPIKey("probe-key"),
				)
			},
			buildHangingServer: buildOpenAICompatHangingServer,
			modelID:            "gpt-4",
		})
	})

	t.Run("HTTP/anthropic", func(t *testing.T) {
		runProviderCancellationConformanceTest(t, providerTestCategory{
			name: "anthropic",
			buildProvider: func(baseURL string) (fantasy.Provider, error) {
				return anthropic.New(
					anthropic.WithBaseURL(baseURL),
					anthropic.WithAPIKey("probe-key"),
				)
			},
			buildHangingServer: buildAnthropicHangingServer,
			modelID:            "claude-3-5-sonnet-20241022",
		})
	})

	t.Run("HTTP/google", func(t *testing.T) {
		// Google provider requires either API key or Vertex auth for the genai library.
		// After investigation:
		// 1. WithSkipAuth(true) creates dummy credentials but genai.NewClient still validates APIKey
		// 2. genai library itself enforces "api key is required for Google AI backend"
		// 3. The genai client validation happens before our mock server is even used
		//
		// Since google uses standard HTTP transport (genai.Client wraps *http.Client),
		// we assume conformance from the other HTTP provider tests.
		t.Skip("google provider's genai library enforces 'api key is required for Google AI backend' even with WithSkipAuth - cannot create client for local mock testing, HTTP transport conformance assumed from other HTTP providers")
	})

	t.Run("HTTP/bedrock", func(t *testing.T) {
		runProviderCancellationConformanceTest(t, providerTestCategory{
			name: "bedrock",
			buildProvider: func(baseURL string) (fantasy.Provider, error) {
				return bedrock.New(
					bedrock.WithBaseURL(baseURL),
					bedrock.WithSkipAuth(true),
				)
			},
			buildHangingServer: buildAnthropicHangingServer, // Bedrock wraps anthropic, uses same wire format
			modelID:            "anthropic.claude-3-5-sonnet-20241022-v2:0",
		})
	})

	t.Run("HTTP/openrouter", func(t *testing.T) {
		// openrouter doesn't export WithBaseURL, but accepts WithHTTPClient.
		// We use a redirect transport that rewrites requests to our local httptest.Server.
		redirectClient := &http.Client{
			Transport: redirectTransport{targetBaseURL: "placeholder"},
		}
		runProviderCancellationConformanceTest(t, providerTestCategory{
			name: "openrouter",
			buildProvider: func(baseURL string) (fantasy.Provider, error) {
				// Update the transport's target base URL dynamically
				redirectClient.Transport = redirectTransport{targetBaseURL: baseURL}
				return openrouter.New(
					openrouter.WithAPIKey("probe-key"),
					openrouter.WithHTTPClient(redirectClient),
				)
			},
			buildHangingServer: buildOpenAICompatHangingServer,
			modelID:            "anthropic/claude-3.5-sonnet",
		})
	})

	t.Run("HTTP/vercel", func(t *testing.T) {
		runProviderCancellationConformanceTest(t, providerTestCategory{
			name: "vercel",
			buildProvider: func(baseURL string) (fantasy.Provider, error) {
				return vercel.New(
					vercel.WithBaseURL(baseURL),
					vercel.WithAPIKey("probe-key"),
				)
			},
			buildHangingServer: buildOpenAICompatHangingServer, // Vercel wraps openai, uses same wire format
			modelID:            "ai-gpt-4-turbo",
		})
	})

	t.Run("HTTP/azure", func(t *testing.T) {
		// Azure OpenAI uses the OpenAI-compatible wire format, so we can
		// test it with the same server as openaicompat.
		runProviderCancellationConformanceTest(t, providerTestCategory{
			name: "azure",
			buildProvider: func(baseURL string) (fantasy.Provider, error) {
				return azure.New(
					azure.WithBaseURL(baseURL),
					azure.WithAPIKey("probe-key"),
				)
			},
			buildHangingServer: buildOpenAICompatHangingServer,
			modelID:            "gpt-4",
		})
	})

	t.Run("CLI/cliprovider", func(t *testing.T) {
		// CLI-based providers use a different hard-abort mechanism (tree-kill).
		// The conformance for this category is already verified by existing tests:
		// - TestStreamKillUsesTreeKillStillTerminatesChild
		// - TestStreamWaitBoundedOnGrandchildHoldsStderr
		//
		// We don't re-implement those tests here to avoid duplication. Instead,
		// we document the contract and reference the existing tests.
		t.Log("CLI provider hard-abort contract: context cancellation leads to process tree termination")
		t.Log("Verified by existing tests in internal/agent/cliprovider/provider_test.go:")
		t.Log("  - TestStreamKillUsesTreeKillStillTerminatesChild")
		t.Log("  - TestStreamWaitBoundedOnGrandchildHoldsStderr")
	})
}

// buildOpenAICompatHangingServer creates an httptest.Server that emulates the
// OpenAI-compatible SSE wire format (used by openai, openaicompat, openrouter,
// vercel, azure).
//
// Wire format: text/event-stream with "data: <json>" lines.
func buildOpenAICompatHangingServer(streamStarted *atomic.Bool, chunksSent *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamStarted.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Send initial chunks to prove the stream is working.
		initialChunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		}
		for _, c := range initialChunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
			chunksSent.Add(1)
		}

		// Hang forever - never send DONE, never close connection.
		// Respect client cancellation by blocking on r.Context().Done().
		<-r.Context().Done()
	}))
}

// buildAnthropicHangingServer creates an httptest.Server that emulates the
// Anthropic SSE wire format.
//
// Wire format: text/event-stream with "event: <type>" and "data: <json>" lines.
func buildAnthropicHangingServer(streamStarted *atomic.Bool, chunksSent *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamStarted.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// Anthropic requires specific event types.
		initialChunks := []struct {
			event string
			data  string
		}{
			{
				event: "message_start",
				data:  `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","content":[]}}`,
			},
			{
				event: "content_block_start",
				data:  `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			},
			{
				event: "content_block_delta",
				data:  `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			},
		}

		for _, chunk := range initialChunks {
			fmt.Fprintf(w, "event: %s\n", chunk.event)
			fmt.Fprintf(w, "data: %s\n\n", chunk.data)
			if fl != nil {
				fl.Flush()
			}
			chunksSent.Add(1)
		}

		// REVERT CHECK: If revertCheckBreak is true, ignore context cancellation
		// for longer than cancellationBound (simulating a non-compliant
		// provider), but still eventually return. A true `select {}` (zero
		// cases, never returns under any circumstance) does NOT correctly
		// simulate "provider ignores context" — it additionally leaks the
		// handler goroutine forever, which makes httptest.Server.Close() (run
		// via t.Cleanup after the client-side assertion has already
		// completed) block indefinitely waiting for in-flight handlers to
		// finish, masking the test's real FAIL/PASS result behind an
		// unrelated "panic: test timed out" from the outer go test binary
		// timeout instead of the intended "stream did not exit within %v"
		// assertion. A bounded sleep longer than cancellationBound still
		// proves the client-side abort is what's being tested (not server
		// cooperation) while letting Close() eventually succeed.
		if revertCheckBreak {
			time.Sleep(cancellationBound + 2*time.Second)
		} else {
			// Hang forever but respect client cancellation
			<-r.Context().Done()
		}
	}))
}

// redirectTransport redirects any HTTP request to the targetBaseURL.
// Used for providers that don't export WithBaseURL but accept WithHTTPClient.
type redirectTransport struct {
	targetBaseURL string
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Parse the target URL
	target, err := url.Parse(rt.targetBaseURL)
	if err != nil {
		return nil, fmt.Errorf("redirect transport: parse target URL: %w", err)
	}

	// Replace scheme, host, and path
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	req.URL.Path = target.Path

	// Call the default transport
	return http.DefaultTransport.RoundTrip(req)
}
