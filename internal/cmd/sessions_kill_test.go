package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadPIDFromLock(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		wantPID int
		wantErr bool
	}{
		{"plain pid", "12345", 12345, false},
		{"pid with newline", "12345\n", 12345, false},
		{"pid with crlf", "12345\r\n", 12345, false},
		{"pid with surrounding whitespace", "  12345  ", 12345, false},
		{"empty file", "", 0, true},
		{"whitespace only", "   \n", 0, true},
		{"non-numeric", "not-a-pid", 0, true},
		{"pid plus garbage", "12345 extra", 0, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".lock")
			require.NoError(t, os.WriteFile(path, []byte(c.content), 0o644))
			pid, err := readPIDFromLock(path)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, c.wantPID, pid)
			}
		})
	}
}

func TestReadPIDFromLock_FileMissing(t *testing.T) {
	pid, err := readPIDFromLock(filepath.Join(t.TempDir(), "nope.lock"))
	assert.Error(t, err)
	assert.Zero(t, pid)
}

func TestSanitiseSessionIDForFilename(t *testing.T) {
	cases := map[string]string{
		"simple-id":      "simple-id",
		"with/slash":     "with_slash",
		"with\\backslash": "with_backslash",
		"with space":     "with_space",
		"a:b*c?d\"e<f>g|h": "a_b_c_d_e_f_g_h",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitiseSessionIDForFilename(in), "input=%q", in)
	}
}
