package tools

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRegexCache(t *testing.T) {
	cache := newRegexCache()

	// Test basic caching
	pattern := "test.*pattern"
	regex1, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to compile regex: %v", err)
	}

	regex2, err := cache.get(pattern)
	if err != nil {
		t.Fatalf("Failed to get cached regex: %v", err)
	}

	// Should be the same instance (cached)
	if regex1 != regex2 {
		t.Error("Expected cached regex to be the same instance")
	}

	// Test that it actually works
	if !regex1.MatchString("test123pattern") {
		t.Error("Regex should match test string")
	}
}

func TestGlobToRegexCaching(t *testing.T) {
	// Test that globToRegex uses pre-compiled regex
	pattern1 := globToRegex("*.{js,ts}")

	// Should not panic and should work correctly
	regex1, err := regexp.Compile(pattern1)
	if err != nil {
		t.Fatalf("Failed to compile glob regex: %v", err)
	}

	if !regex1.MatchString("test.js") {
		t.Error("Glob regex should match .js files")
	}
	if !regex1.MatchString("test.ts") {
		t.Error("Glob regex should match .ts files")
	}
	if regex1.MatchString("test.go") {
		t.Error("Glob regex should not match .go files")
	}
}

func TestGrepWithIgnoreFiles(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Create test files
	testFiles := map[string]string{
		"file1.txt":           "hello world",
		"file2.txt":           "hello world",
		"ignored/file3.txt":   "hello world",
		"node_modules/lib.js": "hello world",
		"secret.key":          "hello world",
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(tempDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	// Create .gitignore file
	gitignoreContent := "ignored/\n*.key\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".gitignore"), []byte(gitignoreContent), 0o644))

	// Create .crushignore file
	crushignoreContent := "node_modules/\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".crushignore"), []byte(crushignoreContent), 0o644))

	// Test both implementations
	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(t.Context(), pattern, path, include)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			matches, _, err := searchWithRipgrep(t.Context(), pattern, path, include, 100)
			return matches, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("hello world", tempDir, "")
			require.NoError(t, err)

			// Convert matches to a set of file paths for easier testing
			foundFiles := make(map[string]bool)
			for _, match := range matches {
				foundFiles[filepath.Base(match.path)] = true
			}

			// Should find file1.txt and file2.txt
			require.True(t, foundFiles["file1.txt"], "Should find file1.txt")
			require.True(t, foundFiles["file2.txt"], "Should find file2.txt")

			// Should NOT find ignored files
			require.False(t, foundFiles["file3.txt"], "Should not find file3.txt (ignored by .gitignore)")
			require.False(t, foundFiles["lib.js"], "Should not find lib.js (ignored by .crushignore)")
			require.False(t, foundFiles["secret.key"], "Should not find secret.key (ignored by .gitignore)")

			// Should find exactly 2 matches
			require.Equal(t, 2, len(matches), "Should find exactly 2 matches")
		})
	}
}

func TestSearchImplementations(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	for path, content := range map[string]string{
		"file1.go":         "package main\nfunc main() {\n\tfmt.Println(\"hello world\")\n}",
		"file2.js":         "console.log('hello world');",
		"file3.txt":        "hello world from text file",
		"binary.exe":       "\x00\x01\x02\x03",
		"empty.txt":        "",
		"subdir/nested.go": "package nested\n// hello world comment",
		".hidden.txt":      "hello world in hidden file",
		"file4.txt":        "hello world from a banana",
		"file5.txt":        "hello world from a grape",
	} {
		fullPath := filepath.Join(tempDir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}

	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".gitignore"), []byte("file4.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, ".crushignore"), []byte("file5.txt\n"), 0o644))

	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(t.Context(), pattern, path, include)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			matches, _, err := searchWithRipgrep(t.Context(), pattern, path, include, 100)
			return matches, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("hello world", tempDir, "")
			require.NoError(t, err)

			require.Equal(t, len(matches), 4)
			for _, match := range matches {
				require.NotEmpty(t, match.path)
				require.NotZero(t, match.lineNum)
				require.NotEmpty(t, match.lineText)
				require.NotZero(t, match.modTime)
				require.NotContains(t, match.path, ".hidden.txt")
				require.NotContains(t, match.path, "file4.txt")
				require.NotContains(t, match.path, "file5.txt")
				require.NotContains(t, match.path, "binary.exe")
			}
		})
	}
}

// Benchmark to show performance improvement
func BenchmarkRegexCacheVsCompile(b *testing.B) {
	cache := newRegexCache()
	pattern := "test.*pattern.*[0-9]+"

	b.Run("WithCache", func(b *testing.B) {
		for b.Loop() {
			_, err := cache.get(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("WithoutCache", func(b *testing.B) {
		for b.Loop() {
			_, err := regexp.Compile(pattern)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestIsTextFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		filename string
		content  []byte
		wantText bool
	}{
		{
			name:     "go file",
			filename: "test.go",
			content:  []byte("package main\n\nfunc main() {}\n"),
			wantText: true,
		},
		{
			name:     "yaml file",
			filename: "config.yaml",
			content:  []byte("key: value\nlist:\n  - item1\n  - item2\n"),
			wantText: true,
		},
		{
			name:     "yml file",
			filename: "config.yml",
			content:  []byte("key: value\n"),
			wantText: true,
		},
		{
			name:     "json file",
			filename: "data.json",
			content:  []byte(`{"key": "value"}`),
			wantText: true,
		},
		{
			name:     "javascript file",
			filename: "script.js",
			content:  []byte("console.log('hello');\n"),
			wantText: true,
		},
		{
			name:     "typescript file",
			filename: "script.ts",
			content:  []byte("const x: string = 'hello';\n"),
			wantText: true,
		},
		{
			name:     "markdown file",
			filename: "README.md",
			content:  []byte("# Title\n\nSome content\n"),
			wantText: true,
		},
		{
			name:     "shell script",
			filename: "script.sh",
			content:  []byte("#!/bin/bash\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "python file",
			filename: "script.py",
			content:  []byte("print('hello')\n"),
			wantText: true,
		},
		{
			name:     "xml file",
			filename: "data.xml",
			content:  []byte("<?xml version=\"1.0\"?>\n<root></root>\n"),
			wantText: true,
		},
		{
			name:     "plain text",
			filename: "file.txt",
			content:  []byte("plain text content\n"),
			wantText: true,
		},
		{
			name:     "css file",
			filename: "style.css",
			content:  []byte("body { color: red; }\n"),
			wantText: true,
		},
		{
			name:     "scss file",
			filename: "style.scss",
			content:  []byte("$primary: blue;\nbody { color: $primary; }\n"),
			wantText: true,
		},
		{
			name:     "sass file",
			filename: "style.sass",
			content:  []byte("$primary: blue\nbody\n  color: $primary\n"),
			wantText: true,
		},
		{
			name:     "rust file",
			filename: "main.rs",
			content:  []byte("fn main() {\n    println!(\"Hello, world!\");\n}\n"),
			wantText: true,
		},
		{
			name:     "zig file",
			filename: "main.zig",
			content:  []byte("const std = @import(\"std\");\npub fn main() void {}\n"),
			wantText: true,
		},
		{
			name:     "java file",
			filename: "Main.java",
			content:  []byte("public class Main {\n    public static void main(String[] args) {}\n}\n"),
			wantText: true,
		},
		{
			name:     "c file",
			filename: "main.c",
			content:  []byte("#include <stdio.h>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "cpp file",
			filename: "main.cpp",
			content:  []byte("#include <iostream>\nint main() { return 0; }\n"),
			wantText: true,
		},
		{
			name:     "fish shell",
			filename: "script.fish",
			content:  []byte("#!/usr/bin/env fish\necho 'hello'\n"),
			wantText: true,
		},
		{
			name:     "powershell file",
			filename: "script.ps1",
			content:  []byte("Write-Host 'Hello, World!'\n"),
			wantText: true,
		},
		{
			name:     "cmd batch file",
			filename: "script.bat",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "cmd file",
			filename: "script.cmd",
			content:  []byte("@echo off\necho Hello, World!\n"),
			wantText: true,
		},
		{
			name:     "binary exe",
			filename: "binary.exe",
			content:  []byte{0x4D, 0x5A, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "png image",
			filename: "image.png",
			content:  []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			wantText: false,
		},
		{
			name:     "jpeg image",
			filename: "image.jpg",
			content:  []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46},
			wantText: false,
		},
		{
			name:     "zip archive",
			filename: "archive.zip",
			content:  []byte{0x50, 0x4B, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00},
			wantText: false,
		},
		{
			name:     "pdf file",
			filename: "document.pdf",
			content:  []byte("%PDF-1.4\n%âãÏÓ\n"),
			wantText: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			filePath := filepath.Join(tempDir, tt.filename)
			require.NoError(t, os.WriteFile(filePath, tt.content, 0o644))

			got := isTextFile(filePath)
			require.Equal(t, tt.wantText, got, "isTextFile(%s) = %v, want %v", tt.filename, got, tt.wantText)
		})
	}
}

func TestMultipleMatchesPerFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// "world" appears on lines 1 and 3, but not line 2. Both grep
	// implementations must report every matching line, not just the first.
	content := "Hello world.\nHello.\nHello world.\n"
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "file.txt"), []byte(content), 0o644))

	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(t.Context(), pattern, path, include)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			matches, _, err := searchWithRipgrep(t.Context(), pattern, path, include, 100)
			return matches, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("world", tempDir, "")
			require.NoError(t, err)
			require.Len(t, matches, 2, "should report both matching lines")

			lines := make([]int, len(matches))
			for i, match := range matches {
				lines[i] = match.lineNum
				require.Equal(t, 7, match.charNum)
				require.Equal(t, "Hello world.", match.lineText)
			}
			require.ElementsMatch(t, []int{1, 3}, lines)
		})
	}
}

func TestColumnMatch(t *testing.T) {
	t.Parallel()

	// Test both implementations
	for name, fn := range map[string]func(pattern, path, include string) ([]grepMatch, error){
		"regex": func(pattern, path, include string) ([]grepMatch, error) {
			return searchFilesWithRegex(t.Context(), pattern, path, include)
		},
		"rg": func(pattern, path, include string) ([]grepMatch, error) {
			matches, _, err := searchWithRipgrep(t.Context(), pattern, path, include, 100)
			return matches, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if name == "rg" && getRg() == "" {
				t.Skip("rg is not in $PATH")
			}

			matches, err := fn("THIS", "./testdata/", "")
			require.NoError(t, err)
			require.Len(t, matches, 1)
			match := matches[0]
			require.Equal(t, 2, match.lineNum)
			require.Equal(t, 14, match.charNum)
			require.Equal(t, "I wanna grep THIS particular word", match.lineText)
			require.Equal(t, "testdata/grep.txt", filepath.ToSlash(filepath.Clean(match.path)))
		})
	}
}

// TestBoundedMatchHeapRetainsNewest verifies the bounded heap keeps exactly the
// K newest matches by modTime (ties broken by earliest insertion order) when
// more than K matches are fed in.
func TestBoundedMatchHeapRetainsNewest(t *testing.T) {
	t.Parallel()
	h := &boundedMatchHeap{}
	limit := 10
	base := time.Now()

	for i := 0; i < 100; i++ {
		gm := grepMatch{
			path:    fmt.Sprintf("file_%d.txt", i),
			modTime: base.Add(time.Duration(i) * time.Minute),
			seq:     int64(i + 1),
		}
		if h.Len() < limit {
			heap.Push(h, gm)
		} else if !evictFirst(gm, (*h)[0]) {
			(*h)[0] = gm
			heap.Fix(h, 0)
		}
	}

	require.Equal(t, limit, h.Len(), "heap must never exceed limit")

	matches := []grepMatch(*h)
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].modTime.After(matches[j].modTime)
		}
		return matches[i].seq < matches[j].seq
	})

	// Should be the 10 newest (indices 90-99), sorted newest-first.
	for i, m := range matches {
		expected := 99 - i
		require.Equal(t, fmt.Sprintf("file_%d.txt", expected), m.path,
			"position %d should be file_%d", i, expected)
	}
}

// TestBoundedMatchHeapStableTiebreak verifies that when multiple matches share
// the same modTime, earlier-inserted ones (smaller seq = earlier line) survive
// eviction over later ones within the same modTime group.
func TestBoundedMatchHeapStableTiebreak(t *testing.T) {
	t.Parallel()
	h := &boundedMatchHeap{}
	limit := 2
	mt := time.Now()

	// 3 matches, same modTime, increasing seq (line order).
	for i := 0; i < 3; i++ {
		gm := grepMatch{
			path:    fmt.Sprintf("f%d", i),
			modTime: mt,
			seq:     int64(i + 1),
		}
		if h.Len() < limit {
			heap.Push(h, gm)
		} else if !evictFirst(gm, (*h)[0]) {
			(*h)[0] = gm
			heap.Fix(h, 0)
		}
	}

	require.Equal(t, limit, h.Len())
	// Should keep seq 1 and 2 (earliest), NOT seq 3.
	seqs := map[int64]bool{}
	for _, m := range *h {
		seqs[m.seq] = true
	}
	require.True(t, seqs[1], "seq=1 must survive (earliest line)")
	require.True(t, seqs[2], "seq=2 must survive")
	require.False(t, seqs[3], "seq=3 must be evicted (latest line, same modTime)")
}

// TestFileMatchesCallbackStopsEarly verifies fileMatches stops reading the file
// immediately when the callback returns false, rather than scanning the entire
// file. This tests the regex fallback path (no ripgrep dependency).
func TestFileMatchesCallbackStopsEarly(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Create a file with 10000 matching lines.
	var content strings.Builder
	for i := 0; i < 10000; i++ {
		content.WriteString("match line\n")
	}
	path := filepath.Join(tempDir, "big.txt")
	require.NoError(t, os.WriteFile(path, []byte(content.String()), 0o644))

	re := regexp.MustCompile("match")
	callCount := 0
	err := fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		callCount++
		return false // stop immediately after first match
	})
	require.NoError(t, err)
	require.Equal(t, 1, callCount,
		"callback must be called exactly once when it returns false on the first match")
}

// TestFileMatchesCallbackCollectsAll verifies the callback path still finds all
// matches when the callback always returns true.
func TestFileMatchesCallbackCollectsAll(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	content := "match one\nxyz\nmatch two\nmatch three\n"
	path := filepath.Join(tempDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	re := regexp.MustCompile("match")
	var collected []lineMatch
	err := fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		collected = append(collected, lm)
		return true
	})
	require.NoError(t, err)
	require.Len(t, collected, 3)
	require.Equal(t, 1, collected[0].lineNum)
	require.Equal(t, 3, collected[1].lineNum)
	require.Equal(t, 4, collected[2].lineNum)
}

// TestScanThenWaitPattern_DrainsOnScanErrorInsteadOfHanging is a regression
// test for a deadlock confirmed by direct reproduction against the exact
// scan-then-Wait shape searchWithRipgrep uses.
//
// rg --json emits one JSON object per line, and that line embeds the ENTIRE
// matched source line. A single matched line long enough to exceed the
// scanner's 4 MiB buffer (a minified bundle, a base64 blob, any
// pathologically long line — a realistic real-world scenario, not
// contrived) makes bufio.Scanner return bufio.ErrTooLong and stop — before
// rg has necessarily finished writing the rest of its output. Per os/exec's
// documented contract, calling Wait() before all reads from the pipe
// complete can deadlock once the child blocks writing to a full OS pipe
// buffer with nobody draining it. Confirmed directly with a standalone
// reproduction of this exact pattern (scan loop, then bare cmd.Wait() with
// no drain-on-error): Wait() hung indefinitely (well past an 8s timeout)
// against a real `rg --json` invocation over a ~6 MiB single-line file.
// searchWithRipgrep's fix drains the pipe (io.Copy to io.Discard) when
// scanner.Err() is non-nil, before calling Wait — this test proves that
// exact pattern is deadlock-free using the SAME real `rg` binary and the
// SAME oversized-line scenario.
//
// This test does NOT call searchWithRipgrep directly: getRgSearchCmd goes
// through getRg(), which is a sync.OnceValue that unconditionally returns
// "" whenever testing.Testing() is true (see rg.go) — a pre-existing,
// deliberate test-time guard that means searchWithRipgrep can never
// actually invoke a real rg process under `go test`, by design. Bypassing
// that package-wide memoized guard just for this one test would be a
// larger, riskier change than the deadlock fix itself, so this test
// exercises the identical scan-then-drain-then-Wait sequence standalone
// against a real rg process resolved via exec.LookPath, which is exactly
// what triggers and then resolves the deadlock — the file-format/JSON
// parsing differences between this and searchWithRipgrep are immaterial to
// what's being proven (the pipe-draining contract).
func TestScanThenWaitPattern_DrainsOnScanErrorInsteadOfHanging(t *testing.T) {
	t.Parallel()
	rgPath, lookErr := exec.LookPath("rg")
	if lookErr != nil {
		t.Skip("rg is not in $PATH")
	}
	tempDir := t.TempDir()

	// One line comfortably past the 4 MiB scanner buffer, followed by
	// several short matching lines that would remain unread in the pipe if
	// the scan loop stopped without draining.
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 6*1024*1024))
	b.WriteString(" NEEDLE_OVERSIZED_LINE\n")
	for i := range 50 {
		fmt.Fprintf(&b, "short line %d NEEDLE_OVERSIZED_LINE\n", i)
	}
	path := filepath.Join(tempDir, "huge.txt")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))

	runOnce := func(drainOnScanError bool) error {
		cmd := exec.CommandContext(t.Context(), rgPath, "--json", "NEEDLE_OVERSIZED_LINE", path)
		stdout, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
		}
		if drainOnScanError && scanner.Err() != nil {
			_, _ = io.Copy(io.Discard, stdout)
		}
		return cmd.Wait()
	}

	t.Run("with_drain_returns_promptly", func(t *testing.T) {
		t.Parallel()
		done := make(chan error, 1)
		go func() { done <- runOnce(true) }()
		select {
		case <-done:
			// rg exits fine either way; we only care that Wait() returned.
		case <-time.After(15 * time.Second):
			t.Fatal("Wait() hung even with the drain-on-scan-error fix applied")
		}
	})

	t.Run("without_drain_hangs_confirming_the_bug_shape", func(t *testing.T) {
		t.Parallel()
		done := make(chan error, 1)
		go func() { done <- runOnce(false) }()
		select {
		case <-done:
			t.Fatal("expected Wait() to hang without the drain (bug reproduction did not trigger — " +
				"the oversized-line/pipe-buffer scenario may no longer apply on this platform)")
		case <-time.After(5 * time.Second):
			// Expected: this proves the bug is real absent the fix, so the
			// "with_drain" subtest above is actually testing something.
		}
	})
}

// TestSearchFilesCancelledContextSkipsFallback verifies that when the context
// is already cancelled before searchFiles is called, the regex fallback walk is
// NOT launched: the ripgrep "not found" error is returned as-is rather than the
// walk running and surfacing context.Canceled.
func TestSearchFilesCancelledContextSkipsFallback(t *testing.T) {
	t.Parallel()
	// A tree large enough that a full fallback walk would be observably slow.
	tempDir := t.TempDir()
	for i := range 4000 {
		p := filepath.Join(tempDir, fmt.Sprintf("d%d", i/100), fmt.Sprintf("f%d.txt", i))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("needle line\n"), 0o644))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call

	start := time.Now()
	matches, truncated, err := searchFiles(ctx, "needle", tempDir, "", 100)
	elapsed := time.Since(start)

	require.Error(t, err)
	// The guard short-circuits: the ripgrep "not found" error is returned
	// as-is. Had the fallback run, it would surface context.Canceled instead.
	require.False(t, errors.Is(err, context.Canceled), "fallback must not run, got %v", err)
	require.False(t, errors.Is(err, context.DeadlineExceeded), "fallback must not run, got %v", err)
	require.Contains(t, err.Error(), "ripgrep", "the ripgrep error must be returned directly, proving the fallback was skipped")
	require.Empty(t, matches)
	require.False(t, truncated)
	t.Logf("cancelled searchFiles returned in %s (err=%v)", elapsed, err)
}

// TestFileMatchesBoundedLineTruncatesHugeLine verifies that a single line with
// no newline, several MiB long, does not force an unbounded allocation: it is
// truncated to the cap, marked, and matched without crashing or hanging.
func TestFileMatchesBoundedLineTruncatesHugeLine(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// ~6 MiB single line, needle within the retained (first 4 MiB) portion.
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 1024*1024))   // 1 MiB prefix
	b.WriteString("NEEDLE")                         // match, well under the 4 MiB cap
	b.WriteString(strings.Repeat("y", 5*1024*1024)) // 5 MiB suffix, no newline
	huge := b.String()
	path := filepath.Join(tempDir, "huge.txt")
	require.NoError(t, os.WriteFile(path, []byte(huge), 0o644))

	re := regexp.MustCompile("NEEDLE")
	var got []lineMatch
	require.NoError(t, fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		got = append(got, lm)
		return true
	}))

	require.Len(t, got, 1, "the single huge line must be matched exactly once")
	require.LessOrEqual(t, len(got[0].lineText), maxFallbackLineBytes+len(fallbackTruncateSuffix),
		"truncated line must not hold the full oversized line (%d bytes)", len(huge))
	require.True(t, strings.HasSuffix(got[0].lineText, fallbackTruncateSuffix),
		"truncated line must carry the truncation marker")
	t.Logf("huge-line match lineText length=%d (full line=%d)", len(got[0].lineText), len(huge))
}

// TestFileMatchesBoundedLineContinuesAfterTruncation verifies that after a
// truncated (oversized) line the scanner keeps scanning subsequent lines — the
// key advantage over bufio.Scanner, which stops dead on ErrTooLong.
func TestFileMatchesBoundedLineContinuesAfterTruncation(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	var b strings.Builder
	// Line 1: oversized, needle in the retained portion.
	b.WriteString(strings.Repeat("x", 1024*1024))
	b.WriteString("NEEDLE")
	b.WriteString(strings.Repeat("y", 5*1024*1024))
	b.WriteString("\n")
	// Line 2: normal line, also matches.
	b.WriteString("NEEDLE on a short line\n")
	path := filepath.Join(tempDir, "huge3.txt")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))

	re := regexp.MustCompile("NEEDLE")
	var got []lineMatch
	require.NoError(t, fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		got = append(got, lm)
		return true
	}))

	require.Len(t, got, 2, "must continue scanning after a truncated line")
	require.Equal(t, 1, got[0].lineNum)
	require.True(t, strings.HasSuffix(got[0].lineText, fallbackTruncateSuffix))
	require.Equal(t, 2, got[1].lineNum)
	require.Equal(t, "NEEDLE on a short line", got[1].lineText)
}

// TestSearchFilesWithRegexRespectsMidWalkCancellation verifies that cancelling
// the context while the fallback walk is in progress aborts it promptly
// (surfacing context.Canceled) instead of grinding through the whole tree. The
// searched pattern does NOT match the file content so the 200-match cap never
// triggers an early stop — the walk must traverse every file, giving
// cancellation something real to interrupt mid-flight.
func TestSearchFilesWithRegexRespectsMidWalkCancellation(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	const files = 4000
	content := strings.Repeat("filler line\n", 160) // ~1.9 KiB/file, no match for the pattern below
	for i := range files {
		p := filepath.Join(tempDir, fmt.Sprintf("d%d", i/200), fmt.Sprintf("f%d.txt", i))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	const missingPattern = "ZZQ_NO_SUCH_NEEDLE_ZZQ"

	// Baseline: a full, uninterrupted walk over the whole tree.
	fullStart := time.Now()
	_, fullErr := searchFilesWithRegex(context.Background(), missingPattern, tempDir, "")
	fullElapsed := time.Since(fullStart)
	require.NoError(t, fullErr)
	t.Logf("full walk of %d files took %s", files, fullElapsed)

	// Cancel mid-walk: wait a quarter of the measured full-walk duration, then
	// cancel, so the walk is guaranteed to be in progress when it arrives.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := searchFilesWithRegex(ctx, missingPattern, tempDir, "")
		done <- err
	}()
	time.Sleep(fullElapsed / 4)
	cancelStart := time.Now()
	cancel()

	select {
	case err := <-done:
		abortElapsed := time.Since(cancelStart)
		require.Error(t, err)
		require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
		t.Logf("mid-walk cancellation returned %s after cancel (full walk %s)", abortElapsed, fullElapsed)
		// Must abort shortly after cancellation, not grind to the end.
		require.Less(t, abortElapsed, fullElapsed/2,
			"walk should abort shortly after cancellation (abort=%s, full=%s)", abortElapsed, fullElapsed)
	case <-time.After(5 * time.Second):
		t.Fatal("searchFilesWithRegex did not return within 5s after cancellation")
	}
}

// TestFileMatchesHonoursDeadlineMidHugeLine proves the regex fallback aborts a
// single multi-MiB line (no '\n') promptly when the context deadline passes,
// rather than reading the whole line and returning nil. fileMatches checks ctx
// only between lines, so with one line that check never fires — the fix lives
// inside readBoundedLine. Timing is measured relative to a full-read baseline
// on this machine, so the assertion is machine-independent.
func TestFileMatchesHonoursDeadlineMidHugeLine(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// A single line, no newline, several MiB. The pattern never matches, so
	// the entire line is scanned (capped at maxFallbackLineBytes, then bytes
	// discarded) before fileMatches can move on.
	huge := strings.Repeat("x", 16*1024*1024) // 16 MiB
	path := filepath.Join(tempDir, "huge.txt")
	require.NoError(t, os.WriteFile(path, []byte(huge), 0o644))

	re := regexp.MustCompile("ZZQ_NO_SUCH_NEEDLE_ZZQ")

	// Baseline: an uninterrupted full read of the single huge line.
	fullStart := time.Now()
	require.NoError(t, fileMatches(context.Background(), path, re, func(lineMatch) bool { return true }))
	fullElapsed := time.Since(fullStart)
	t.Logf("full read of 16 MiB single-line file took %s", fullElapsed)

	// Deadline short enough that the read must still be in progress.
	deadline := fullElapsed / 4
	if deadline < 5*time.Millisecond {
		deadline = 5 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- fileMatches(ctx, path, re, func(lineMatch) bool { return true })
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"fileMatches must honour the deadline mid-line, got %v", err)
		require.Less(t, elapsed, fullElapsed,
			"fileMatches should abort near the deadline (%s), not after a full read (%s)", deadline, fullElapsed)
		t.Logf("aborted %s after start (deadline %s, full read %s)", elapsed, deadline, fullElapsed)
	case <-time.After(10 * time.Second):
		t.Fatal("fileMatches did not return within 10s")
	}
}

// slowEndlessLineReader yields an endless stream of non-newline bytes,
// sleeping a fixed interval on each Read. It models a single pathological
// line with no '\n' that takes wall-clock time to read, letting a unit test
// assert cancellation latency deterministically rather than depending on
// disk/CPU speed.
type slowEndlessLineReader struct {
	chunkInterval time.Duration
	chunk         []byte
}

func (r *slowEndlessLineReader) Read(p []byte) (int, error) {
	time.Sleep(r.chunkInterval)
	return copy(p, r.chunk), nil
}

// TestReadBoundedLineHonoursContextCancellation proves readBoundedLine reacts
// to a cancelled context mid-line (every ~64 KiB) rather than only between
// lines. Against a single pathological line with no newline — which never lets
// fileMatches' per-line check fire — the in-line cadence is the only thing
// that bounds cancellation latency.
func TestReadBoundedLineHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	r := &slowEndlessLineReader{
		chunkInterval: 2 * time.Millisecond,
		chunk:         bytes.Repeat([]byte{'x'}, 4096),
	}
	br := bufio.NewReader(r)
	var buf bytes.Buffer

	done := make(chan error, 1)
	go func() {
		_, err := readBoundedLine(ctx, br, &buf, maxFallbackLineBytes)
		done <- err
	}()

	// Let it start producing bytes, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled,
			"readBoundedLine must surface the context error once cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("readBoundedLine did not return within 2s of cancellation " +
			"(it read the whole pathological line instead of honouring ctx)")
	}
}

// TestFileMatchesDoesNotReportSpuriousMatchOnCancelledPartialLine proves that
// when readBoundedLine surfaces a non-EOF error (context cancellation fired
// mid-line by its in-line ~64 KiB cadence, see #135), fileMatches must not
// call onMatch against the partial line buffered so far — even when a real
// pattern match sits in the bytes already read. Before the fix, fileMatches
// ran pattern.FindStringIndex on the partial line before checking rerr, so a
// match in the already-read prefix of a huge, never-terminated line was
// reported right before the cancellation error was returned.
func TestFileMatchesDoesNotReportSpuriousMatchOnCancelledPartialLine(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// A single line, no newline, with a real match in its first bytes,
	// followed by many MiB of filler. readBoundedLine buffers the match
	// (and returns truncated=true well before EOF) long before the whole
	// line — or a full read — completes, so a short deadline is guaranteed
	// to land mid-line, after the match is already sitting in lineBuf.
	const needle = "needle-match-XYZ"
	huge := needle + " " + strings.Repeat("x", 16*1024*1024) // 16 MiB
	path := filepath.Join(tempDir, "huge-with-early-match.txt")
	require.NoError(t, os.WriteFile(path, []byte(huge), 0o644))

	re := regexp.MustCompile(needle)

	// Baseline: an uninterrupted full read of the single huge line.
	fullStart := time.Now()
	var baselineCalls int
	require.NoError(t, fileMatches(context.Background(), path, re, func(lineMatch) bool {
		baselineCalls++
		return true
	}))
	fullElapsed := time.Since(fullStart)
	require.Equal(t, 1, baselineCalls, "sanity check: the needle must be a real, findable match")
	t.Logf("full read of 16 MiB single-line file took %s", fullElapsed)

	// Deadline short enough that the read must still be in progress, so
	// readBoundedLine's mid-line cadence (every ~64 KiB) fires the
	// cancellation while the needle is already buffered but the line isn't
	// finished (no io.EOF yet).
	deadline := fullElapsed / 4
	if deadline < 5*time.Millisecond {
		deadline = 5 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	var onMatchCalls int
	done := make(chan error, 1)
	go func() {
		done <- fileMatches(ctx, path, re, func(lineMatch) bool {
			onMatchCalls++
			return true
		})
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"fileMatches must surface the cancellation error, got %v", err)
		require.Equal(t, 0, onMatchCalls,
			"a match found only in a partial, not-fully-read line must not reach onMatch "+
				"when the read was aborted by context cancellation")
	case <-time.After(10 * time.Second):
		t.Fatal("fileMatches did not return within 10s of the context deadline")
	}
}
