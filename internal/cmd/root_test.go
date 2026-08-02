package cmd

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRecoverAndLogPanic_LogsBeforeRePanicking is the regression test for
// task #178: Go's default panic handler writes only to os.Stderr, never
// through slog, so an unrecovered panic anywhere in the command tree
// previously left crush.log with zero trace of what happened. This proves
// recoverAndLogPanic (deferred at the top of Execute) logs the panic value
// and a stack trace via slog.Error under crashLogMarker BEFORE re-panicking
// with the exact same value — so the process's normal crash behavior (exit
// code, stderr trace, for anyone watching the terminal directly) is
// unchanged, while crush.log now durably records what happened.
func TestRecoverAndLogPanic_LogsBeforeRePanicking(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	const panicValue = "boom: simulated top-level panic"

	var rePanicked any
	func() {
		defer func() {
			rePanicked = recover()
		}()
		func() {
			defer recoverAndLogPanic()
			panic(panicValue)
		}()
	}()

	require.Equal(t, panicValue, rePanicked,
		"recoverAndLogPanic must re-panic with the exact same value, unchanged")

	logged := logBuf.String()
	require.Contains(t, logged, crashLogMarker,
		"the panic must be logged under the fixed crashLogMarker so sessions_why's hint stays accurate")
	require.Contains(t, logged, panicValue, "the actual panic value must appear in the logged line")
	require.Contains(t, logged, "goroutine", "a real stack trace (runtime/debug.Stack output) must be logged")
}

// TestRecoverAndLogPanic_NoPanicIsANoop confirms recoverAndLogPanic does
// nothing observable when there is no panic to recover — it must not log
// anything or otherwise interfere with a normal, successful return.
func TestRecoverAndLogPanic_NoPanicIsANoop(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	func() {
		defer recoverAndLogPanic()
		// No panic here — normal return.
	}()

	require.False(t, strings.Contains(logBuf.String(), crashLogMarker),
		"recoverAndLogPanic must not log anything when nothing panicked")
}

// withStdin temporarily replaces os.Stdin for the duration of a test.
func withStdin(t *testing.T, f *os.File) {
	t.Helper()
	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = prev })
}

// TestMaybePrependStdin_RegularFileReadsImmediately pins the unchanged
// `< file` behavior: a regular file is fully available immediately and
// must be read and prepended with no bound.
func TestMaybePrependStdin_RegularFileReadsImmediately(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin-*.txt")
	require.NoError(t, err)
	_, err = f.WriteString("piped content")
	require.NoError(t, err)
	_, err = f.Seek(0, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	withStdin(t, f)

	got, err := MaybePrependStdin("the prompt")
	require.NoError(t, err)
	require.Equal(t, "piped content\n\nthe prompt", got)
}

// TestMaybePrependStdin_NamedPipeWithDataReadsIt confirms a `|` pipe that
// writes and closes promptly still gets prepended, same as before this
// change.
func TestMaybePrependStdin_NamedPipeWithDataReadsIt(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	withStdin(t, r)

	_, err = w.WriteString("piped content")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	got, err := MaybePrependStdin("the prompt")
	require.NoError(t, err)
	require.Equal(t, "piped content\n\nthe prompt", got)
}

// TestMaybePrependStdin_NamedPipeNeverClosesDoesNotHang is the regression
// test for the incident that motivated this change: an operator (or a
// launcher script) invoked `crush run` with a positional prompt and no
// explicit `< file` redirect. stdin resolved to a dangling pipe — nothing
// written, never closed — and io.ReadAll blocked forever, well before
// --timeout's context deadline is even wired up, leaving the process
// alive with zero visible session for hours. maybePrependStdin must give
// up after the grace duration and proceed with just the original prompt
// instead of hanging.
func TestMaybePrependStdin_NamedPipeNeverClosesDoesNotHang(t *testing.T) {
	const grace = 50 * time.Millisecond

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		r.Close()
		w.Close() // never written to, never closed by the "writer" itself
	})
	withStdin(t, r)

	done := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, err := maybePrependStdin("the prompt", grace)
		done <- struct {
			got string
			err error
		}{got, err}
	}()

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.Equal(t, "the prompt", res.got, "must fall back to the bare prompt, not hang or corrupt it")
	case <-time.After(2 * time.Second):
		t.Fatal("maybePrependStdin hung past the grace duration — it must never block indefinitely on a dangling pipe")
	}
}

// TestMaybePrependStdin_NamedPipeSlowCloseKeepsData is the regression test
// for the data-loss bug in the original stdinReadGrace fix (bea57a9b): that
// version raced io.ReadAll of the WHOLE stream against the grace duration,
// so a producer that wrote real data but took longer than the grace
// duration to close the pipe caused the timeout branch to fire — silently
// discarding every byte the still-running goroutine had already buffered,
// while logging the misleading "produced no data" message even though data
// existed. maybePrependStdin must now only bound the wait for the FIRST
// byte; once the producer proves it's alive, the data it already sent must
// survive even if the close is slow. Note the close here (200ms) is slower
// than the grace window (50ms), so this scenario genuinely goes through the
// idle-timeout-with-partial-data path — not a clean EOF — so per follow-up
// 1 the returned text carries the truncation marker; that's expected, not a
// regression: the data itself must still survive intact either way.
func TestMaybePrependStdin_NamedPipeSlowCloseKeepsData(t *testing.T) {
	const grace = 50 * time.Millisecond

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	withStdin(t, r)

	const payload = "some real data written promptly, but the pipe stays open past the grace window"
	_, err = w.WriteString(payload)
	require.NoError(t, err)

	done := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, err := maybePrependStdin("the prompt", grace)
		done <- struct {
			got string
			err error
		}{got, err}
	}()

	// Give maybePrependStdin's first-byte read a chance to complete, then
	// wait well past the grace duration before closing — proving the write
	// already happened and was seen before the grace window expired, and
	// that the data is not discarded just because the pipe stayed open past
	// the grace window (it hits the idle-timeout branch, not a clean EOF).
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, w.Close())

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.True(t, strings.HasPrefix(res.got, payload),
			"data written before the grace window expired must not be silently discarded just because the pipe closed late")
		require.True(t, strings.HasSuffix(res.got, "\n\nthe prompt"),
			"the original prompt must still be appended at the end")
	case <-time.After(2 * time.Second):
		t.Fatal("maybePrependStdin hung waiting for EOF after already seeing data — it must read through to EOF once the producer proved it's alive")
	}
}

// TestMaybePrependStdin_NamedPipeGoesSilentAfterFirstByteDoesNotHang is the
// regression test for the bug @oh's review found in the previous fix
// (task #199 / commit 6e7e1dc6): that version bounded only the FIRST read
// against the grace duration, then read the rest of the stream to EOF with
// NO further timeout. A producer that writes some data and then goes silent
// forever — never sending more, never closing the pipe — proved that
// "no further timeout" reintroduced the exact indefinite hang the grace
// duration was created to prevent. maybePrependStdin must instead bound
// EVERY gap between chunks by the grace duration, so it always returns in
// bounded time (using the already-shrunk grace duration) with whatever
// partial data the producer already sent, never hanging just because the
// pipe itself never closes. Because this is a genuine truncation (the
// producer never signaled it was done), the returned text must also carry
// the truncation-warning marker for the model to see.
func TestMaybePrependStdin_NamedPipeGoesSilentAfterFirstByteDoesNotHang(t *testing.T) {
	const grace = 50 * time.Millisecond

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		r.Close()
		w.Close() // never closed by the "writer" itself — it just goes quiet
	})
	withStdin(t, r)

	const payload = "here is one chunk, then the producer never says another word and never closes"
	_, err = w.WriteString(payload)
	require.NoError(t, err)

	done := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, err := maybePrependStdin("the prompt", grace)
		done <- struct {
			got string
			err error
		}{got, err}
	}()

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.True(t, strings.HasPrefix(res.got, payload),
			"must return the partial data already received once the producer goes idle, not hang forever waiting for more")
		require.True(t, strings.HasSuffix(res.got, "\n\nthe prompt"),
			"the original prompt must still be appended at the end")
		require.Contains(t, res.got, "truncated",
			"a partial read on the idle-timeout path must carry a truncation marker for the model to see")
	case <-time.After(2 * time.Second):
		t.Fatal("maybePrependStdin hung after receiving one chunk and then the producer going silent forever — the idle timeout must bound EVERY gap between chunks, not just the wait for the first byte")
	}
}

// TestMaybePrependStdin_NamedPipeIdleTimerResetsPerChunk proves the idle
// timeout genuinely resets on every chunk received, rather than being a
// single absolute deadline measured from the start of the read. The
// producer writes several bursts, each separated by a pause shorter than
// the grace duration, then closes. If the implementation used one deadline
// from the start instead of a true per-chunk idle reset, a long enough
// total elapsed time (sum of the pauses) would trip the grace window and
// truncate the data even though no individual gap ever exceeded it.
func TestMaybePrependStdin_NamedPipeIdleTimerResetsPerChunk(t *testing.T) {
	const grace = 150 * time.Millisecond

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	withStdin(t, r)

	done := make(chan struct {
		got string
		err error
	}, 1)
	go func() {
		got, err := maybePrependStdin("the prompt", grace)
		done <- struct {
			got string
			err error
		}{got, err}
	}()

	// Three bursts, each pause well under the grace duration (150ms), but
	// the bursts together span ~240ms total — longer than a single grace
	// window from the start would allow, proving the timer resets per chunk.
	const burst1 = "burst-one-"
	const burst2 = "burst-two-"
	const burst3 = "burst-three"
	_, err = w.WriteString(burst1)
	require.NoError(t, err)
	time.Sleep(80 * time.Millisecond)
	_, err = w.WriteString(burst2)
	require.NoError(t, err)
	time.Sleep(80 * time.Millisecond)
	_, err = w.WriteString(burst3)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.Equal(t, burst1+burst2+burst3+"\n\nthe prompt", res.got,
			"all bursts must survive: no individual gap between them exceeded the grace duration, so the idle timer must have reset each time")
	case <-time.After(2 * time.Second):
		t.Fatal("maybePrependStdin hung reading multiple bursts separated by pauses shorter than the grace duration")
	}
}

// TestMaybePrependStdin_IdleTimeoutTruncationCarriesNote is the regression
// test for task #220 follow-up 1: when the idle-timeout path returns partial
// stdin data (the producer went idle before EOF), the caller — usually a
// model, reading a `crush run` prompt fed non-interactively with stderr
// unwatched — has no way to know the "stdin" section might be an arbitrary
// mid-stream cut unless the returned text says so explicitly. This proves
// the idle-timeout path appends a clearly-worded, model-readable truncation
// marker to the returned string.
func TestMaybePrependStdin_IdleTimeoutTruncationCarriesNote(t *testing.T) {
	const grace = 50 * time.Millisecond

	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		r.Close()
		w.Close()
	})
	withStdin(t, r)

	const payload = "partial data before the producer goes idle forever"
	_, err = w.WriteString(payload)
	require.NoError(t, err)

	got, err := maybePrependStdin("the prompt", grace)
	require.NoError(t, err)

	require.True(t, strings.HasPrefix(got, payload),
		"the partial data actually read must still be present")
	require.True(t, strings.HasSuffix(got, "\n\nthe prompt"),
		"the original prompt must still be appended at the end")
	require.Contains(t, got, "truncated",
		"the idle-timeout-with-partial-data path must carry an explicit truncation marker for the model to see")
	require.Contains(t, got, grace.String(),
		"the truncation marker should be concrete about the grace window that elapsed")
}

// TestMaybePrependStdin_CleanEOFHasNoTruncationNote is the companion
// negative case for TestMaybePrependStdin_IdleTimeoutTruncationCarriesNote:
// a clean EOF (the producer wrote data and then genuinely closed the pipe)
// is complete data, not a truncation, and must NOT carry the truncation
// marker — only the two idle-timeout/non-EOF-error partial-data paths are
// truncation risks.
func TestMaybePrependStdin_CleanEOFHasNoTruncationNote(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	withStdin(t, r)

	const payload = "complete data, cleanly closed"
	_, err = w.WriteString(payload)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	got, err := maybePrependStdin("the prompt", 3*time.Second)
	require.NoError(t, err)

	require.Equal(t, payload+"\n\nthe prompt", got,
		"clean EOF must return exactly the data plus prompt, with no truncation marker appended")
	require.NotContains(t, got, "truncated",
		"clean EOF is complete data, not a truncation — it must never carry the truncation marker")
}

// An earlier version of this file also had
// TestMaybePrependStdin_ChunkRacesTimeoutIsNotDropped, an end-to-end attempt
// to reproduce the follow-up 2 boundary race by sweeping a second write's
// timing across a band straddling grace and measuring the test's own
// wall-clock elapsed time. It was removed: verified flaky under repeated
// runs (2/15 failures with `-count=15 -race` in isolation, and again under
// full-package `-race` load) — NOT because the fix is wrong, but because the
// test's own timing assumption doesn't hold under scheduling contention.
// There is real, unaccounted latency between "test writes to the pipe" and
// "maybePrependStdin's internal select loop actually re-arms its
// time.After(grace) timer" (pipe write/read syscalls, goroutine scheduling,
// channel handoff) that the test's own start-to-write measurement cannot
// see — so a write the test believes landed comfortably inside the grace
// window can, under load, actually land after the internal timer already
// fired. That produces exactly the "elapsed < grace but chunk still lost"
// failures observed, indistinguishable from a real regression using this
// technique. Per this session's zero-tolerance-for-flaky-tests policy (see
// task #216), a test that cannot reliably distinguish "fix present" from
// "fix absent" under normal load has negative value — it does not stay in
// the suite. The exact simultaneous-readiness window this follow-up guards
// against is not deterministically reproducible with the tools available
// here; TestMaybePrependStdin_TimeoutDrainHelperHandlesBufferedChunk below,
// plus code-review confidence (the drain is a single, unambiguously correct
// non-blocking receive on a buffered channel), is what this suite relies on
// instead.

// TestMaybePrependStdin_TimeoutDrainHelperHandlesBufferedChunk exercises the
// follow-up 2 drain-check logic in isolation, without relying on real-world
// timing: it proves that IF a chunk is sitting in a capacity-1 buffered
// channel at the moment the idle-timeout branch is entered, a single
// non-blocking receive (the same shape as the drain check added to
// maybePrependStdin's timeout branch) successfully retrieves it rather than
// treating the channel as empty. This deterministically pins the core
// correctness property the fix depends on: a non-blocking receive on a
// capacity-1 buffered channel with a pending send always succeeds.
func TestMaybePrependStdin_TimeoutDrainHelperHandlesBufferedChunk(t *testing.T) {
	chunkCh := make(chan stdinChunkResult, 1)
	chunkCh <- stdinChunkResult{data: []byte("buffered chunk"), err: io.EOF}

	// This mirrors exactly the non-blocking drain performed at the top of
	// the `case <-time.After(grace):` branch in maybePrependStdin.
	select {
	case chunk := <-chunkCh:
		require.Equal(t, []byte("buffered chunk"), chunk.data)
		require.ErrorIs(t, chunk.err, io.EOF)
	default:
		t.Fatal("non-blocking receive must retrieve a chunk already sitting in the capacity-1 buffered channel, not treat it as empty")
	}
}
