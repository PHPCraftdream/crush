package platform

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAssignToNewJobObject(t *testing.T) {
	err := AssignToNewJobObject()
	require.NoError(t, err, "AssignToNewJobObject should succeed on first call")
}

func TestAssignToNewJobObjectIdempotent(t *testing.T) {
	err := AssignToNewJobObject()
	require.NoError(t, err)
	// Second call should also succeed (or gracefully handle nested job).
	err = AssignToNewJobObject()
	require.NoError(t, err, "AssignToNewJobObject should handle repeated calls")
}
