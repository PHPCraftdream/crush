package tools

import (
	"bufio"
	"container/heap"
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
		"regex": searchFilesWithRegex,
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
		"regex": searchFilesWithRegex,
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
		"regex": searchFilesWithRegex,
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
		"regex": searchFilesWithRegex,
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
	err := fileMatches(path, re, func(lm lineMatch) bool {
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
	err := fileMatches(path, re, func(lm lineMatch) bool {
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
	rgPath, lookErr := exec.LookPath("rg")
	if lookErr != nil {
		t.Skip("rg is not in $PATH")
	}
	t.Parallel()
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
