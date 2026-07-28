package tools

import (
	"bufio"
	"cmp"
	"container/heap"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/stringext"
)

// regexCache provides thread-safe caching of compiled regex patterns
type regexCache struct {
	*csync.Map[string, *regexp.Regexp]
}

// newRegexCache creates a new regex cache
func newRegexCache() *regexCache {
	return &regexCache{
		Map: csync.NewMap[string, *regexp.Regexp](),
	}
}

// get retrieves a compiled regex from cache or compiles and caches it
func (rc *regexCache) get(pattern string) (*regexp.Regexp, error) {
	re, ok := rc.Get(pattern)
	if ok && re != nil {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	rc.Set(pattern, re)
	return re, nil
}

// ResetCache clears compiled regex caches to prevent unbounded growth across sessions.
func ResetCache() {
	searchRegexCache.Reset(map[string]*regexp.Regexp{})
	globRegexCache.Reset(map[string]*regexp.Regexp{})
}

// Global regex cache instances
var (
	searchRegexCache = newRegexCache()
	globRegexCache   = newRegexCache()
	// Pre-compiled regex for glob conversion (used frequently)
	globBraceRegex = regexp.MustCompile(`\{([^}]+)\}`)
)

type GrepParams struct {
	Pattern     string `json:"pattern" description:"The regex pattern to search for in file contents"`
	Path        string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include     string `json:"include,omitempty" description:"File pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText bool   `json:"literal_text,omitempty" description:"If true, the pattern will be treated as literal text with special regex characters escaped. Default is false."`
}

type grepMatch struct {
	path     string
	modTime  time.Time
	lineNum  int
	charNum  int
	lineText string
	seq      int64 // insertion order, for stable sort tie-breaking
}

// boundedMatchHeap is a min-heap keyed by eviction priority: the root is the
// match that should be discarded FIRST when the heap is full — the one with
// the oldest modTime, and among ties the one inserted latest (largest seq).
// This retains the top-K newest matches while preserving line order within
// ties (same modTime), matching the previous sort.SliceStable behaviour.
type boundedMatchHeap []grepMatch

func (h boundedMatchHeap) Len() int { return len(h) }
func (h boundedMatchHeap) Less(i, j int) bool {
	if !h[i].modTime.Equal(h[j].modTime) {
		return h[i].modTime.Before(h[j].modTime) // older = more evictable
	}
	return h[i].seq > h[j].seq // same modTime: later line = more evictable
}
func (h boundedMatchHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *boundedMatchHeap) Push(x any)   { *h = append(*h, x.(grepMatch)) }
func (h *boundedMatchHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// evictFirst reports whether a should be evicted before b (a is "less" in heap
// order — more evictable). Used to compare a candidate against the heap root.
func evictFirst(a, b grepMatch) bool {
	if !a.modTime.Equal(b.modTime) {
		return a.modTime.Before(b.modTime)
	}
	return a.seq > b.seq
}

type GrepResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	Truncated       bool `json:"truncated"`
}

const (
	GrepToolName        = "grep"
	maxGrepContentWidth = 500
)

//go:embed grep.md.tpl
var grepDescriptionTmpl []byte

var grepDescriptionTpl = template.Must(
	template.New("grepDescription").
		Parse(string(grepDescriptionTmpl)),
)

type grepDescriptionData struct {
	MaxResults int
}

func grepDescription() string {
	return renderTemplate(grepDescriptionTpl, grepDescriptionData{
		MaxResults: 100,
	})
}

// escapeRegexPattern escapes special regex characters so they're treated as literal characters
func escapeRegexPattern(pattern string) string {
	specialChars := []string{"\\", ".", "+", "*", "?", "(", ")", "[", "]", "{", "}", "^", "$", "|"}
	escaped := pattern

	for _, char := range specialChars {
		escaped = strings.ReplaceAll(escaped, char, "\\"+char)
	}

	return escaped
}

func NewGrepTool(workingDir string, config config.ToolGrep) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GrepToolName,
		grepDescription(),
		func(ctx context.Context, params GrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			searchPattern := params.Pattern
			if params.LiteralText {
				searchPattern = escapeRegexPattern(params.Pattern)
			}

			searchPath := cmp.Or(params.Path, workingDir)

			searchCtx, cancel := context.WithTimeout(ctx, config.GetTimeout())
			defer cancel()

			matches, truncated, err := searchFiles(searchCtx, searchPattern, searchPath, params.Include, 100)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
			}

			var output strings.Builder
			if len(matches) == 0 {
				output.WriteString("No files found")
			} else {
				fmt.Fprintf(&output, "Found %d matches\n", len(matches))

				currentFile := ""
				for _, match := range matches {
					if currentFile != match.path {
						if currentFile != "" {
							output.WriteString("\n")
						}
						currentFile = match.path
						fmt.Fprintf(&output, "%s:\n", filepath.ToSlash(match.path))
					}
					if match.lineNum > 0 {
						lineText := match.lineText
						if len(lineText) > maxGrepContentWidth {
							lineText = stringext.Truncate(lineText, maxGrepContentWidth) + "..."
						}
						if match.charNum > 0 {
							fmt.Fprintf(&output, "  Line %d, Char %d: %s\n", match.lineNum, match.charNum, lineText)
						} else {
							fmt.Fprintf(&output, "  Line %d: %s\n", match.lineNum, lineText)
						}
					} else {
						fmt.Fprintf(&output, "  %s\n", match.path)
					}
				}

				if truncated {
					output.WriteString("\n(Results are truncated. Consider using a more specific path or pattern.)")
				}
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output.String()),
				GrepResponseMetadata{
					NumberOfMatches: len(matches),
					Truncated:       truncated,
				},
			), nil
		},
	)
}

func searchFiles(ctx context.Context, pattern, rootPath, include string, limit int) ([]grepMatch, bool, error) {
	matches, truncated, err := searchWithRipgrep(ctx, pattern, rootPath, include, limit)
	if err != nil {
		matches, err = searchFilesWithRegex(pattern, rootPath, include)
		if err != nil {
			return nil, false, err
		}
		// Regex fallback path: sort + truncate as before.
		sort.SliceStable(matches, func(i, j int) bool {
			return matches[i].modTime.After(matches[j].modTime)
		})
		truncated = len(matches) > limit
		if truncated {
			matches = matches[:limit]
		}
	}
	return matches, truncated, nil
}

func searchWithRipgrep(ctx context.Context, pattern, path, include string, limit int) ([]grepMatch, bool, error) {
	cmd := getRgSearchCmd(ctx, pattern, path, include)
	if cmd == nil {
		return nil, false, fmt.Errorf("ripgrep not found in $PATH")
	}

	// Only add ignore files if they exist.
	for _, ignoreFile := range []string{".gitignore", ".crushignore"} {
		ignorePath := filepath.Join(path, ignoreFile)
		if _, err := os.Stat(ignorePath); err == nil {
			cmd.Args = append(cmd.Args, "--ignore-file", ignorePath)
		}
	}

	// Stream rg's stdout line-by-line instead of buffering the entire output.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	// statCache ensures one os.Stat call per unique file path, not one per
	// submatch. A file with N matches previously triggered N Stat syscalls;
	// now it triggers exactly one.
	statCache := make(map[string]os.FileInfo)
	h := &boundedMatchHeap{}
	var seq int64

	scanner := bufio.NewScanner(stdout)
	// Allow long lines (minified JS etc.) — up to 4 MiB per JSON line.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var match ripgrepMatch
		if err := json.Unmarshal(line, &match); err != nil {
			continue
		}
		if match.Type != "match" || len(match.Data.Submatches) == 0 {
			continue
		}
		// Only take the first submatch per line (matches original behaviour).
		sub := match.Data.Submatches[0]

		fi, ok := statCache[match.Data.Path.Text]
		if !ok {
			fi, err = os.Stat(match.Data.Path.Text)
			if err != nil {
				continue // Skip files we can't access.
			}
			statCache[match.Data.Path.Text] = fi
		}

		seq++
		gm := grepMatch{
			path:     match.Data.Path.Text,
			modTime:  fi.ModTime(),
			lineNum:  match.Data.LineNumber,
			charNum:  sub.Start + 1, // ensure 1-based
			lineText: strings.TrimSpace(match.Data.Lines.Text),
			seq:      seq,
		}

		if h.Len() < limit {
			heap.Push(h, gm)
		} else if !evictFirst(gm, (*h)[0]) {
			// gm is less evictable than the root — it deserves a spot.
			(*h)[0] = gm
			heap.Fix(h, 0)
		}
	}

	// If the scan loop stopped early due to a scanner error (most notably
	// bufio.ErrTooLong: a single JSON line — e.g. one match inside a
	// pathologically long line, a minified bundle, a base64 blob — exceeding
	// the 4 MiB buffer above) rather than reaching the pipe's natural EOF,
	// rg may still be mid-write with more output queued. Per os/exec's
	// documented contract, Wait must not be called until all reads from the
	// pipe have completed; skipping this drain lets rg block forever on a
	// full OS pipe buffer once nobody is reading it, and Wait then hangs
	// forever waiting for a process that will never exit on its own.
	// Confirmed by reproduction: a single ~6 MiB matched line reliably hung
	// Wait() indefinitely without this drain.
	if scanErr := scanner.Err(); scanErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}

	// Wait for rg to finish and check exit code (1 = no matches, not an error).
	if waitErr := cmd.Wait(); waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// rg exit code 1 = no matches found.
		} else {
			return nil, false, waitErr
		}
	}

	// Extract and sort the bounded heap: newest modTime first, ties by seq asc
	// (preserves original line order within the same file/modTime).
	matches := []grepMatch(*h)
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].modTime.After(matches[j].modTime)
		}
		return matches[i].seq < matches[j].seq
	})

	return matches, seq > int64(limit), nil
}

type ripgrepMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

func searchFilesWithRegex(pattern, rootPath, include string) ([]grepMatch, error) {
	matches := []grepMatch{}

	// Use cached regex compilation
	regex, err := searchRegexCache.get(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	var includePattern *regexp.Regexp
	if include != "" {
		regexPattern := globToRegex(include)
		includePattern, err = globRegexCache.get(regexPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid include pattern: %w", err)
		}
	}

	// Create walker with gitignore and crushignore support
	walker := fsext.NewFastGlobWalker(rootPath)

	err = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		if info.IsDir() {
			// Check if directory should be skipped
			if walker.ShouldSkip(path) {
				return filepath.SkipDir
			}
			return nil // Continue into directory
		}

		// Use walker's shouldSkip method for files
		if walker.ShouldSkip(path) {
			return nil
		}

		// Skip hidden files (starting with a dot) to match ripgrep's default behavior
		base := filepath.Base(path)
		if base != "." && strings.HasPrefix(base, ".") {
			return nil
		}

		if includePattern != nil && !includePattern.MatchString(path) {
			return nil
		}

		stopWalk := false
		walkErr := fileMatches(path, regex, func(lm lineMatch) bool {
			matches = append(matches, grepMatch{
				path:     path,
				modTime:  info.ModTime(),
				lineNum:  lm.lineNum,
				charNum:  lm.charNum,
				lineText: lm.lineText,
			})
			if len(matches) >= 200 {
				stopWalk = true
				return false
			}
			return true
		})
		if walkErr != nil {
			return nil // Skip files we can't read.
		}
		if stopWalk {
			return filepath.SkipAll
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return matches, nil
}

// lineMatch is a single matching line within a file: its 1-based line
// number, the 1-based column of the first match on that line, and the
// line text (with the trailing newline stripped).
type lineMatch struct {
	lineNum  int
	charNum  int
	lineText string
}

// fileMatches calls onMatch for every line in filePath that matches pattern.
// Like ripgrep, it reports one entry per matching line (using the first match
// on the line for the column) instead of stopping at the first match in the
// file. If onMatch returns false, scanning stops immediately — the caller can
// bail out as soon as it has enough matches without reading the entire file.
func fileMatches(filePath string, pattern *regexp.Regexp, onMatch func(lineMatch) bool) error {
	if pattern == nil {
		return nil
	}
	// Only search text files.
	if !isTextFile(filePath) {
		return nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNum := 0
	for {
		line, err := reader.ReadString('\n')
		lineNum++
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if loc := pattern.FindStringIndex(line); loc != nil {
			if !onMatch(lineMatch{
				lineNum:  lineNum,
				charNum:  loc[0] + 1,
				lineText: line,
			}) {
				return nil
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	return nil
}

// isTextFile checks if a file is a text file by examining its MIME type.
func isTextFile(filePath string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer file.Close()

	// Read first 512 bytes for MIME type detection.
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return false
	}

	// Detect content type.
	contentType := http.DetectContentType(buffer[:n])

	// Check if it's a text MIME type.
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml" ||
		contentType == "application/javascript" ||
		contentType == "application/x-sh"
}

func globToRegex(glob string) string {
	regexPattern := strings.ReplaceAll(glob, ".", "\\.")
	regexPattern = strings.ReplaceAll(regexPattern, "*", ".*")
	regexPattern = strings.ReplaceAll(regexPattern, "?", ".")

	// Use pre-compiled regex instead of compiling each time
	regexPattern = globBraceRegex.ReplaceAllStringFunc(regexPattern, func(match string) string {
		inner := match[1 : len(match)-1]
		return "(" + strings.ReplaceAll(inner, ",", "|") + ")"
	})

	return regexPattern
}
