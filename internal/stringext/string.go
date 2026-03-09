package stringext

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

func Capitalize(text string) string {
	return cases.Title(language.English, cases.Compact).String(text)
}

// NormalizeSpace normalizes whitespace in the given content string.
// It replaces Windows-style line endings with Unix-style line endings,
// converts tabs to four spaces, and trims leading and trailing whitespace.
func NormalizeSpace(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\t", "    ")
	content = strings.TrimSpace(content)
	return content
}

// Truncate truncates a string to maxLen bytes while respecting UTF-8 boundaries.
// If the string is longer than maxLen bytes, it returns the first maxLen bytes
// that form valid UTF-8, ensuring no multi-byte character is split.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	for maxLen > 0 {
		if utf8.ValidString(s[:maxLen]) {
			return s[:maxLen]
		}
		maxLen--
	}
	return ""
}

// TruncateEnd truncates a string to maxLen bytes from the end while respecting UTF-8 boundaries.
func TruncateEnd(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	start := len(s) - maxLen
	for start < len(s) {
		if utf8.ValidString(s[start:]) {
			return s[start:]
		}
		start++
	}
	return s
}

// TruncateAt truncates a string to maxLen bytes while respecting UTF-8 boundaries.
// It returns the truncated string and the actual byte position where truncation occurred.
func TruncateAt(s string, maxLen int) (truncated string, pos int) {
	if len(s) <= maxLen {
		return s, len(s)
	}
	for maxLen > 0 {
		if utf8.ValidString(s[:maxLen]) {
			return s[:maxLen], maxLen
		}
		maxLen--
	}
	return "", 0
}

// TruncateEndAt truncates a string to maxLen bytes from the end while respecting UTF-8 boundaries.
// It returns the truncated string and the actual byte position where the end portion starts.
func TruncateEndAt(s string, maxLen int) (truncated string, startPos int) {
	if len(s) <= maxLen {
		return s, 0
	}
	start := len(s) - maxLen
	for start < len(s) {
		if utf8.ValidString(s[start:]) {
			return s[start:], start
		}
		start++
	}
	return s, 0
}
